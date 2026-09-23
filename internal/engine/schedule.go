package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/durable"
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
	prefix := app.JobUnitPrefix

	// What is installed now, so anything no longer declared can go.
	res, err := e.T.Run(ctx, "systemctl list-unit-files --no-legend --type=timer 2>/dev/null | awk '{print $1}'")
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		unit := strings.TrimSpace(line)
		if !strings.HasSuffix(unit, ".timer") || !unitName.MatchString(unit) {
			continue
		}
		bare := strings.TrimSuffix(unit, ".timer")
		if strings.HasPrefix(bare, prefix) {
			installed[bare] = true
		}
	}

	wanted := map[string]bool{}
	if err := e.requireScheduleHost(ctx, jobs); err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Execution != nil {
			res, err := e.mutate(ctx, "install -d -m 700 "+q(n.AppDir()+"/schedule"))
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return errors.New("cannot create durable execution helper directory")
			}
			if err := e.writeServiceFile(ctx, durable.Helper(n.AppDir()), []byte(durable.Script)); err != nil {
				return fmt.Errorf("install durable execution helper: %w", err)
			}
			break
		}
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
		runner := scheduleRunnerScript(e.Spec.Name, job, n, e.lockPath(), runtimeEnvFiles, e.lockTTL(), e.hasTriggerUnit(ctx))
		if job.Execution != nil {
			runner, err = e.durableScheduleRunner(job, runtimeEnvFiles)
			if err != nil {
				return err
			}
		}
		notifier, err := e.scheduleNotifier(job)
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

	// A marker is host state that outlives the units, so the schedule
	// directory is reconciled with the same list the units are.
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		names = append(names, job.Name)
	}
	if err := e.pruneSchedulePauses(ctx, names); err != nil {
		return errors.Join(removalErr, err)
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
	// A pause is the operator's decision about this host, and reconciliation
	// does not get to overrule it. The units above were still written, so a
	// fix lands while the job stays stopped; only the timer is left alone.
	paused, err := e.pausedJobs(ctx, names)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		unit := n.ScheduledJobUnit(job.Name) + ".timer"
		action, outcome := "enable --now", fmt.Sprintf("%s at %s (%s)", job.Name, job.Cron, job.Timezone)
		if state, ok := paused[job.Name]; ok {
			action = "disable --now"
			outcome = fmt.Sprintf("%s stays paused (%s)", job.Name, pauseSummary(state))
		}
		if res, err := e.mutate(ctx, "systemctl "+action+" "+unit); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("job %s: cannot %s its timer: %s", job.Name, action, strings.TrimSpace(res.Stderr))
		}
		e.logf("schedule: %s", outcome)
	}
	return nil
}

// pauseSummary is the one-line account of a pause that status, list and the
// deploy log all print.
func pauseSummary(state SchedulePauseState) string {
	parts := make([]string, 0, 3)
	if state.Operator != "" {
		parts = append(parts, "by "+state.Operator)
	}
	if state.PausedAt != "" {
		parts = append(parts, "since "+state.PausedAt)
	}
	if state.Reason != "" {
		parts = append(parts, state.Reason)
	}
	if len(parts) == 0 {
		return "no reason recorded"
	}
	return strings.Join(parts, "; ")
}

