package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/durable"
	"github.com/labstack/onebox/internal/journal"
)

type executionStep struct {
	Name       string            `json:"name"`
	Command    []string          `json:"command"`
	Inputs     map[string]string `json:"inputs"`
	Outputs    []string          `json:"outputs"`
	Attempts   int               `json:"attempts"`
	Backoff    int               `json:"backoff"`
	MaxBackoff int               `json:"max_backoff"`
}

// Invalidate before an effect, not after its result: a crash or failed migration
// can still have changed the schema. Hosts without durable records need no helper.
func invalidateExecutionCommand(root string) string {
	return "if [ -e " + q(durable.Store(root)) + " ] || [ -L " + q(durable.Store(root)) + " ]; then /usr/bin/python3 " + q(durable.Helper(root)) + " invalidate " + q(root) + "; fi"
}

func durableContainerCleanup(container string) string {
	return "if [ \"$(/usr/bin/docker inspect --format '{{ index .Config.Labels \"ob.execution.invocation\" }}' " + q(container) + " 2>/dev/null)\" = \"${INVOCATION_ID:-missing}\" ]; then " + scheduleContainerCleanup(container) + "; fi"
}

type executionDefinition struct {
	DeployLock       string            `json:"deploy_lock"`
	Notify           []string          `json:"notify"`
	Application      string            `json:"application"`
	Job              string            `json:"job"`
	Unit             string            `json:"unit"`
	Container        string            `json:"container"`
	Defaults         map[string]string `json:"defaults"`
	Steps            []executionStep   `json:"steps"`
	Services         []string          `json:"services"`
	EnvFiles         []string          `json:"env_files"`
	FingerprintFiles []string          `json:"fingerprint_files"`
	TimeoutSeconds   float64           `json:"timeout_seconds"`
	RetainSeconds    float64           `json:"retain_seconds"`
}

func (e *Engine) durableScheduleRunner(job app.ScheduledJob, envFiles []app.EnvFile) (string, error) {
	n := e.names()
	timeout, ok := app.ParseDuration(job.Timeout)
	if !ok {
		return "", fmt.Errorf("invalid durable job timeout")
	}
	retention := 7 * 24 * time.Hour
	if job.Execution.Retention != "" {
		retention, _ = app.ParseDuration(job.Execution.Retention)
	}
	definition := executionDefinition{
		DeployLock: job.DeployLock, Notify: job.Notify,
		Application: e.Spec.Name, Job: job.Name, Unit: n.ScheduledJobUnit(job.Name), Container: n.Container(job.Name, 1),
		Defaults: map[string]string{}, Steps: []executionStep{}, Services: []string{}, EnvFiles: []string{},
		TimeoutSeconds: timeout.Seconds(), RetainSeconds: retention.Seconds(),
	}
	for key, input := range job.Inputs {
		definition.Defaults[key] = input.Default
	}
	for _, file := range envFiles {
		if !file.Encrypted() {
			definition.EnvFiles = append(definition.EnvFiles, file.StagedPath())
		}
	}
	for _, file := range e.Spec.Workloads[job.Name].EnvFiles {
		if !file.Encrypted() {
			definition.FingerprintFiles = append(definition.FingerprintFiles, file.StagedPath())
		}
	}
	for _, name := range sortedNames(e.Spec.Services) {
		definition.Services = append(definition.Services, n.ServiceContainer(name))
	}
	steps := job.Execution.Steps
	if len(steps) == 0 {
		steps = []app.JobStep{{ID: "main"}}
	}
	for _, step := range steps {
		resolved := executionStep{Name: step.ID, Command: step.Command, Inputs: step.Inputs, Outputs: step.Outputs,
			Attempts: job.RetryAttempts, Backoff: app.RetryBackoffSeconds(job.RetryBackoff), MaxBackoff: app.RetryBackoffSeconds(job.RetryMaxBackoff)}
		if resolved.Command == nil {
			resolved.Command = []string{}
		}
		if resolved.Inputs == nil {
			resolved.Inputs = map[string]string{}
		}
		if resolved.Outputs == nil {
			resolved.Outputs = []string{}
		}
		if step.Retry != nil {
			// Resolve through the same defaults as ordinary schedule retries.
			copySpec := *e.Spec
			copyWorkload := e.Spec.Workloads[job.Name]
			copySchedule := *copyWorkload.Schedule
			copySchedule.Retry = step.Retry
			copyWorkload.Schedule = &copySchedule
			copySpec.Workloads = map[string]app.Workload{job.Name: copyWorkload}
			jobs, err := copySpec.ScheduledJobs()
			if err != nil {
				return "", err
			}
			resolved.Attempts, resolved.Backoff, resolved.MaxBackoff = jobs[0].RetryAttempts, app.RetryBackoffSeconds(jobs[0].RetryBackoff), app.RetryBackoffSeconds(jobs[0].RetryMaxBackoff)
		}
		definition.Steps = append(definition.Steps, resolved)
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return "", err
	}
	helper := "/usr/bin/python3 " + q(durable.Helper(n.AppDir()))
	lines := []string{"#!/bin/sh", "# Written by Onebox. Durable execution protocol v1.", "set -eu", "install -d -m 700 " + q(n.AppDir()+"/schedule")}
	lines = append(lines, scheduleInputsLines(n.ScheduledJobRunInputs(job.Name))...)
	lines = append(lines, scheduleLockLines(n, job.Name, job.DeployLock, e.lockPath(), e.lockTTL(), scheduleRendezvousWait(job.Timeout))...)
	lines = append(lines,
		"release_dir=$(readlink -f "+q(n.CurrentLink())+")",
		"release=${release_dir##*/}",
		"[ \"${release_dir%/*}\" = "+q(n.ReleasesDir())+" ] || exit 1",
		"exec 7>>\"$release_dir/.ob-schedule.lease\"", "chmod 600 \"$release_dir/.ob-schedule.lease\"", "/usr/bin/flock --shared 7")
	lines = append(lines, scheduleRunPreamble(true)...)
	lines = append(lines, "write_state 1",
		"execution=$("+helper+" prepare "+q(n.AppDir())+" "+q(base64.StdEncoding.EncodeToString(encoded))+" \"$release\" \"${INVOCATION_ID:-}\" \"$execution\" \"$operation\" \"{$inputs_json}\")")
	lines = append(lines, "printf 'execution=%s\\n' \"$execution\" >>\"$state\"")
	// Publish the durable reference while still inside the retention rendezvous.
	if job.DeployLock == "pinned" {
		lines = append(lines, "/usr/bin/flock --unlock 8")
	}
	lines = append(lines, helper+" run "+q(n.AppDir())+" \"$execution\" \"${INVOCATION_ID:-}\"", "")
	return strings.Join(lines, "\n"), nil
}

