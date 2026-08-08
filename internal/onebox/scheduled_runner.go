package onebox

import (
	"context"
	"errors"
	"io"
	"time"
)

type ScheduledLifecycleExecution struct {
	Envelope ScheduledOperationEnvelope
	Steps    []OperationStep
}

type ScheduledLifecycleExecutor interface {
	ExecuteScheduledLifecycle(context.Context, ScheduledLifecycleExecution) error
}

type ScheduledRunner struct {
	Compatibility ScheduledRunnerCompatibility
	Now           func() time.Time
	ObserveState  func(string) (string, error)
	Executor      ScheduledLifecycleExecutor
}

// ExecuteRecurring materializes one occurrence from an installed schedule
// template, then executes exactly that occurrence. Callers must invoke this
// once per timer firing; retries inside the firing retain the materialized
// identity, while later timer activations cannot be deduplicated against it.
func (runner ScheduledRunner) ExecuteRecurring(ctx context.Context, template ScheduledOperationEnvelope, entropy io.Reader) error {
	if runner.Now == nil {
		runner.Now = time.Now
	}
	now := runner.Now().UTC()
	occurrence, err := MaterializeScheduledOccurrence(template, now, entropy)
	if err != nil {
		return err
	}
	runner.Now = func() time.Time { return now }
	return runner.Execute(ctx, occurrence)
}

func (runner ScheduledRunner) Execute(ctx context.Context, envelope ScheduledOperationEnvelope) error {
	compatibility := runner.Compatibility
	if compatibility.RunnerProtocol == 0 {
		compatibility = CurrentScheduledRunnerCompatibility()
	}
	if runner.Now == nil {
		runner.Now = time.Now
	}
	if runner.ObserveState == nil {
		runner.ObserveState = ReadScheduledStateDigest
	}
	if runner.Executor == nil {
		return errors.New("scheduled lifecycle executor is unavailable")
	}
	observedState, err := runner.ObserveState(envelope.State.Path)
	if err != nil {
		return err
	}
	now := runner.Now().UTC()
	if err := envelope.ValidateForRunner(compatibility, observedState, now); err != nil {
		return err
	}
	steps, err := LifecycleOperationGraph(envelope.Operation, LifecycleScheduledRunnerSchema, envelope.Service)
	if err != nil {
		return err
	}
	maxRuntime, _ := time.ParseDuration(envelope.Timing.MaxRuntime)
	expiresAt, _ := time.Parse(time.RFC3339Nano, envelope.Timing.ExpiresAt)
	deadline := now.Add(maxRuntime)
	if expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	executionContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return runner.Executor.ExecuteScheduledLifecycle(executionContext, ScheduledLifecycleExecution{Envelope: envelope, Steps: steps})
}