// scheduleRunnerScript keeps the existing whole-run deploy exclusion unless a
// job explicitly opts into the narrower pinned-release contract. Pinned mode
// meets the deploy acquirer briefly under schedule.lock, leases the resolved
// release before releasing that rendezvous, then retains only its own job lock.
func scheduleRunnerScript(application string, job app.ScheduledJob, names app.Names, applicationLock string, runtimeEnvFiles []app.EnvFile, lockTTL time.Duration, triggerUnit bool) string {
	if job.DeployLock == "pinned" {
		return pinnedScheduleRunnerScript(application, job, names, applicationLock, runtimeEnvFiles, lockTTL, triggerUnit)
	}
	container := names.Container(job.Name, 1)
	projectDir := q(names.CurrentLink())
	compose := "/usr/bin/docker compose -p " + q(application) + " --project-directory " + projectDir +
		" -f " + projectDir + "/" + q("compose.yaml") + scheduleRuntimeEnvArgs(projectDir, runtimeEnvFiles) +
		" run --rm --no-deps \"$@\" --label " + q(ExecutionJobLabel+"="+job.Name) + " --name " + q(container) + " " + q(job.Name)
	lines := []string{
		"#!/bin/sh",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"set -eu",
		"install -d -m 700 " + q(names.AppDir()+"/schedule"),
	}
	lines = append(lines, scheduleInputsLines(names.ScheduledJobRunInputs(job.Name))...)
	lines = append(lines, scheduleLockLines(names, job.Name, job.DeployLock, applicationLock, lockTTL, scheduleRendezvousWait(job.Timeout))...)
	lines = append(lines,
		// Best effort: the record names the release that ran, and an exclusive
		// job runs whatever `current` points at when it starts.
		"release_dir=$(readlink -f "+q(names.CurrentLink())+" 2>/dev/null || true)",
		"release=${release_dir##*/}",
		scheduleContainerRemove(container),
		"cleanup() { if [ -f \"$state\" ]; then printf 'phase=stopping\\n' >>\"$state\"; fi; "+scheduleContainerStop(container, job.ShutdownGrace)+"; rm -f \"$tmp\"; }",
		"trap cleanup 0",
		"trap 'exit 129' 1",
		"trap 'exit 130' 2",
		"trap 'exit 143' 15",
	)
	lines = append(lines, scheduleRunPreamble(triggerUnit)...)
	lines = append(lines, schedulePlannedBindingLines()...)
	if job.DataEffect != app.DataEffectNone {
		lines = append(lines, invalidateExecutionCommand(names.AppDir()))
	}
	lines = append(lines, scheduleAttemptLoop(job, compose, container)...)
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// ExecutionJobLabel names the job a one-off container runs. Every scheduled run
// carries it, durable or not, so a container a crash left behind is still
// provably this job's when the durable runner has to reclaim it.
const ExecutionJobLabel = "onebox.execution.job"

func pinnedScheduleRunnerScript(application string, job app.ScheduledJob, names app.Names, applicationLock string, runtimeEnvFiles []app.EnvFile, lockTTL time.Duration, triggerUnit bool) string {
	scheduleDir := names.AppDir() + "/schedule"
	container := names.Container(job.Name, 1)
	projectDir := `"$release_dir"`
	compose := "/usr/bin/docker compose -p " + q(application) + " --project-directory " + projectDir +
		" -f " + projectDir + "/" + q("compose.yaml") + scheduleRuntimeEnvArgs(projectDir, runtimeEnvFiles) +
		" run --rm --no-deps \"$@\" --label " + q(ExecutionJobLabel+"="+job.Name) + " --name " + q(container) + " " + q(job.Name)
	lines := []string{
		"#!/bin/sh",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"set -eu",
		"install -d -m 700 " + q(scheduleDir),
	}
	lines = append(lines, scheduleInputsLines(names.ScheduledJobRunInputs(job.Name))...)
	lines = append(lines, scheduleLockLines(names, job.Name, job.DeployLock, applicationLock, lockTTL, scheduleRendezvousWait(job.Timeout))...)
	lines = append(lines,
		// These are misconfigurations, not timing: the run fails, loudly.
		"release_dir=$(readlink -f "+q(names.CurrentLink())+") || { echo 'onebox: current release cannot be resolved' >&2; exit 1; }",
		"if [ \"${release_dir%/*}\" != "+q(names.ReleasesDir())+" ]; then echo 'onebox: current release resolves outside the release store' >&2; exit 1; fi",
		"release=${release_dir##*/}",
		"if ! printf '%s\\n' \"$release\" | grep -Eq '^[0-9]{8}-[0-9]{6}-[0-9A-Za-z_-]+$'; then echo 'onebox: current release identity is invalid' >&2; exit 1; fi",
		"if [ ! -f \"$release_dir/compose.yaml\" ]; then echo 'onebox: pinned release has no compose.yaml' >&2; exit 1; fi",
		"exec 7>>\"$release_dir/.onebox-schedule.lease\"",
		"chmod 600 \"$release_dir/.onebox-schedule.lease\"",
		"/usr/bin/flock --shared 7",
		// The immutable release is leased, so the writer rendezvous is complete.
		// Container cleanup and state bookkeeping are per-job work and must not
		// keep an application operation waiting behind them.
		"/usr/bin/flock --unlock 8",
		scheduleContainerRemove(container),
		"cleanup() { if [ -f \"$state\" ]; then printf 'phase=stopping\\n' >>\"$state\"; fi; "+scheduleContainerStop(container, job.ShutdownGrace)+"; rm -f \"$tmp\"; }",
		"trap cleanup 0",
		"trap 'exit 129' 1",
		"trap 'exit 130' 2",
		"trap 'exit 143' 15",
	)
	lines = append(lines, scheduleRunPreamble(triggerUnit)...)
	lines = append(lines, schedulePlannedBindingLines()...)
	lines = append(lines, scheduleAttemptLoop(job, compose, container)...)
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// scheduleLockLines take the run's locks, and turn a conflict into a recorded
// skip rather than a failed unit. A skip is a fact about timing: another run
// of this job, or an application operation, is in progress. The runner writes
// the reason into the state file and exits 0, so systemd sees a clean unit and
// the notifier records `skipped` with that reason. The container's own exit
// status is never mistaken for a skip, because a skip happens before any
// container starts.
//
// The application lock is honoured for as long as AcquireLock would honour it:
// a lock older than the TTL belongs to a runner that died, and AcquireLock
// takes it over, so the timer must not defer to it forever either. The age
// comes from the same shell AcquireLock reads it with, in whole seconds, and
// that shell fails closed: an unreadable lock reads as fresh.
func scheduleLockLines(names app.Names, job, deployLock, applicationLock string, lockTTL, rendezvousWait time.Duration) []string {
	ttlSeconds := int(math.Ceil(lockTTL.Seconds()))
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	waitSeconds := strconv.FormatFloat(rendezvousWait.Seconds(), 'f', -1, 64)
	waitMode := "--timeout " + waitSeconds
	busyReason := "the scheduling rendezvous is busy"
	if rendezvousWait > 0 {
		busyReason = "the scheduling rendezvous remained busy for " + rendezvousWait.String()
	} else {
		// util-linux documents --timeout 0 as equivalent to --nonblock, but
		// spelling the mode explicitly makes the zero-budget contract clear.
		waitMode = "--nonblock"
	}
	rendezvousMode := "--exclusive"
	if deployLock == "pinned" {
		// Pinned jobs only need to exclude writers while they establish their
		// immutable release leases. Different pinned jobs are readers of the
		// same release state and may safely enter together.
		rendezvousMode = "--shared"
	}
	return []string{
		"state=" + q(names.ScheduledJobRunState(job)),
		"tmp=\"$state.$$\"",
		// The operation and inputs of an operator request are kept on the skip
		// record too, so `ob job run` can find its own outcome.
		// Writing the state requires holding the job lock: it is the run in
		// flight that owns that file, and overwriting it would replace a real
		// run's outcome with this one's skip.
		"skip() { umask 077; printf 'skipped=%s\\noperation=%s\\ninputs=%s\\n' \"$1\" \"$operation\" \"$inputs_json\" >\"$tmp\"; mv -f \"$tmp\" \"$state\"; echo \"onebox: skipped: $1\" >&2; exit 0; }",
		// No lock, so no claim on the state file: the run already in flight
		// owns it and will record itself. This activation leaves its own note
		// instead, keyed to its own invocation, and the notifier reads that
		// rather than the state a different run is still writing. Without the
		// note the notifier would see a clean exit and record this activation
		// as a success that never ran.
		// Named for the activation, the same way the notifier looks for it.
		// The two have to agree exactly: a marker the notifier cannot find
		// sends it back to the state file, which is the run in flight's.
		"skip_marker=\"$state.skip.${INVOCATION_ID:-}\"",
		"stand_aside() { umask 077; printf 'skipped=%s\\noperation=%s\\ninputs=%s\\n' \"$1\" \"$operation\" \"$inputs_json\" >\"$skip_marker\"; echo \"onebox: skipped: $1\" >&2; exit 0; }",
		"exec 9>" + q(names.ScheduledJobRunLock(job)),
		"lock_code=0; /usr/bin/flock --exclusive --nonblock --conflict-exit-code " + strconv.Itoa(flockConflictExitCode) + " 9 || lock_code=$?; case $lock_code in 0) ;; " + strconv.Itoa(flockConflictExitCode) + ") stand_aside 'another run of this job is still in progress' ;; *) echo 'onebox: cannot acquire the scheduled-job lock' >&2; exit \"$lock_code\" ;; esac",
		// Only the activation that wrote a note removes it, so one lost
		// between the runner exiting and ExecStopPost — a power cut, a killed
		// systemd — would sit here forever. Swept a day later, under the job
		// lock, which is long past any live note's few milliseconds.
		"find " + q(names.AppDir()+"/schedule") + " -maxdepth 1 -name " + q(job+".state.skip.*") + " -mtime +1 -delete 2>/dev/null || true",
		"exec 8>" + q(names.ScheduleRunLock()),
		"lock_code=0; /usr/bin/flock " + rendezvousMode + " " + waitMode + " --conflict-exit-code " + strconv.Itoa(flockConflictExitCode) + " 8 || lock_code=$?; case $lock_code in 0) ;; " + strconv.Itoa(flockConflictExitCode) + ") skip " + q(busyReason) + " ;; *) echo 'onebox: cannot acquire the application scheduling lock' >&2; exit \"$lock_code\" ;; esac",
		"if [ -e " + q(applicationLock) + " ] && [ \"$(" + lockAgeCmd(applicationLock) + ")\" -le " + strconv.Itoa(ttlSeconds) + " ]; then skip 'an application operation holds the deploy lock'; fi",
	}
}

// scheduleRendezvousWait keeps the ordinary ten-second handoff without letting
// it consume a short job's entire systemd TimeoutStartSec. A second is reserved
// for the runner to record a contention skip and exit; sub-second jobs therefore
// use a non-blocking rendezvous rather than being killed while waiting.
func scheduleRendezvousWait(jobTimeout string) time.Duration {
	wait := time.Duration(scheduleRendezvousWaitSeconds) * time.Second
	timeout, ok := app.ParseDuration(jobTimeout)
	if !ok || timeout <= 0 {
		return wait
	}
	const exitReserve = time.Second
	if timeout <= exitReserve {
		return 0
	}
	if available := timeout - exitReserve; available < wait {
		return available
	}
	return wait
}

// requireScheduleHost is what a host needs before any scheduled job can be
// installed on it. Preflight asks it so a deploy refuses before staging, and
// SyncSchedules asks again so `ob schedule apply` cannot bypass it.
//
// systemd 252 introduced TRIGGER_UNIT, which is how the runner tells a timer
// firing from an operator's start. The floor applies only to a job that
// declares inputs, and to `ob job run`; see below for why, and why a host
// that has been running scheduled jobs for years is not refused one.
func (e *Engine) requireScheduleHost(ctx context.Context, jobs []app.ScheduledJob) error {
	if len(jobs) == 0 {
		return nil
	}
	if !e.hasScheduleFlock(ctx) {
		return errors.New("scheduled jobs require a compatible util-linux flock at /usr/bin/flock so lock contention can be distinguished from host failures; install util-linux or upgrade it and deploy again")
	}
	for _, job := range jobs {
		if job.Execution == nil {
			continue
		}
		res, err := e.T.Run(ctx, "/usr/bin/python3 -c 'import sys,fcntl; assert sys.version_info >= (3,8)'")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return errors.New("durable jobs require Python 3.8 or newer at /usr/bin/python3; install it and apply schedules again")
		}
		break
	}
	// Declared inputs are the one feature that cannot work without
	// TRIGGER_UNIT: the runner would have to guess whether an activation is
	// the operator's, and guessing wrong hands a timer firing the inputs a
	// person meant for their own run. Everything else works on an older
	// systemd, so a host that has run scheduled jobs for years keeps running
	// them — it only records `unknown` where it cannot know the trigger.
	if !needsTriggerUnit(jobs) || e.hasTriggerUnit(ctx) {
		return nil
	}
	return fmt.Errorf("a job declares inputs, which need systemd 252 or newer on the host: without $TRIGGER_UNIT the runner cannot tell a timer firing from an operator's run")
}

// hasTriggerUnit reports whether the host's systemd sets TRIGGER_UNIT on a
// timer activation, which systemd 252 introduced.
func (e *Engine) hasTriggerUnit(ctx context.Context) bool {
	if e.triggerUnitProbed {
		return e.triggerUnitPresent
	}
	res, err := e.T.Run(ctx, "systemctl --version 2>/dev/null | head -1")
	if err != nil {
		// A transport failure says nothing about the host's systemd. Caching
		// it would turn one flaky round trip into "this host is too old" for
		// the rest of the operation, and preflight would refuse the deploy.
		return false
	}
	e.triggerUnitProbed = true
	version, ok := systemdVersion(res.Stdout)
	e.triggerUnitPresent = ok && version >= 252
	return e.triggerUnitPresent
}

func needsTriggerUnit(jobs []app.ScheduledJob) bool {
	for _, job := range jobs {
		if len(job.Inputs) > 0 || job.Execution != nil {
			return true
		}
	}
	return false
}

func scheduleContainerRemove(container string) string {
	return "/usr/bin/docker rm -f " + q(container) + " >/dev/null 2>&1 || true"
}

// scheduleContainerStop gives the container its own TERM grace while the
// runner still owns its flock descriptors. Only Onebox's explicit KILL path
// sets forced_kill; an exit code alone cannot prove how the process stopped.
func scheduleContainerStop(container string, grace time.Duration) string {
	seconds := int((grace + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	name := q(container)
	return "forced_kill=false; " +
		"if [ \"$(/usr/bin/docker inspect -f '{{.State.Running}}' " + name + " 2>/dev/null || true)\" = true ]; then " +
		"/usr/bin/docker kill --signal TERM " + name + " >/dev/null 2>&1 || true; " +
		"deadline=$(($(date -u '+%s')+" + strconv.Itoa(seconds) + ")); " +
		"while [ \"$(/usr/bin/docker inspect -f '{{.State.Running}}' " + name + " 2>/dev/null || true)\" = true ]; do " +
		"if [ \"$(date -u '+%s')\" -ge \"$deadline\" ]; then /usr/bin/docker kill --signal KILL " + name + " >/dev/null 2>&1 || true; forced_kill=true; break; fi; sleep 1; done; fi; " +
		"/usr/bin/docker rm -f " + name + " >/dev/null 2>&1 || true; " +
		"if [ -f \"$state\" ]; then printf 'forced_kill=%s\\n' \"$forced_kill\" >>\"$state\"; fi"
}

// scheduleStateFunction renders the shell function both runners use to record
// the run in progress. The notifier reads it after the run ends, so the runner
// never removes it: a runner that cleaned up its own state would erase the
// only evidence a timed-out run leaves behind.
func scheduleStateFunction() []string {
	return []string{
		"write_state() {",
		"  umask 077",
		"  printf 'release=%s\\nstarted_at=%s\\nstarted_epoch=%s\\ntrigger=%s\\noperation=%s\\nattempt=%s\\nphase=%s\\ninputs=%s\\n' " +
			"\"$release\" \"$started_at\" \"$started_epoch\" \"$trigger\" \"$operation\" \"$1\" \"$phase\" \"$inputs_json\" >\"$tmp\"",
		"  mv -f \"$tmp\" \"$state\"",
		"}",
	}
}

// scheduleRunPreamble sets the variables write_state records. The trigger is
// systemd's own word for it: a timer activation carries TRIGGER_UNIT (systemd
// 252 and newer), anything else is an operator.
func scheduleRunPreamble(triggerUnit bool) []string {
	// TRIGGER_UNIT is set on a timer activation and on nothing else, so its
	// absence names an operator — but only on a systemd that sets it at all.
	// On an older host the runner says `unknown` rather than inventing a
	// trigger it cannot observe.
	otherwise := "unknown"
	if triggerUnit {
		otherwise = "operator"
	}
	return append([]string{
		"started_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')",
		"started_epoch=$(date -u '+%s')",
		"phase=starting",
		"if [ -n \"${TRIGGER_UNIT:-}\" ]; then trigger=timer; else trigger=" + otherwise + "; fi",
	}, scheduleStateFunction()...)
}

// scheduleInputsLines consumes the one-shot inputs file on an operator
// activation. Values reach the container as -e arguments, never as shell
// text, and the file is gone before any lock is taken so a skipped operator run
// cannot hand its inputs to the next timer firing. A timer activation never
// opens the file: TRIGGER_UNIT says which one this is.
func scheduleInputsLines(inputsPath string) []string {
	return []string{
		"operation=''",
		"execution=''",
		"expected_release=''",
		"expected_runtime=''",
		"inputs_json=''",
		"inputs_file=" + q(inputsPath),
		"if [ -z \"${TRIGGER_UNIT:-}\" ] && [ -f \"$inputs_file\" ]; then",
		"  while IFS= read -r line || [ -n \"$line\" ]; do",
		"    case \"$line\" in",
		"      ONEBOX_OPERATION=*) operation=${line#ONEBOX_OPERATION=} ;;",
		"      ONEBOX_EXECUTION=*) execution=${line#ONEBOX_EXECUTION=} ;;",
		"      ONEBOX_EXPECTED_RELEASE=*) expected_release=${line#ONEBOX_EXPECTED_RELEASE=} ;;",
		"      ONEBOX_EXPECTED_RUNTIME=*) expected_runtime=${line#ONEBOX_EXPECTED_RUNTIME=} ;;",
		"      [A-Z]*=*) set -- \"$@\" -e \"$line\"; key=${line%%=*}; value=${line#*=}; inputs_json=\"${inputs_json:+$inputs_json,}\\\"$key\\\":\\\"$value\\\"\" ;;",
		"    esac",
		"  done <\"$inputs_file\"",
		"  rm -f \"$inputs_file\"",
		"fi",
	}
}

// schedulePlannedBindingLines makes a sealed operator job plan authoritative at
// the point that owns execution: after the host runner has acquired its locks,
// immediately before it can start the container. Timer firings carry no
// expected binding and pass through unchanged.
const sealedManualJobBindingMarker = "Sealed manual job binding protocol v1."

func schedulePlannedBindingLines() []string {
	return []string{
		"# " + sealedManualJobBindingMarker,
		"if [ -n \"$expected_release\" ] && [ \"$release\" != \"$expected_release\" ]; then write_state 0; echo 'onebox: serving release changed after job approval' >&2; exit 74; fi",
		"if [ -n \"$expected_runtime\" ]; then",
		"  runtime_hash=$(sha256sum \"$release_dir/compose.yaml\") || { write_state 0; echo 'onebox: cannot hash the approved job runtime' >&2; exit 74; }",
		"  runtime_digest=sha256:${runtime_hash%% *}",
		"  if [ \"$runtime_digest\" != \"$expected_runtime\" ]; then write_state 0; echo 'onebox: serving runtime changed after job approval' >&2; exit 74; fi",
		"fi",
	}
}

// systemdVersion reads the leading number from `systemd 255 (255.4-1ubuntu8)`.
func systemdVersion(firstLine string) (int, bool) {
	fields := strings.Fields(firstLine)
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0, false
	}
	n, err := strconv.Atoi(fields[1])
	return n, err == nil
}

// scheduleAttemptLoop runs the container until it exits 0 or the attempts are
// spent. Backoff doubles and is capped; every sleep happens under the locks
// the run already holds, which is why validation keeps the sum under the
// timeout. A single-attempt job gets no loop, so its runner reads as before.
func scheduleAttemptLoop(job app.ScheduledJob, compose, container string) []string {
	if job.RetryAttempts <= 1 {
		return []string{"phase=running", "write_state 1", compose}
	}
	return []string{
		fmt.Sprintf("max_attempts=%d", job.RetryAttempts),
		// Whole seconds, the same rounding validation used to bound the sum.
		fmt.Sprintf("backoff=%d", app.RetryBackoffSeconds(job.RetryBackoff)),
		fmt.Sprintf("max_backoff=%d", app.RetryBackoffSeconds(job.RetryMaxBackoff)),
		"attempt=1",
		"while :; do",
		"  phase=running",
		"  write_state \"$attempt\"",
		// The container name is fixed, so a corpse from the previous attempt
		// would fail every attempt after it with "name already in use" and
		// turn one transient failure into all of them.
		"  " + scheduleContainerRemove(container),
		"  status=0",
		"  " + compose + " || status=$?",
		"  [ \"$status\" -eq 0 ] && exit 0",
		"  if [ \"$attempt\" -ge \"$max_attempts\" ]; then exit \"$status\"; fi",
		"  echo \"onebox: attempt $attempt of $max_attempts exited $status; retrying in ${backoff}s\" >&2",
		"  phase=backing-off",
		"  write_state \"$attempt\"",
		"  sleep \"$backoff\"",
		"  backoff=$((backoff * 2))",
		"  [ \"$backoff\" -gt \"$max_backoff\" ] && backoff=$max_backoff",
		"  attempt=$((attempt + 1))",
		"done",
	}
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
		// ExecStopPost runs after success, failures, and timeouts. It always
		// attempts fenced container cleanup, writes the run record from the
		// runner's state and SERVICE_RESULT, then notifies per the job's policy.
		"ExecStopPost=/bin/sh " + notifyPath,
		"TimeoutStartSec=" + job.Timeout,
		"TimeoutStopSec=" + (job.ShutdownGrace + 10*time.Second).String(),
		"",
	}, "\n")
}

