package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/notify"
)

// A scheduled job runs when nobody is watching, so it runs on the host's own
// scheduler rather than on anything Onebox keeps alive. A systemd timer
// survives a reboot, records its last run, and is inspectable with `systemctl
// list-timers` by anyone with a shell — none of which is true of a scheduler
// process that has to stay running, or of a container whose job is to start
// other containers.
//
// The unit invokes the job through the `current` symlink rather than through
// the release that installed it. A scheduled job should run the code that is
// live, not the code that happened to be live when the timer was written, and
// a rollback must move the job back with everything else.

// A unit name reaches a shell as an argument. It is derived from names this
// contract already bounds, and checked again here because the check is cheap
// and the consequence of it being wrong is a root shell.
var unitName = regexp.MustCompile(`^[a-zA-Z0-9@:_.-]+$`)

// SyncSchedules installs a timer for every scheduled job and removes the timers
// of jobs that are no longer scheduled.
//
// Removal matters as much as installation: a job deleted from the project whose
// timer stayed behind would keep running against the current release forever,
// and nothing in the project would explain why.
func (e *Engine) SyncSchedules(ctx context.Context) error {
	jobs, err := e.Spec.ScheduledJobs()
	if err != nil {
		return err
	}
	n := e.names()
	prefixes := n.ScheduledJobUnitPrefixes()
	prefix := prefixes[0]

	// What is installed now, so anything no longer declared can go.
	res, err := e.T.Run(ctx, "systemctl list-unit-files --no-legend --type=timer 2>/dev/null | awk '{print $1}'")
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		unit := strings.TrimSpace(line)
		// Backups own their own namespace and reconciles it separately. Its
		// units begin "ob-backup-", which also begins with this prefix when
		// the application is literally named "backup" — belt and braces,
		// because the failure mode is a deploy silently deleting every
		// scheduled backup.
		if strings.HasPrefix(unit, app.BackupUnitPrefix) {
			continue
		}
		if !strings.HasSuffix(unit, ".timer") || !unitName.MatchString(unit) {
			continue
		}
		bare := strings.TrimSuffix(unit, ".timer")
		if matchesRuntimePrefix(bare, prefix) {
			installed[bare] = true
			continue
		}
		if matchesAnyPrefix(unit, prefixes[1:]) {
			owned, err := e.scheduleUnitBelongsToOwner(ctx, bare, false)
			if err != nil {
				return err
			}
			if owned {
				installed[bare] = true
			}
		}
	}

	wanted := map[string]bool{}
	if len(jobs) > 0 && !e.hasFlock(ctx) {
		return errors.New("scheduled jobs require flock on the target so they cannot overlap deployments; install util-linux and deploy again")
	}
	for _, job := range jobs {
		unit := n.ScheduledJobUnit(job.Name)
		wanted[unit] = true

		// Validated by the host before anything is installed. The translation
		// is exact by construction, and this is the check that it stayed exact
		// against the systemd the target actually runs.
		expr := calendarExpr(job)
		check, err := e.T.Run(ctx, "systemd-analyze calendar "+q(expr)+" >/dev/null 2>&1 && echo ok")
		if err != nil {
			return err
		}
		if strings.TrimSpace(check.Stdout) != "ok" {
			return fmt.Errorf("job %s: the host rejected the calendar expression %q derived from cron %q in %s. "+
				"A timezone in OnCalendar needs systemd 252 or newer; on an older host, declare the schedule in UTC",
				job.Name, expr, job.Cron, job.Timezone)
		}

		runnerPath := "/etc/systemd/system/" + unit + ".run"
		notifyPath := "/etc/systemd/system/" + unit + ".notify"
		var runtimeEnvFiles []app.EnvFile
		if e.Spec.Runtime != nil {
			runtimeEnvFiles = e.Spec.Runtime.EnvFiles
		}
		runner := scheduleRunnerScript(e.Spec.Name, job, n, e.lockPath(), runtimeEnvFiles)
		notifier, err := e.scheduleFailureNotifier(job.Name)
		if err != nil {
			return fmt.Errorf("job %s: cannot render its failure notifier: %w", job.Name, err)
		}
		service := scheduleServiceUnit(e.Spec.Name, job, runnerPath, notifyPath)
		timer := scheduleTimerUnit(e.Spec.Name, job)
		if err := e.writeServiceFile(ctx, runnerPath, []byte(runner)); err != nil {
			return fmt.Errorf("job %s: cannot install its runner: %w", job.Name, err)
		}
		if err := e.writeServiceFile(ctx, notifyPath, []byte(notifier)); err != nil {
			return fmt.Errorf("job %s: cannot install its failure notifier: %w", job.Name, err)
		}
		if err := e.writeServiceFile(ctx, "/etc/systemd/system/"+unit+".service", []byte(service)); err != nil {
			return fmt.Errorf("job %s: cannot install its unit: %w", job.Name, err)
		}
		if err := e.writeServiceFile(ctx, "/etc/systemd/system/"+unit+".timer", []byte(timer)); err != nil {
			return fmt.Errorf("job %s: cannot install its timer: %w", job.Name, err)
		}
	}

	var stale []string
	for unit := range installed {
		if !wanted[unit] {
			stale = append(stale, unit)
		}
	}
	var removalErr error
	for _, unit := range sortedNames(setOf(stale)) {
		if err := e.removeScheduleUnit(ctx, unit); err != nil {
			removalErr = errors.Join(removalErr, err)
			continue
		}
		e.logf("schedule: removed %s (no longer declared)", unit)
	}

	if len(jobs) == 0 && len(stale) == 0 {
		return nil
	}
	if res, err := e.mutate(ctx, "systemctl daemon-reload"); err != nil {
		return errors.Join(removalErr, err)
	} else if res.ExitCode != 0 {
		return errors.Join(removalErr, fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(res.Stderr)))
	}
	if removalErr != nil {
		return removalErr
	}
	for _, job := range jobs {
		unit := n.ScheduledJobUnit(job.Name) + ".timer"
		if res, err := e.mutate(ctx, "systemctl enable --now "+unit); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("job %s: cannot start its timer: %s", job.Name, strings.TrimSpace(res.Stderr))
		}
		e.logf("schedule: %s at %s (%s)", job.Name, job.Cron, job.Timezone)
	}
	return nil
}