// ExecutionRecord is public non-secret evidence. The saved definition and step
// attempts remain available in JSON without being coupled to the current spec.
type ExecutionRecord map[string]any

func (e *Engine) ExecutionInspect(ctx context.Context, id string) (ExecutionRecord, error) {
	if !scheduleRunID.MatchString(id) {
		return nil, fmt.Errorf("invalid execution ID %q", id)
	}
	var record ExecutionRecord
	err := e.readExecutions(ctx, "inspect "+q(e.names().AppDir())+" "+q(id), &record)
	return record, err
}

func (e *Engine) ExecutionList(ctx context.Context, count int) ([]ExecutionRecord, error) {
	if count < 1 || count > 1000 {
		return nil, fmt.Errorf("execution count must be between 1 and 1000")
	}
	var records []ExecutionRecord
	err := e.readExecutions(ctx, fmt.Sprintf("list %s %d", q(e.names().AppDir()), count), &records)
	return records, err
}

func (e *Engine) readExecutions(ctx context.Context, args string, result any) error {
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	helper := durable.Helper(e.names().AppDir())
	command := "/usr/bin/python3 " + q(helper) + " " + args
	if strings.HasPrefix(args, "list ") {
		store := q(durable.Store(e.names().AppDir()))
		command = "if [ ! -e " + store + " ] && [ ! -L " + store + " ]; then printf '[]\\n'; else " + command + "; fi"
	}
	res, err := e.T.Run(ctx, command)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("read durable executions: %s", strings.TrimSpace(res.Stderr))
	}
	if err := json.Unmarshal([]byte(res.Stdout), result); err != nil {
		return fmt.Errorf("invalid execution evidence: %w", err)
	}
	return nil
}

func (e *Engine) ExecutionResume(ctx context.Context, operation, id string, wait bool) (ScheduleRunResult, error) {
	record, err := e.ExecutionInspect(ctx, id)
	if err != nil {
		return ScheduleRunResult{}, err
	}
	job, ok := record["job"].(string)
	if !ok {
		return ScheduleRunResult{}, fmt.Errorf("execution job evidence is missing")
	}
	if resumable, _ := record["resumable"].(bool); !resumable {
		return ScheduleRunResult{}, fmt.Errorf("execution is active, terminal, or expired")
	}
	if same, _ := record["current_release_matches"].(bool); !same {
		return ScheduleRunResult{}, fmt.Errorf("resume requires the original current release")
	}
	// Host runner rechecks the saved definition and compatibility under locks.
	return e.scheduleRun(ctx, operation, job, nil, wait, id)
}

func (e *Engine) ExecutionAbandon(ctx context.Context, operation, id string) (err error) {
	if !scheduleRunID.MatchString(id) {
		return fmt.Errorf("invalid execution ID %q", id)
	}
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	epoch, err := e.AcquireLock(ctx, operation, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, operation, epoch); err != nil {
		return err
	}
	writer := &journal.Writer{T: e.T, Names: e.names(), DeployID: operation, Epoch: epoch,
		Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner}
	record := journal.Record{Phase: "execution-abandon", Event: "start", Status: "ok", Target: id, TargetKind: "job"}
	if err := writer.Append(ctx, record); err != nil {
		return err
	}
	defer func() {
		record.Event = "finish"
		if err != nil {
			record.Status = "fail"
		}
		if appendErr := writer.Append(ctx, record); appendErr != nil && err == nil {
			err = appendErr
		}
	}()
	res, err := e.mutate(ctx, "/usr/bin/python3 "+q(durable.Helper(e.names().AppDir()))+" abandon "+q(e.names().AppDir())+" "+q(id))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("abandon execution: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}