const scheduleNotificationTimestamp = "__ONEBOX_SCHEDULE_TIMESTAMP__"

// scheduleRunIdentifier is the syslog identifier of the one line the notifier
// writes per run. The journal is the store, so there is no file to trim and
// nothing that can disagree with the unit's own log.
//
// The line carries its own ONEBOX_UNIT and ONEBOX_JOB fields and the history
// query matches on them, not on journald's cgroup attribution. A process that
// writes one line and exits is often gone before journald reads /proc for it,
// and such an entry has no _SYSTEMD_UNIT at all; `journalctl -u` would never
// find it. Explicit fields survive that race, and `logger --journald` is
// util-linux, which flock already requires.
const scheduleRunIdentifier = "onebox-run"

// scheduleRunRecordLines finalises the run the runner started. This lives in
// ExecStopPost because only systemd knows how the run ended: a timed-out
// runner is killed mid-sleep and cannot write its own outcome. Every value
// interpolated into the JSON is either numeric, a timestamp the runner
// formatted, a release id, or an input value the loader restricted to a
// charset that needs no escaping.
func scheduleRunRecordLines(application, unit, job, state string) []string {
	return []string{
		"state=" + q(state),
		"release=''; started_at=''; started_epoch=''; trigger=''; operation=''; attempt=0; inputs=''; skipped=''; execution=''; forced_kill=false",
		// A run that stood aside left a note under its own invocation. It
		// never held the job lock, so the state file belongs to whichever run
		// is still going: read the note and leave that file alone.
		"skip_marker=\"$state.skip.${INVOCATION_ID:-}\"",
		"if [ -f \"$skip_marker\" ]; then",
		"  while IFS= read -r line || [ -n \"$line\" ]; do",
		"    case \"$line\" in",
		"      skipped=*) skipped=${line#skipped=} ;;",
		"      forced_kill=*) forced_kill=${line#forced_kill=} ;;",
		"      operation=*) operation=${line#operation=} ;;",
		"      inputs=*) inputs=${line#inputs=} ;;",
		"    esac",
		"  done <\"$skip_marker\"",
		"  rm -f \"$skip_marker\"",
		"elif [ -f \"$state\" ]; then",
		"  while IFS= read -r line || [ -n \"$line\" ]; do",
		"    case \"$line\" in",
		"      release=*) release=${line#release=} ;;",
		"      execution=*) execution=${line#execution=} ;;",
		"      started_at=*) started_at=${line#started_at=} ;;",
		"      started_epoch=*) started_epoch=${line#started_epoch=} ;;",
		"      trigger=*) trigger=${line#trigger=} ;;",
		"      operation=*) operation=${line#operation=} ;;",
		"      attempt=*) attempt=${line#attempt=} ;;",
		"      inputs=*) inputs=${line#inputs=} ;;",
		"      skipped=*) skipped=${line#skipped=} ;;",
		"      forced_kill=*) forced_kill=${line#forced_kill=} ;;",
		"    esac",
		"  done <\"$state\"",
		"  rm -f \"$state\"",
		"fi",
		"if [ -z \"$trigger\" ]; then if [ -n \"${TRIGGER_UNIT:-}\" ]; then trigger=timer; else trigger=unknown; fi; fi",
		"result=${SERVICE_RESULT:-success}",
		"status=${EXIT_STATUS:-0}",
		// EXIT_STATUS is a signal name when the main process was killed.
		"case \"$status\" in ''|*[!0-9]*) status=null ;; esac",
		"case \"$attempt\" in ''|*[!0-9]*) attempt=0 ;; esac",
		"case \"$forced_kill\" in true) ;; *) forced_kill=false ;; esac",
		// A skip is the runner's own word, written before any container ran;
		// a container that exits non-zero, 75 included, is a failure.
		"if [ \"$result\" = timeout ]; then outcome=timeout",
		"elif [ -n \"$skipped\" ]; then outcome=skipped",
		"elif [ \"$result\" = success ] && [ \"$status\" = 0 ]; then outcome=success",
		"else outcome=failure; fi",
		"finished_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')",
		"now=$(date -u '+%s')",
		"duration=0",
		"case \"$started_epoch\" in ''|*[!0-9]*) ;; *) duration=$((now - started_epoch)) ;; esac",
		"[ -z \"$started_at\" ] && started_at=$finished_at",
		"execution_field=''",
		"if [ -n \"$execution\" ]; then execution_field=$(printf '\"execution\":\"%s\",' \"$execution\"); fi",
		"record=$(printf '{%s\"run\":\"%s\",\"job\":\"%s\",\"trigger\":\"%s\",\"operation\":\"%s\",\"release\":\"%s\",\"started_at\":\"%s\",\"finished_at\":\"%s\",\"duration_s\":%s,\"attempts\":%s,\"exit_status\":%s,\"outcome\":\"%s\",\"forced_kill\":%s,\"reason\":\"%s\",\"inputs\":{%s}}' " +
			"\"$execution_field\" \"${INVOCATION_ID:-}\" " + q(job) + " \"$trigger\" \"$operation\" \"$release\" \"$started_at\" \"$finished_at\" \"$duration\" \"$attempt\" \"$status\" \"$outcome\" \"$forced_kill\" \"$skipped\" \"$inputs\")",
		"printf 'MESSAGE=%s\\nPRIORITY=6\\nSYSLOG_IDENTIFIER=" + scheduleRunIdentifier + "\\nONEBOX_APP=%s\\nONEBOX_UNIT=%s\\nONEBOX_JOB=%s\\n' " +
			"\"$record\" " + q(application) + " " + q(unit) + " " + q(job) + " | logger --journald || true",
	}
}

