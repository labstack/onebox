package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
)

// ScheduleRunResult is what an operator-initiated run leaves on the
// workstation side. The outcome lives on the host: it is the run record, and
// it is returned here only when the caller waited for it.
type ScheduleRunResult struct {
	Job       string             `json:"job"`
	Unit      string             `json:"unit"`
	Operation string             `json:"operation"`
	Inputs    map[string]string  `json:"inputs,omitempty"`
	Started   bool               `json:"started"`
	Record    *ScheduleRunRecord `json:"record,omitempty"`
}

// ScheduleRun starts a scheduled job's unit now with declared, validated
// inputs. It is journaled like every operation, but the application lock is
// released before the unit starts: the runner exits 75 when it sees that
// lock, so holding it across the start would skip the very run requested.
//
// Only a job whose data effect is none may run this way. The timer already
// runs any scheduled job unattended, but an operator choosing the moment and
// the inputs is the case the sealed plan of `ob job run` exists for, and a
// migration or destructive job keeps that path.
func (e *Engine) ScheduleRun(ctx context.Context, operationID, name string, inputs map[string]string, wait bool) (ScheduleRunResult, error) {
	result := ScheduleRunResult{Job: name, Operation: operationID, Inputs: inputs}
	if strings.TrimSpace(operationID) == "" {
		return result, errors.New("schedule run requires an operation id")
	}
	if err := e.RequireHostOwner(ctx); err != nil {
		return result, err
	}
	if _, err := e.scheduledJob(name); err != nil {
		return result, err
	}
	workload := e.Spec.Workloads[name]
	if workload.DataEffect != app.DataEffectNone {
		return result, fmt.Errorf("job %s declares data_effect %q; operator-initiated runs of it go through the sealed plan: ob job plan %s, then ob job run",
			name, workload.DataEffect, name)
	}
	if err := app.ValidateJobInputValues(workload, inputs); err != nil {
		return result, err
	}
	unit := e.names().ScheduledJobUnit(name)
	result.Unit = unit

	// Starting an active unit is a no-op to systemd and would consume the
	// inputs file for a run that never happens; say so instead.
	active, err := e.T.Run(ctx, "systemctl is-active "+q(unit+".service")+" 2>/dev/null || true")
	if err != nil {
		return result, err
	}
	switch state := strings.TrimSpace(active.Stdout); state {
	case "active", "activating", "deactivating":
		return result, fmt.Errorf("job %s is running (%s); wait for it, or read ob schedule history %s", name, state, name)
	}

	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return result, err
	}
	locked := true
	defer func() {
		if locked {
			e.ReleaseLock(ctx)
		}
	}()
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return result, err
	}

	// noclobber: a second manual run before the first is consumed would
	// otherwise rewrite the file under it and misattribute the inputs. The
	// existence check in front gives that case its own exit status, so a
	// host that simply refuses the write is reported as that and not as a
	// pending run nobody can find.
	path := e.names().ScheduledJobRunInputs(name)
	create := "if [ -e " + q(path) + " ]; then exit 73; fi; " +
		"umask 077 && install -d -m 700 " + q(e.names().AppDir()+"/schedule") + " && set -C && cat > " + q(path)
	res, err := e.T.RunInput(ctx, create, scheduleInputsFile(operationID, inputs))
	if err != nil {
		return result, err
	}
	switch {
	case res.ExitCode == 73:
		return result, fmt.Errorf("a manual run of %s is already pending (%s exists); wait for it, or remove the file on the host", name, path)
	case res.ExitCode != 0:
		return result, fmt.Errorf("cannot write the inputs file %s on the host: %s", path, strings.TrimSpace(res.Stderr))
	}
	// From here on the file is ours to clean up: a request that fails before
	// the unit starts must not leave it behind to refuse the next one.
	pending := true
	defer func() {
		if pending {
			e.discardInputs(ctx, path)
		}
	}()

	writer := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: operationID, Epoch: epoch, Operator: journal.DefaultOperator(),
		GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner,
	}
	detail := "inputs: defaults"
	if len(inputs) > 0 {
		detail = "inputs: " + scheduleInputsDetail(inputs)
	}
	record := journal.Record{Phase: "schedule-run", Event: "start", Status: "ok", Target: name, TargetKind: "job", Detail: detail}
	if err := writer.Append(ctx, record); err != nil {
		return result, fmt.Errorf("journal schedule run start: %w", err)
	}
	// The finish is written now, under the lock, because the outcome does not
	// belong to this operation: it is the run record on the host, joined to
	// this journal entry by the operation id the inputs file carries.
	record.Event, record.Detail = "finish", "unit started; outcome in ob schedule history "+name
	if err := writer.Append(ctx, record); err != nil {
		return result, fmt.Errorf("journal schedule run finish: %w", err)
	}
	e.ReleaseLock(ctx)
	locked = false

	start := "systemctl start --no-block " + q(unit+".service")
	if wait {
		start = "systemctl start " + q(unit+".service")
	}
	res, err = e.mutate(ctx, start)
	if err != nil {
		return result, err
	}
	if res.ExitCode != 0 && !wait {
		return result, fmt.Errorf("systemctl start %s: %s", unit, strings.TrimSpace(res.Stderr))
	}
	// The unit was activated, so the runner owns the file now, whether it ran
	// or skipped; a blocking start that exits non-zero still activated it.
	pending = false
	result.Started = true
	if !wait {
		e.logf("schedule: %s started as %s; ob schedule history %s shows the outcome", name, operationID, name)
		return result, nil
	}
	last, err := e.awaitScheduleRecord(ctx, name, operationID)
	if err != nil {
		if res.ExitCode != 0 {
			return result, fmt.Errorf("%w; systemctl start exited %d: %s", err, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		return result, err
	}
	result.Record = last
	exit := "-"
	if last.ExitStatus != nil {
		exit = fmt.Sprint(*last.ExitStatus)
	}
	e.logf("schedule: %s run %s %s after %d attempt(s) in %ds (exit %s)",
		name, last.Run, last.Outcome, last.Attempts, last.DurationSeconds, exit)
	// The operator asked for this run and waited for it, so anything but a
	// success is a failure of the request, a skip included: the unit exits
	// cleanly, but the work was not done.
	if last.Outcome != "success" {
		return result, fmt.Errorf("job %s run %s ended %s; see ob schedule logs %s --run %s", name, last.Run, last.Outcome, name, last.Run)
	}
	return result, nil
}

// awaitScheduleRecord finds the record of this operation's run. The notifier
// writes it from ExecStopPost and journald ingests it a moment after the
// blocking start returns, so a few short retries stand between the start and
// the read. Matching on the operation id means a record left by an earlier
// run, or by a timer firing that took this slot, is never reported as ours.
func (e *Engine) awaitScheduleRecord(ctx context.Context, name, operationID string) (*ScheduleRunRecord, error) {
	for attempt := range 10 {
		if attempt > 0 {
			e.Opts.Sleep(200 * time.Millisecond)
		}
		records, err := e.ScheduleHistory(ctx, name, 5)
		if err != nil {
			return nil, err
		}
		for i := range records {
			if records[i].Operation == operationID {
				return &records[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no run record carries operation %s for job %s: the unit did not run for this request; a timer firing may have taken the slot, or the host's notifier wrote nothing", operationID, name)
}

// discardInputs removes a pending inputs file this request wrote and can no
// longer hand to a run. Best effort: the file is root-only state on the host,
// and the error the caller is already returning is the one that matters.
func (e *Engine) discardInputs(ctx context.Context, path string) {
	if _, err := e.T.Run(ctx, "rm -f "+q(path)); err != nil {
		e.warnf("could not remove the pending inputs file %s: %v", path, err)
	}
}

// scheduleInputsFile is the one-shot file the runner consumes: the operation
// id on its reserved line, then one declared override per line. Values were
// validated against a charset that has no newline or quote, so the format
// needs no escaping.
func scheduleInputsFile(operationID string, inputs map[string]string) string {
	lines := []string{app.ReservedInputPrefix + "OPERATION=" + operationID}
	for _, name := range sortedInputNames(inputs) {
		lines = append(lines, name+"="+inputs[name])
	}
	return strings.Join(lines, "\n") + "\n"
}

func scheduleInputsDetail(inputs map[string]string) string {
	parts := make([]string, 0, len(inputs))
	for _, name := range sortedInputNames(inputs) {
		parts = append(parts, name+"="+inputs[name])
	}
	return strings.Join(parts, ",")
}

func sortedInputNames(inputs map[string]string) []string {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
