package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/release"
)

// StatusSchedule is the host-observed state of one declared scheduled job. The
// verdict comes from the run records the notifier writes to the journal:
// outcome, attempts, duration, and how many firings in a row have failed or
// been skipped. systemd contributes the timer's state and next elapse and
// whether a run is in progress.
type StatusSchedule struct {
	Name          string   `json:"name"`
	Unit          string   `json:"unit"`
	TimerState    string   `json:"timer_state"`
	Running       bool     `json:"running"`
	DeployLock    string   `json:"deploy_lock"`
	Timeout       string   `json:"timeout"`
	PinnedRelease string   `json:"pinned_release,omitempty"`
	StartedAt     string   `json:"started_at,omitempty"`
	Diverged      bool     `json:"diverged"`
	Issues        []string `json:"issues,omitempty"`

	NextRun             string `json:"next_run,omitempty"`
	Attempt             int    `json:"attempt,omitempty"`
	LastOutcome         string `json:"last_outcome,omitempty"`
	LastReason          string `json:"last_reason,omitempty"`
	LastDurationSeconds int    `json:"last_duration_s,omitempty"`
	LastAttempts        int    `json:"last_attempts,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	ConsecutiveSkips    int    `json:"consecutive_skips,omitempty"`
	// JournalPersistent is false when the host keeps its journal in memory, so
	// the records above only reach back to the last boot.
	JournalPersistent bool `json:"journal_persistent"`
}

// skipStreakIssue is how many firings in a row may be skipped before status
// says so. One skip is timing; a streak is a job that never runs, which is the
// failure mode a skipped record exists to expose.
const skipStreakIssue = 3

type scheduleUnitObservation struct {
	loadState   string
	activeState string
	release     string
	startedAt   string
	attempt     string
	next        string
	history     []ScheduleRunRecord
}

func (e *Engine) scheduleStatuses(ctx context.Context) ([]StatusSchedule, error) {
	jobs, err := e.Spec.ScheduledJobs()
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return []StatusSchedule{}, nil
	}

	commands := []string{
		"printf '%s\\n' '@@journal'",
		"if [ -d /var/log/journal ]; then echo persistent; else echo volatile; fi",
	}
	for _, job := range jobs {
		unit := e.names().ScheduledJobUnit(job.Name)
		commands = append(commands,
			"printf '%s\\n' "+q("@@"+job.Name+":service"),
			"systemctl show "+q(unit+".service")+" --no-pager --property=LoadState --property=ActiveState",
			"printf '%s\\n' "+q("@@"+job.Name+":timer"),
			"systemctl show "+q(unit+".timer")+" --no-pager --property=LoadState --property=ActiveState --property=NextElapseUSecRealtime",
			"printf '%s\\n' "+q("@@"+job.Name+":run"),
			"cat "+q(e.names().ScheduledJobRunState(job.Name))+" 2>/dev/null || true",
			"printf '%s\\n' "+q("@@"+job.Name+":history"),
			// Status degrades rather than fails: an unreadable journal costs
			// this section its records, not the whole report. `ob schedule
			// history` is the command that says why the read failed.
			scheduleHistoryCommand(unit, 20)+" 2>/dev/null || true",
		)
	}
	res, err := e.T.Run(ctx, strings.Join(commands, "\n"))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read scheduled-job state (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	observed := map[string]map[string]scheduleUnitObservation{}
	journalPersistent := true
	name, kind := "", ""
	values := map[string]string{}
	var raw []string
	flush := func() {
		if name == "" || kind == "" {
			return
		}
		if observed[name] == nil {
			observed[name] = map[string]scheduleUnitObservation{}
		}
		observed[name][kind] = scheduleUnitObservation{
			loadState: values["LoadState"], activeState: values["ActiveState"],
			release: values["release"], startedAt: values["started_at"], attempt: values["attempt"],
			next:    values["NextElapseUSecRealtime"],
			history: parseScheduleRunRecords(strings.Join(raw, "\n"), name),
		}
		values = map[string]string{}
		raw = nil
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "@@") {
			flush()
			marker := strings.TrimPrefix(line, "@@")
			if marker == "journal" {
				name, kind = "", ""
				continue
			}
			name, kind, _ = strings.Cut(marker, ":")
			continue
		}
		if name == "" {
			if line == "volatile" {
				journalPersistent = false
			}
			continue
		}
		if kind == "history" {
			raw = append(raw, line)
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	flush()

	statuses := make([]StatusSchedule, 0, len(jobs))
	for _, job := range jobs {
		unit := e.names().ScheduledJobUnit(job.Name)
		service := observed[job.Name]["service"]
		timer := observed[job.Name]["timer"]
		run := observed[job.Name]["run"]
		records := observed[job.Name]["history"].history
		status := StatusSchedule{
			Name: job.Name, Unit: unit, TimerState: timer.activeState,
			Running: service.activeState == "activating", DeployLock: job.DeployLock, Timeout: job.Timeout,
			NextRun: timer.next, JournalPersistent: journalPersistent,
		}
		if status.Running {
			status.Attempt, _ = strconv.Atoi(run.attempt)
			if job.DeployLock == "pinned" {
				_, timeErr := time.Parse(time.RFC3339, run.startedAt)
				if !release.IsID(run.release) || timeErr != nil {
					status.Issues = append(status.Issues, "running pinned job state is unavailable or invalid")
				} else {
					status.PinnedRelease = run.release
					status.StartedAt = run.startedAt
				}
			}
		}
		// lastRun is the newest record that actually ran: the one whose
		// outcome is the job's standing verdict. A skip is news about timing,
		// and it must not clear a failure that nothing has fixed yet.
		var lastRun *ScheduleRunRecord
		if len(records) > 0 {
			last := records[0]
			status.LastOutcome = last.Outcome
			status.LastReason = last.Reason
			status.LastDurationSeconds = last.DurationSeconds
			status.LastAttempts = last.Attempts
			for _, record := range records {
				if record.Outcome != "skipped" {
					break
				}
				status.ConsecutiveSkips++
			}
			for i, record := range records {
				if record.Outcome == "skipped" {
					continue
				}
				if lastRun == nil {
					lastRun = &records[i]
				}
				if record.Outcome != "failure" && record.Outcome != "timeout" {
					break
				}
				status.ConsecutiveFailures++
			}
		}
		if timer.loadState != "loaded" || timer.activeState != "active" {
			status.Issues = append(status.Issues, "timer is not active")
		}
		if service.loadState != "loaded" {
			status.Issues = append(status.Issues, "service unit is not loaded")
		}
		// The record is the verdict: the newest run that actually happened is
		// the one that counts, and so is a job that keeps being skipped.
		if lastRun != nil && (lastRun.Outcome == "failure" || lastRun.Outcome == "timeout") {
			exit := "?"
			if lastRun.ExitStatus != nil {
				exit = strconv.Itoa(*lastRun.ExitStatus)
			}
			issue := fmt.Sprintf("last run failed: %s (exit %s)", lastRun.Outcome, exit)
			if status.LastOutcome == "skipped" {
				issue += ", and nothing has run since"
			}
			status.Issues = append(status.Issues, issue)
		}
		if status.ConsecutiveSkips >= skipStreakIssue {
			status.Issues = append(status.Issues, fmt.Sprintf("skipped %d firings in a row: %s", status.ConsecutiveSkips, status.LastReason))
		}
		status.Diverged = len(status.Issues) > 0
		statuses = append(statuses, status)
	}
	return statuses, nil
}