// scheduleRunnerScript keeps the existing whole-run deploy exclusion unless a
// job explicitly opts into the narrower pinned-release contract. Pinned mode
// meets the deploy acquirer briefly under schedule.lock, leases the resolved
// release before releasing that rendezvous, then retains only its own job lock.
func scheduleRunnerScript(application string, job app.ScheduledJob, names app.Names, applicationLock string, runtimeEnvFiles []app.EnvFile) string {
	if job.DeployLock == "pinned" {
		return pinnedScheduleRunnerScript(application, job.Name, names, applicationLock, runtimeEnvFiles)
	}
	container := names.Container(job.Name, 1)
	projectDir := q(names.CurrentLink())
	compose := "/usr/bin/docker compose -p " + q(application) + " --project-directory " + projectDir +
		" -f " + projectDir + "/" + q("compose.yaml") + scheduleRuntimeEnvArgs(projectDir, runtimeEnvFiles) +
		" run --rm --no-deps --name " + q(container) + " " + q(job.Name)
	return strings.Join([]string{
		"#!/bin/sh",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"set -eu",
		"install -d -m 700 " + q(names.AppDir()+"/schedule"),
		"exec 9>" + q(names.ScheduledJobRunLock(job.Name)),
		"/usr/bin/flock --exclusive --nonblock --conflict-exit-code 75 9",
		"exec 8>" + q(names.ScheduleRunLock()),
		"/usr/bin/flock --exclusive --nonblock --conflict-exit-code 75 8",
		"if [ -e " + q(applicationLock) + " ]; then echo 'onebox: an application operation holds the deploy lock' >&2; exit 75; fi",
		scheduleContainerCleanup(container),
		"cleanup() { " + scheduleContainerCleanup(container) + "; }",
		"trap cleanup 0",
		"trap 'exit 129' 1",
		"trap 'exit 130' 2",
		"trap 'exit 143' 15",
		compose,
		"",
	}, "\n")
}

