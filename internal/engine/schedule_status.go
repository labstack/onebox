package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/release"
)

// StatusSchedule is the host-observed state of one declared scheduled job.
// systemd's Result and exit status are reported as observed; the verdict comes
// from the run records the notifier writes to the journal: outcome, attempts,
// duration, and how many firings in a row have failed.
type StatusSchedule struct {
	Name           string   `json:"name"`
	Unit           string   `json:"unit"`
	TimerState     string   `json:"timer_state"`
	Running        bool     `json:"running"`
	DeployLock     string   `json:"deploy_lock"`
	Timeout        string   `json:"timeout"`
	PinnedRelease  string   `json:"pinned_release,omitempty"`
	StartedAt      string   `json:"started_at,omitempty"`
	LastResult     string   `json:"last_result"`
	LastExitStatus int      `json:"last_exit_status,omitempty"`
	Diverged       bool     `json:"diverged"`
	Issues         []string `json:"issues,omitempty"`

	// From the timer and the run records.
	NextRun             string `json:"next_run,omitempty"`
	Attempt             int    `json:"attempt,omitempty"`
	LastOutcome         string `json:"last_outcome,omitempty"`
	LastDurationSeconds int    `json:"last_duration_s,omitempty"`
	LastAttempts        int    `json:"last_attempts,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	// JournalPersistent is false when the host keeps its journal in memory, so
	// the records above only reach back to the last boot.
	JournalPersistent bool `json:"journal_persistent"`
}

type scheduleUnitObservation struct {
	loadState   string
	activeState string
	result      string
	exitStatus  int
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
			"systemctl show "+q(unit+".service")+" --no-pager --property=LoadState --property=ActiveState --property=Result --property=ExecMainStatus",
			"printf '%s\\n' "+q("@@"+job.Name+":timer"),
			"systemctl show "+q(unit+".timer")+" --no-pager --property=LoadState --property=ActiveState --property=NextElapseUSecRealtime",
			"printf '%s\\n' "+q("@@"+job.Name+":run"),
			"cat "+q(e.names().ScheduledJobRunState(job.Name))+" 2>/dev/null || true",
			"printf '%s\\n' "+q("@@"+job.Name+":history"),
			scheduleHistoryCommand(unit, 20),
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
		exit, _ := strconv.Atoi(values["ExecMainStatus"])
		if observed[name] == nil {
			observed[name] = map[string]scheduleUnitObservation{}
		}
		observed[name][kind] = scheduleUnitObservation{
			loadState: values["LoadState"], activeState: values["ActiveState"],
			result: values["Result"], exitStatus: exit,
			release: values["release"], startedAt: values["started_at"], attempt: values["attempt"],
			next:    values["NextElapseUSecRealtime"],
			history: parseScheduleRunRecords(strings.Join(raw, "\n")),
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
			LastResult: service.result, LastExitStatus: service.exitStatus,
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
		if len(records) > 0 {
			last := records[0]
			status.LastOutcome = last.Outcome
			status.LastDurationSeconds = last.DurationSeconds
			status.LastAttempts = last.Attempts
			// A skip says nothing about the job, so it neither breaks nor
			// extends a failure streak.
			for _, record := range records {
				if record.Outcome == "skipped" {
					continue
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
		// The record is the verdict. systemd's Result and ExecMainStatus are
		// still reported as observed, but only a recorded failure or timeout
		// is an issue.
		if status.LastOutcome == "failure" || status.LastOutcome == "timeout" {
			exit := "?"
			if records[0].ExitStatus != nil {
				exit = strconv.Itoa(*records[0].ExitStatus)
			}
			status.Issues = append(status.Issues, fmt.Sprintf("last run failed: %s (exit %s)", status.LastOutcome, exit))
		}
		status.Diverged = len(status.Issues) > 0
		statuses = append(statuses, status)
	}
	return statuses, nil
}