// scheduleNotificationRun marks where the notifier substitutes the run id at
// send time. It travels as the payload's deploy_id: the correlation key an
// operator hands to `ob job logs --run`. Nothing else about the run goes
// into a notification; the notify package redacts diagnostics on purpose, and
// attempts, duration and exit status belong to the run record on the host.
const scheduleNotificationRun = "__ONEBOX_SCHEDULE_RUN__"

// scheduleNotifier extends the existing notification contract to work fired
// directly by systemd. It finalises the run record first, then sends for the
// outcomes the job selected. The generated file is mode 0600, keeping webhook
// tokens out of unit metadata, and every send is bounded and fail-open.
//
// Bodies are prepared here, once per outcome class, because the payload
// contract lives in the notify package and the host has no Onebox to ask at
// 2am. Only the timestamp and the run id are filled in on the host.
func (e *Engine) scheduleNotifier(job app.ScheduledJob) (string, error) {
	cleanup := scheduleContainerRemove(e.names().Container(job.Name, 1))
	if job.Execution != nil {
		cleanup = durableContainerStop(e.names().Container(job.Name, 1), job.ShutdownGrace)
	}
	environment := e.Opts.Environment
	lines := []string{
		"#!/bin/sh",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"set -u",
		"state=" + q(e.names().ScheduledJobRunState(job.Name)),
		"exec 9>" + q(e.names().ScheduledJobRunLock(job.Name)),
		"if /usr/bin/flock --exclusive --nonblock 9; then",
		"  " + cleanup,
		"fi",
	}
	lines = append(lines, scheduleRunRecordLines(e.Spec.Name, e.names().ScheduledJobUnit(job.Name), job.Name, e.names().ScheduledJobRunState(job.Name))...)
	lines = append(lines, `case " `+strings.Join(job.Notify, " ")+` " in *" $outcome "*) ;; *) exit 0 ;; esac`)
	// Three classes, because a skip is neither: the job did not fail, and it
	// did not do its work either.
	var wants = map[string]bool{}
	for _, outcome := range job.Notify {
		switch outcome {
		case "success":
			wants["ok"] = true
		case "skipped":
			wants["skipped"] = true
		default:
			wants["fail"] = true
		}
	}
	sends := map[string][]string{}
	for _, class := range []string{"ok", "skipped", "fail"} {
		if !wants[class] {
			continue
		}
		rendered, err := e.scheduleNotificationSends(job.Name, environment, class)
		if err != nil {
			return "", err
		}
		sends[class] = rendered
	}
	if len(sends["ok"])+len(sends["skipped"])+len(sends["fail"]) > 0 {
		lines = append(lines, `ts=$(date -u '+%Y-%m-%dT%H:%M:%SZ')`)
		lines = append(lines, `if [ "$outcome" = success ]; then`)
		lines = append(lines, orNoop(sends["ok"])...)
		lines = append(lines, `elif [ "$outcome" = skipped ]; then`)
		lines = append(lines, orNoop(sends["skipped"])...)
		lines = append(lines, "else")
		lines = append(lines, orNoop(sends["fail"])...)
		lines = append(lines, "fi", "wait || true")
	}
	lines = append(lines, "exit 0", "")
	return strings.Join(lines, "\n"), nil
}

