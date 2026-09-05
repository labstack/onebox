package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

// ScheduleRunRecord is the line the notifier writes to the journal when a
// scheduled run ends. The journal is the store: there is no file to trim and
// nothing that can disagree with the unit's own log.
type ScheduleRunRecord struct {
	Run             string `json:"run"`
	Job             string `json:"job"`
	Trigger         string `json:"trigger"`
	Operation       string `json:"operation,omitempty"`
	Release         string `json:"release,omitempty"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at"`
	DurationSeconds int    `json:"duration_s"`
	Attempts        int    `json:"attempts"`
	ExitStatus      *int   `json:"exit_status"`
	Outcome         string `json:"outcome"`
	// Reason is set on a skipped run: what the runner met instead of running.
	Reason string            `json:"reason,omitempty"`
	Inputs map[string]string `json:"inputs,omitempty"`
}

// ScheduleListing is one declared job beside its timer as the host reports it.
type ScheduleListing struct {
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Cron        string `json:"cron"`
	Timezone    string `json:"timezone"`
	DeployLock  string `json:"deploy_lock"`
	Timeout     string `json:"timeout"`
	TimerState  string `json:"timer_state"`
	NextRun     string `json:"next_run,omitempty"`
	LastTrigger string `json:"last_trigger,omitempty"`
}

// A run id is systemd's invocation id. It reaches a shell as a journalctl
// match, so it is checked against the only shape systemd produces.
var scheduleRunID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// scheduleHistoryCommand matches the record's own fields rather than the
// unit journald attributed it to; see scheduleRunIdentifier for why.
func scheduleHistoryCommand(unit string, n int) string {
	if n <= 0 {
		n = 20
	}
	return "journalctl SYSLOG_IDENTIFIER=" + scheduleRunIdentifier + " ONEBOX_UNIT=" + q(unit) +
		" -o cat -r -n " + strconv.Itoa(n) + " --no-pager 2>/dev/null || true"
}

// parseScheduleRunRecords keeps the lines that decode and drops the rest: a
// truncated or hand-written entry must not hide the records around it.
func parseScheduleRunRecords(stdout string) []ScheduleRunRecord {
	var out []ScheduleRunRecord
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var record ScheduleRunRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		out = append(out, record)
	}
	return out
}

func (e *Engine) scheduledJob(name string) (app.ScheduledJob, error) {
	jobs, err := e.Spec.ScheduledJobs()
	if err != nil {
		return app.ScheduledJob{}, err
	}
	for _, job := range jobs {
		if job.Name == name {
			return job, nil
		}
	}
	return app.ScheduledJob{}, fmt.Errorf("job %q is not a scheduled job", name)
}

// ScheduleHistory returns the newest n run records of one job, newest first.
func (e *Engine) ScheduleHistory(ctx context.Context, name string, n int) ([]ScheduleRunRecord, error) {
	job, err := e.scheduledJob(name)
	if err != nil {
		return nil, err
	}
	res, err := e.T.Run(ctx, scheduleHistoryCommand(e.names().ScheduledJobUnit(job.Name), n))
	if err != nil {
		return nil, err
	}
	records := parseScheduleRunRecords(res.Stdout)
	if records == nil {
		records = []ScheduleRunRecord{}
	}
	return records, nil
}

// ScheduleList reads every declared job's timer in one round trip.
func (e *Engine) ScheduleList(ctx context.Context) ([]ScheduleListing, error) {
	jobs, err := e.Spec.ScheduledJobs()
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return []ScheduleListing{}, nil
	}
	var commands []string
	for _, job := range jobs {
		unit := e.names().ScheduledJobUnit(job.Name)
		commands = append(commands,
			"printf '%s\\n' "+q("@@"+job.Name),
			"systemctl show "+q(unit+".timer")+" --no-pager --property=ActiveState --property=NextElapseUSecRealtime --property=LastTriggerUSec 2>/dev/null || true")
	}
	res, err := e.T.Run(ctx, strings.Join(commands, "\n"))
	if err != nil {
		return nil, err
	}
	observed := map[string]map[string]string{}
	current := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "@@") {
			current = strings.TrimPrefix(line, "@@")
			observed[current] = map[string]string{}
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok && current != "" {
			observed[current][key] = value
		}
	}
	out := make([]ScheduleListing, 0, len(jobs))
	for _, job := range jobs {
		values := observed[job.Name]
		out = append(out, ScheduleListing{
			Name: job.Name, Unit: e.names().ScheduledJobUnit(job.Name), Cron: job.Cron, Timezone: job.Timezone,
			DeployLock: job.DeployLock, Timeout: job.Timeout, TimerState: values["ActiveState"],
			NextRun: values["NextElapseUSecRealtime"], LastTrigger: values["LastTriggerUSec"],
		})
	}
	return out, nil
}

// ScheduleLogs streams the journal of one run and returns the run id it
// streamed. The run id is systemd's invocation id, so the output is exactly
// that activation and nothing else. With no run given, the newest record's run
// is used, and the caller learns which one that was.
func (e *Engine) ScheduleLogs(ctx context.Context, name, run string, stdout, stderr io.Writer) (string, error) {
	if _, err := e.scheduledJob(name); err != nil {
		return "", err
	}
	if run == "" {
		records, err := e.ScheduleHistory(ctx, name, 1)
		if err != nil {
			return "", err
		}
		if len(records) == 0 {
			return "", fmt.Errorf("job %s has no recorded runs", name)
		}
		run = records[0].Run
	}
	if !scheduleRunID.MatchString(run) {
		return "", fmt.Errorf("run id %q is not a systemd invocation id", run)
	}
	return run, e.T.RunStream(ctx, "journalctl _SYSTEMD_INVOCATION_ID="+run+" --no-pager -o short-iso", stdout, stderr)
}