func pinnedScheduleRunnerScript(application, job string, names app.Names, applicationLock string, runtimeEnvFiles []app.EnvFile) string {
	scheduleDir := names.AppDir() + "/schedule"
	state := names.ScheduledJobRunState(job)
	container := names.Container(job, 1)
	projectDir := `"$release_dir"`
	compose := "/usr/bin/docker compose -p " + q(application) + " --project-directory " + projectDir +
		" -f " + projectDir + "/" + q("compose.yaml") + scheduleRuntimeEnvArgs(projectDir, runtimeEnvFiles) +
		" run --rm --no-deps --name " + q(container) + " " + q(job)
	lines := []string{
		"#!/bin/sh",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"set -eu",
		"install -d -m 700 " + q(scheduleDir),
		"exec 9>" + q(names.ScheduledJobRunLock(job)),
		"/usr/bin/flock --exclusive --nonblock --conflict-exit-code 75 9",
		"exec 8>" + q(names.ScheduleRunLock()),
		"/usr/bin/flock --exclusive --nonblock --conflict-exit-code 75 8",
		"if [ -e " + q(applicationLock) + " ]; then echo 'onebox: an application operation holds the deploy lock' >&2; exit 75; fi",
		"release_dir=$(readlink -f " + q(names.CurrentLink()) + ") || { echo 'onebox: current release cannot be resolved' >&2; exit 75; }",
		"if [ \"${release_dir%/*}\" != " + q(names.ReleasesDir()) + " ]; then echo 'onebox: current release resolves outside the release store' >&2; exit 75; fi",
		"release=${release_dir##*/}",
		"if ! printf '%s\\n' \"$release\" | grep -Eq '^[0-9]{8}-[0-9]{6}-[0-9A-Za-z_-]+$'; then echo 'onebox: current release identity is invalid' >&2; exit 75; fi",
		"if [ ! -f \"$release_dir/compose.yaml\" ]; then echo 'onebox: pinned release has no compose.yaml' >&2; exit 75; fi",
		"exec 7>>\"$release_dir/.ob-schedule.lease\"",
		"chmod 600 \"$release_dir/.ob-schedule.lease\"",
		"/usr/bin/flock --shared 7",
		scheduleContainerCleanup(container),
		"state=" + q(state),
		"tmp=\"$state.$$\"",
		"cleanup() { " + scheduleContainerCleanup(container) + "; rm -f \"$state\" \"$tmp\"; }",
		"trap cleanup 0",
		"trap 'exit 129' 1",
		"trap 'exit 130' 2",
		"trap 'exit 143' 15",
		"started_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')",
		"umask 077",
		"printf 'release=%s\\nstarted_at=%s\\n' \"$release\" \"$started_at\" >\"$tmp\"",
		"mv -f \"$tmp\" \"$state\"",
		"/usr/bin/flock --unlock 8",
		compose,
		"",
	}
	return strings.Join(lines, "\n")
}

func scheduleContainerCleanup(container string) string {
	return "/usr/bin/docker rm -f " + q(container) + " >/dev/null 2>&1 || true"
}

func scheduleRuntimeEnvArgs(projectDir string, entries []app.EnvFile) string {
	args := ""
	for _, entry := range entries {
		if entry.Encrypted() {
			continue
		}
		args += " --env-file " + projectDir + "/" + q(entry.StagedPath())
	}
	return args
}

func scheduleServiceUnit(application string, job app.ScheduledJob, runnerPath, notifyPath string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Onebox scheduled job " + job.Name + " for " + application,
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"After=docker.service",
		"Requires=docker.service",
		"",
		"[Service]",
		"Type=oneshot",
		"ExecStart=/bin/sh " + runnerPath,
		// ExecStopPost runs after success, start failures, and timeouts. It always
		// attempts fenced container cleanup, then uses SERVICE_RESULT to decide
		// whether failure notifications are needed.
		"ExecStopPost=/bin/sh " + notifyPath,
		"TimeoutStartSec=" + job.Timeout,
		"",
	}, "\n")
}

const scheduleNotificationTimestamp = "__ONEBOX_SCHEDULE_TIMESTAMP__"