// orNoop keeps a shell branch syntactically present when it has nothing to do.
func orNoop(lines []string) []string {
	if len(lines) == 0 {
		return []string{"  :"}
	}
	return lines
}

// scheduleNotificationSends renders one backgrounded curl per notification
// that selects the given class: ok, skipped, or fail. A skip routes with the
// failures, because that is the channel an operator watches, and says what it
// is rather than claiming the job failed. A failed send is logged and never
// replaces the job's own result.
func (e *Engine) scheduleNotificationSends(job, environment, class string) ([]string, error) {
	var sends []string
	for _, name := range sortedNames(e.Spec.Notifications) {
		cfg := e.Spec.Notifications[name]
		status := "fail"
		if class == "ok" {
			status = "ok"
		}
		payload := notify.Payload{
			App: e.Spec.Name, Env: environment, Host: e.T.Destination(),
			Verb: "scheduled job " + job, Status: status,
			DeployID: scheduleNotificationRun, TS: scheduleNotificationTimestamp,
			Skipped: class == "skipped",
		}
		if status != "ok" {
			payload.Error = "scheduled job failed; inspect trusted host diagnostics"
		}
		prepared, err := notify.Prepare(cfg, payload)
		if err != nil {
			return nil, err
		}
		if prepared == nil {
			continue
		}
		curl := "curl --fail --silent --show-error --max-time 5 --request POST" +
			" --header " + q("Content-Type: "+prepared.ContentType) +
			" --header " + q("X-Title: "+prepared.Title) +
			` --data-binary "$body" ` + q(cfg.Webhook)
		sends = append(sends, "  (body="+shellBody(string(prepared.Body))+"; if ! "+curl+"; then echo "+
			q("onebox: notification "+name+" failed")+" >&2; fi) &")
	}
	return sends, nil
}

