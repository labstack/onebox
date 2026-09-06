package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/journal"
)

// SchedulePause stops one job's timer until someone resumes it.
//
// A pause is host state, not project state. The project says the job runs
// hourly; the pause says this machine is not doing that right now, and why.
// Keeping it on the host is what lets the next deploy install a fixed unit
// without quietly starting the timer again — a pause a deploy undoes is not a
// pause, and the operator who set it would not find out.
//
// The reason is required for the same purpose `ob exec` requires one: a job
// that is deliberately not running looks exactly like one that is broken, and
// the difference is only ever in someone's head unless it is written down.
//
// A run already in flight is left alone. Pausing stops the next firing; it is
// not a way to kill work that has started.
func (e *Engine) SchedulePause(ctx context.Context, operationID, name, reason string) (err error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("schedule pause requires --reason: a job that is deliberately not running is indistinguishable from one that is broken")
	}
	if err := ValidateExecReason(reason); err != nil {
		return err
	}
	return e.setSchedulePause(ctx, operationID, name, reason, true)
}

// ScheduleResume starts a paused job's timer again and removes the record of
// the pause. The job's next run is its next scheduled elapse; resuming does
// not run it now, and does not make up firings missed while it was paused.
func (e *Engine) ScheduleResume(ctx context.Context, operationID, name string) error {
	return e.setSchedulePause(ctx, operationID, name, "", false)
}

func (e *Engine) setSchedulePause(ctx context.Context, operationID, name, reason string, pause bool) (err error) {
	if strings.TrimSpace(operationID) == "" {
		return errors.New("schedule pause requires an operation id")
	}
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	job, err := e.scheduledJob(name)
	if err != nil {
		return err
	}
	unit := e.names().ScheduledJobUnit(job.Name)
	marker := e.names().ScheduledJobPause(job.Name)

	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return err
	}

	phase, verb := "schedule-resume", "resumed"
	if pause {
		phase, verb = "schedule-pause", "paused"
	}
	writer := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: operationID, Epoch: epoch, Operator: journal.DefaultOperator(),
		GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner,
	}
	record := journal.Record{Phase: phase, Event: "start", Status: "ok", Target: name, TargetKind: "job", Reason: reason}
	if err := writer.Append(ctx, record); err != nil {
		return fmt.Errorf("journal %s start: %w", phase, err)
	}
	defer func() {
		finish := record
		finish.Event, finish.Status = "finish", "ok"
		if err != nil {
			finish.Status = "fail"
		}
		if appendErr := writer.Append(ctx, finish); appendErr != nil {
			err = errors.Join(err, fmt.Errorf("journal %s finish: %w", phase, appendErr))
		}
	}()

	if pause {
		body := fmt.Sprintf("operator=%s\npaused_at=%s\nreason=%s\n",
			journal.DefaultOperator(), e.Opts.Now().UTC().Format(time.RFC3339), reason)
		create := "umask 077 && install -d -m 700 " + q(e.names().AppDir()+"/schedule") + " && cat > " + q(marker)
		res, writeErr := e.mutateInput(ctx, create, body)
		if writeErr != nil {
			return writeErr
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("cannot record the pause at %s: %s", marker, strings.TrimSpace(res.Stderr))
		}
	}

	// The timer is stopped before the marker is removed on resume, and after
	// it is written on pause, so a failure in between leaves the host in the
	// state the marker describes rather than one it does not.
	action := "enable --now"
	if pause {
		action = "disable --now"
	}
	res, err := e.mutate(ctx, "systemctl "+action+" "+q(unit+".timer"))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("systemctl %s %s: %s", action, unit, strings.TrimSpace(res.Stderr))
	}
	if !pause {
		if res, removeErr := e.mutate(ctx, "rm -f "+q(marker)); removeErr != nil {
			return removeErr
		} else if res.ExitCode != 0 {
			return fmt.Errorf("cannot clear the pause at %s: %s", marker, strings.TrimSpace(res.Stderr))
		}
	}
	e.logf("schedule: %s %s", name, verb)
	return nil
}

// SchedulePauseState is what the host records about a paused job.
type SchedulePauseState struct {
	Operator string `json:"operator,omitempty"`
	PausedAt string `json:"paused_at,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// pausedJobs reads every pause marker in one round trip. A job with no marker
// is absent from the result.
func (e *Engine) pausedJobs(ctx context.Context, jobs []string) (map[string]SchedulePauseState, error) {
	if len(jobs) == 0 {
		return map[string]SchedulePauseState{}, nil
	}
	var commands []string
	for _, job := range jobs {
		commands = append(commands,
			"printf '%s\\n' "+q("@@paused:"+job),
			"cat "+q(e.names().ScheduledJobPause(job))+" 2>/dev/null || true")
	}
	res, err := e.T.Run(ctx, strings.Join(commands, "\n"))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read scheduled-job pauses (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	out := map[string]SchedulePauseState{}
	name := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "@@paused:"); ok {
			name = after
			continue
		}
		if name == "" || line == "" {
			continue
		}
		state := out[name]
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "operator":
			state.Operator = value
		case "paused_at":
			state.PausedAt = value
		case "reason":
			state.Reason = value
		}
		out[name] = state
	}
	return out, nil
}
