package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/labstack/onebox/internal/app"
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
	prefix := "ob-" + e.Spec.Name + "-"

	// What is installed now, so anything no longer declared can go.
	res, err := e.T.Run(ctx, "systemctl list-unit-files --no-legend --type=timer 2>/dev/null | awk '{print $1}'")
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		unit := strings.TrimSpace(line)
		if strings.HasPrefix(unit, prefix) && strings.HasSuffix(unit, ".timer") {
			installed[strings.TrimSuffix(unit, ".timer")] = true
		}
	}

	wanted := map[string]bool{}
	for _, job := range jobs {
		unit := prefix + job.Name
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

		service := scheduleServiceUnit(e.Spec.Name, job.Name, n.CurrentLink())
		timer := scheduleTimerUnit(e.Spec.Name, job)
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
	for _, unit := range sortedNames(setOf(stale)) {
		if _, err := e.mutate(ctx, fmt.Sprintf(
			"systemctl disable --now %s.timer >/dev/null 2>&1; rm -f /etc/systemd/system/%s.timer /etc/systemd/system/%s.service",
			unit, unit, unit)); err != nil {
			return err
		}
		e.logf("schedule: removed %s (no longer declared)", unit)
	}

	if len(jobs) == 0 && len(stale) == 0 {
		return nil
	}
	if res, err := e.mutate(ctx, "systemctl daemon-reload"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(res.Stderr))
	}
	for _, job := range jobs {
		unit := prefix + job.Name + ".timer"
		if res, err := e.mutate(ctx, "systemctl enable --now "+unit); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("job %s: cannot start its timer: %s", job.Name, strings.TrimSpace(res.Stderr))
		}
		e.logf("schedule: %s at %s (%s)", job.Name, job.Cron, job.Timezone)
	}
	return nil
}

// scheduleServiceUnit runs one job exactly as a release-phase job runs, through
// the current release's runtime.
func scheduleServiceUnit(application, job, currentLink string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Onebox scheduled job " + job + " for " + application,
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"After=docker.service",
		"Requires=docker.service",
		"",
		"[Service]",
		"Type=oneshot",
		// --rm because a scheduled job leaves no container behind, and
		// --no-deps because its prerequisites are already running; starting
		// them here would duplicate the application beside itself.
		fmt.Sprintf("ExecStart=/usr/bin/docker compose -p %s -f %s/compose.yaml run --rm --no-deps %s",
			application, currentLink, job),
		"",
	}, "\n")
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
		"Persistent=true",
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
	prefix := "ob-" + e.Spec.Name + "-"
	res, err := e.T.Run(ctx, "systemctl list-unit-files --no-legend --type=timer 2>/dev/null | awk '{print $1}'")
	if err != nil {
		return err
	}
	var units []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		unit := strings.TrimSpace(line)
		if strings.HasPrefix(unit, prefix) && strings.HasSuffix(unit, ".timer") && unitName.MatchString(unit) {
			units = append(units, strings.TrimSuffix(unit, ".timer"))
		}
	}
	if len(units) == 0 {
		return nil
	}
	for _, unit := range sortedNames(setOf(units)) {
		if _, err := e.mutate(ctx, fmt.Sprintf(
			"systemctl disable --now %s.timer >/dev/null 2>&1; rm -f /etc/systemd/system/%s.timer /etc/systemd/system/%s.service",
			unit, unit, unit)); err != nil {
			return err
		}
		e.logf("schedule: removed %s", unit)
	}
	if res, err := e.mutate(ctx, "systemctl daemon-reload"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}