// shellBody quotes a prepared body for the shell, leaving the two runtime
// placeholders as expansions of variables the notifier sets before sending.
func shellBody(body string) string {
	var out strings.Builder
	for body != "" {
		next, placeholder, expansion := -1, "", ""
		if i := strings.Index(body, scheduleNotificationTimestamp); i >= 0 {
			next, placeholder, expansion = i, scheduleNotificationTimestamp, `"$ts"`
		}
		if i := strings.Index(body, scheduleNotificationRun); i >= 0 && (next < 0 || i < next) {
			next, placeholder, expansion = i, scheduleNotificationRun, `"${INVOCATION_ID:-}"`
		}
		if next < 0 {
			out.WriteString(q(body))
			break
		}
		if next > 0 {
			out.WriteString(q(body[:next]))
		}
		out.WriteString(expansion)
		body = body[next+len(placeholder):]
	}
	return out.String()
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
		// systemd's one-minute default deliberately coalesces local timers.
		// Five-field cron already chooses the minute; keep that staggering
		// instead of bunching unrelated jobs at one host-wide wake-up.
		"AccuracySec=1s",
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
	// namespace — app.JobUnitPrefix explains why. Teardown is the opposite case
	// and needs both: matching only the job prefix once left backup timers
	// loaded and firing against a release directory `ob destroy` had just
	// deleted. They belong to this application and they go with it: the host
	// owner record keeps a host to one application.
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
		if strings.HasPrefix(unit, app.JobUnitPrefix) || strings.HasPrefix(unit, app.BackupUnitPrefix) {
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
