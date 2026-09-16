package engine

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/labstack/onebox/internal/journal"
)

// JobHistoryRecord is the common read model for timer, operator-submitted,
// and sealed job executions. The stores remain independent; Operation joins
// the workstation audit lifecycle to a host-supervised activation.
type JobHistoryRecord struct {
	ID              string            `json:"id"`
	Run             string            `json:"run,omitempty"`
	Operation       string            `json:"operation,omitempty"`
	Job             string            `json:"job"`
	Trigger         string            `json:"trigger"`
	Release         string            `json:"release,omitempty"`
	StartedAt       string            `json:"started_at"`
	FinishedAt      string            `json:"finished_at,omitempty"`
	DurationSeconds int               `json:"duration_s,omitempty"`
	Attempts        int               `json:"attempts,omitempty"`
	ExitStatus      *int              `json:"exit_status,omitempty"`
	Outcome         string            `json:"outcome"`
	ForcedKill      bool              `json:"forced_kill,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	Operator        string            `json:"operator,omitempty"`
	Inputs          map[string]string `json:"inputs,omitempty"`
}

// JobHistory merges existing host records at read time. It deliberately does
// not introduce another store or imply completeness beyond journal retention.
func (e *Engine) JobHistory(ctx context.Context, name string, n int) ([]JobHistoryRecord, error) {
	workload, ok := e.Spec.Workloads[name]
	if !ok || !workload.IsJob() {
		return nil, fmt.Errorf("unknown job %q", name)
	}
	var out []JobHistoryRecord
	byOperation := map[string]int{}
	if workload.Schedule != nil {
		records, err := e.ScheduleHistory(ctx, name, n)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			out = append(out, JobHistoryRecord{
				ID: record.Run, Run: record.Run, Operation: record.Operation, Job: name,
				Trigger: record.Trigger, Release: record.Release, StartedAt: record.StartedAt,
				FinishedAt: record.FinishedAt, DurationSeconds: record.DurationSeconds,
				Attempts: record.Attempts, ExitStatus: record.ExitStatus, Outcome: record.Outcome,
				ForcedKill: record.ForcedKill, Reason: record.Reason, Inputs: record.Inputs,
			})
			if record.Operation != "" {
				byOperation[record.Operation] = len(out) - 1
			}
		}
	}

	ids, journals, err := journal.Journals(ctx, e.T, e.names())
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		records := journals[id]
		var start, finish *journal.Record
		for i := range records {
			record := &records[i]
			// Target supports records written before schedule-run populated the
			// common Service field.
			if (record.Service != name && record.Target != name) || (record.Phase != "job" && record.Phase != "schedule-run") {
				continue
			}
			if record.Event == "start" && start == nil {
				start = record
			}
			if record.Event == "finish" || record.Event == "abort" {
				finish = record
			}
		}
		if start == nil {
			continue
		}
		if index, exists := byOperation[start.DeployID]; exists {
			out[index].Operator = start.Operator
			continue
		}
		if start.Phase != "job" {
			continue // a host record is the outcome authority for schedule-run
		}
		record := JobHistoryRecord{
			ID: start.DeployID, Operation: start.DeployID, Job: name, Trigger: "operator",
			Release: start.ReleaseID, StartedAt: start.TS, Operator: start.Operator,
			Attempts: 1, Outcome: "incomplete",
		}
		if finish != nil {
			record.FinishedAt = finish.TS
			record.DurationSeconds = elapsedSeconds(start.TS, finish.TS)
			switch {
			case finish.ErrorCode == "interrupted":
				record.Outcome = "interrupted"
			case finish.Event == "abort":
				record.Outcome = "aborted"
			case finish.Status == "ok":
				record.Outcome = "success"
			default:
				record.Outcome = "failure"
			}
		}
		out = append(out, record)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	if n <= 0 {
		n = 20
	}
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

func elapsedSeconds(started, finished string) int {
	start, startErr := time.Parse(time.RFC3339, started)
	finish, finishErr := time.Parse(time.RFC3339, finished)
	if startErr != nil || finishErr != nil || finish.Before(start) {
		return 0
	}
	return int(finish.Sub(start).Seconds())
}

// JobLogs streams exact logs for host-supervised executions. Sealed attached
// executions have durable outcome evidence but no separate retained log.
func (e *Engine) JobLogs(ctx context.Context, name, run string, stdout, stderr io.Writer) (string, error) {
	workload, ok := e.Spec.Workloads[name]
	if !ok || !workload.IsJob() {
		return "", fmt.Errorf("unknown job %q", name)
	}
	if workload.Schedule == nil {
		return "", fmt.Errorf("job %s has no host-supervised run logs", name)
	}
	return e.ScheduleLogs(ctx, name, run, stdout, stderr)
}
