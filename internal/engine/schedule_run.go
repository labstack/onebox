package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	// otherwise rewrite the file under it and misattribute the inputs.
	path := e.names().ScheduledJobRunInputs(name)
	create := "umask 077 && install -d -m 700 " + q(e.names().AppDir()+"/schedule") + " && set -C && cat > " + q(path)
	res, err := e.T.RunInput(ctx, create, scheduleInputsFile(operationID, inputs))
	if err != nil {
		return result, err
	}
	if res.ExitCode != 0 {
		return result, fmt.Errorf("a manual run of %s is already pending (%s exists); wait for it, or remove the file on the host", name, path)
	}

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
	result.Started = true
	if !wait {
		if res.ExitCode != 0 {
			return result, fmt.Errorf("systemctl start %s: %s", unit, strings.TrimSpace(res.Stderr))
		}
		e.logf("schedule: %s started as %s; ob schedule history %s shows the outcome", name, operationID, name)
		return result, nil
	}
	records, err := e.ScheduleHistory(ctx, name, 1)
	if err != nil {
		return result, err
	}
	if len(records) == 0 {
		return result, fmt.Errorf("job %s ran (systemctl exit %d) but left no run record; the host's notifier did not write one", name, res.ExitCode)
	}
	last := records[0]
	result.Record = &last
	exit := "-"
	if last.ExitStatus != nil {
		exit = fmt.Sprint(*last.ExitStatus)
	}
	e.logf("schedule: %s run %s %s after %d attempt(s) in %ds (exit %s)",
		name, last.Run, last.Outcome, last.Attempts, last.DurationSeconds, exit)
	// The operator asked for this run and waited for it, so anything but a
	// success is a failure of the request, a skip included: the unit exits 75
	// cleanly, but the work was not done.
	if last.Outcome != "success" {
		return result, fmt.Errorf("job %s run %s ended %s; see ob schedule logs %s --run %s", name, last.Run, last.Outcome, name, last.Run)
	}
	return result, nil
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