// scheduleFailureNotifier extends the existing notification contract to work
// fired directly by systemd. The generated file is mode 0600, keeping webhook
// tokens out of unit metadata, and every send is bounded and fail-open.
func (e *Engine) scheduleFailureNotifier(job string) (string, error) {
	environment := e.Opts.Environment
	if environment == "" {
		environment = e.Spec.Env
	}
	lines := []string{
		"#!/bin/sh",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"set -u",
		"exec 9>" + q(e.names().ScheduledJobRunLock(job)),
		"if /usr/bin/flock --exclusive --nonblock 9; then",
		"  " + scheduleContainerCleanup(e.names().Container(job, 1)),
		"fi",
		`[ "${SERVICE_RESULT:-success}" = success ] && exit 0`,
	}
	var sends []string
	for _, name := range sortedNames(e.Spec.Notifications) {
		cfg := e.Spec.Notifications[name]
		prepared, err := notify.Prepare(cfg, notify.Payload{
			App: e.Spec.Name, Env: environment, Host: e.T.Destination(),
			Verb: "scheduled job " + job, Status: "fail",
			Error: "scheduled job failed; inspect trusted host diagnostics",
			TS:    scheduleNotificationTimestamp,
		})
		if err != nil {
			return "", err
		}
		if prepared == nil {
			continue
		}
		body := q(string(prepared.Body))
		if before, after, ok := strings.Cut(string(prepared.Body), scheduleNotificationTimestamp); ok {
			body = q(before) + `"$ts"` + q(after)
		}
		curl := "curl --fail --silent --show-error --max-time 5 --request POST" +
			" --header " + q("Content-Type: "+prepared.ContentType) +
			" --header " + q("X-Title: "+prepared.Title) +
			` --data-binary "$body" ` + q(cfg.Webhook)
		sends = append(sends, "(body="+body+"; if ! "+curl+"; then echo "+
			q("onebox: notification "+name+" failed")+" >&2; fi) &")
	}
	if len(sends) > 0 {
		lines = append(lines, `ts=$(date -u '+%Y-%m-%dT%H:%M:%SZ')`)
		lines = append(lines, sends...)
		lines = append(lines, "wait || true")
	}
	lines = append(lines, "exit 0", "")
	return strings.Join(lines, "\n"), nil
}

// calendarExpr is the one string both the host's validator and the installed
// unit see, so the expression that was checked is the expression that runs.
func calendarExpr(job app.ScheduledJob) string {
	if job.Timezone == "" {
		return job.Calendar
	}
	return job.Calendar + " " + job.Timezone
}

func scheduleTimerUnit(application string, job app.ScheduledJob) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Onebox schedule for " + job.Name + " (" + application + ")",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"",
		"[Timer]",
		// The timezone belongs in the expression. `Timezone=` is not a [Timer]
		// directive: systemd ignores it silently and evaluates the calendar in
		// the host's zone, so a job declared for 02:00 Europe/Berlin runs at
		// 02:00 UTC and nothing anywhere says so.
		"OnCalendar=" + calendarExpr(job),
		// A box that was off at 2am still runs the job when it comes back,
		// which is the behaviour anyone declaring a nightly job expects.
		fmt.Sprintf("Persistent=%t", job.CatchUp),
		"",
		"[Install]",
		"WantedBy=timers.target",
		"",
	}, "\n")
}

func setOf(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

// RemoveSchedules takes down every timer this app installed.
//
// SyncSchedules only removes what the project no longer declares, which is the
// right rule while the app exists and the wrong one once it does not: a
// destroyed app's timer keeps firing against a release directory that has been
// deleted, failing every minute forever and explaining itself to nobody.
func (e *Engine) RemoveSchedules(ctx context.Context) error {
	// Both namespaces this application installs into.
	//
	// Backup timers are deliberately named outside the job scheduler's
	// namespace — app.BackupTimerForEnvironment explains why: a deploy used to
	// treat them as "no longer declared" and delete every scheduled backup.
	// Teardown is the opposite case and needs both, and matching only the job
	// prefix meant `ob destroy` left ob-backup-<app>-<env>-<service>-<op>
	// timers loaded and firing against a release directory it had just
	// deleted. They belong to this application and they go with it.
	n := e.names()
	jobPrefixes := n.ScheduledJobUnitPrefixes()
	backupPrefixes := n.BackupUnitPrefixes()
	res, err := e.T.Run(ctx, "systemctl list-unit-files --no-legend --type=timer 2>/dev/null | awk '{print $1}'")
	if err != nil {
		return err
	}
	var units []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		unit := strings.TrimSpace(line)
		if !strings.HasSuffix(unit, ".timer") || !unitName.MatchString(unit) {
			continue
		}
		unit = strings.TrimSuffix(unit, ".timer")
		var owned bool
		if strings.HasPrefix(unit, app.BackupUnitPrefix) {
			switch {
			case matchesRuntimePrefix(unit, backupPrefixes[0]):
				owned = true
			case matchesAnyPrefix(unit, backupPrefixes[1:]):
				owned, err = e.scheduleUnitBelongsToOwner(ctx, unit, true)
			}
		} else {
			switch {
			case matchesRuntimePrefix(unit, jobPrefixes[0]):
				owned = true
			case matchesAnyPrefix(unit, jobPrefixes[1:]):
				owned, err = e.scheduleUnitBelongsToOwner(ctx, unit, false)
			}
		}
		if err != nil {
			return err
		}
		if owned {
			units = append(units, unit)
		}
	}
	if len(units) == 0 {
		return nil
	}
	var removalErr error
	for _, unit := range sortedNames(setOf(units)) {
		if err := e.removeScheduleUnit(ctx, unit); err != nil {
			removalErr = errors.Join(removalErr, err)
			continue
		}
		e.logf("schedule: removed %s", unit)
	}
	if res, err := e.mutate(ctx, "systemctl daemon-reload"); err != nil {
		return errors.Join(removalErr, err)
	} else if res.ExitCode != 0 {
		return errors.Join(removalErr, fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(res.Stderr)))
	}
	return removalErr
}

// removeScheduleUnit keeps unit-file cleanup independent from systemd's
// ability to disable the timer. Both operations are attempted, and callers
// reload systemd before returning, so a failed disable cannot strand files that make
// the next reconciliation see the same stale schedule again.
func (e *Engine) removeScheduleUnit(ctx context.Context, unit string) error {
	disable, disableErr := e.mutate(ctx, "systemctl disable --now "+unit+".timer >/dev/null")
	remove, removeErr := e.mutate(ctx, fmt.Sprintf(
		"rm -f /etc/systemd/system/%s.timer /etc/systemd/system/%s.service /etc/systemd/system/%s.run /etc/systemd/system/%s.notify", unit, unit, unit, unit))
	var errs []error
	if disableErr != nil {
		errs = append(errs, fmt.Errorf("disable schedule %s: %w", unit, disableErr))
	} else if disable.ExitCode != 0 {
		errs = append(errs, fmt.Errorf("disable schedule %s failed (exit %d): %s", unit, disable.ExitCode, strings.TrimSpace(disable.Stderr)))
	}
	if removeErr != nil {
		errs = append(errs, fmt.Errorf("remove schedule files %s: %w", unit, removeErr))
	} else if remove.ExitCode != 0 {
		errs = append(errs, fmt.Errorf("remove schedule files %s failed (exit %d): %s", unit, remove.ExitCode, strings.TrimSpace(remove.Stderr)))
	}
	return errors.Join(errs...)
}

func matchesAnyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// matchesRuntimePrefix distinguishes a component boundary from the first half
// of an escaped hyphen. For example, ob-acme- owns ob-acme-nightly but not
// ob-acme--web-nightly, whose application component is acme-web.
func matchesRuntimePrefix(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) && len(name) > len(prefix) && name[len(prefix)] != '-'
}

// scheduleUnitBelongsToOwner resolves an ambiguous old unit name from the
// unambiguous owner embedded in its service body. New backup units include the
// environment as well; the application-only suffix remains migration input for
// units written before environments were recorded there.
// Missing or unfamiliar files are left alone: ownership must be proved before
// reconciliation removes a host-global unit.
func (e *Engine) scheduleUnitBelongsToOwner(ctx context.Context, unit string, backup bool) (bool, error) {
	res, err := e.T.Run(ctx, "cat "+q("/etc/systemd/system/"+unit+".service")+" 2>/dev/null")
	if err != nil {
		return false, fmt.Errorf("inspect legacy schedule %s: %w", unit, err)
	}
	if res.ExitCode != 0 {
		return false, nil
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if backup {
			if strings.HasPrefix(line, "Description=Onebox backup ") &&
				(strings.HasSuffix(line, " ("+e.Spec.Name+"/"+e.Opts.Environment+")") ||
					strings.HasSuffix(line, " ("+e.Spec.Name+")")) {
				return true, nil
			}
			continue
		}
		if strings.HasPrefix(line, "Description=Onebox scheduled job ") && strings.HasSuffix(line, " for "+e.Spec.Name) {
			return true, nil
		}
	}
	return false, nil
}
