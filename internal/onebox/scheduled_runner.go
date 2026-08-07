package onebox

import (
	"context"
	"errors"
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
	if err := envelope.ValidateForRunner(compatibility, observedState, runner.Now()); err != nil {
		return err
	}
	steps, err := LifecycleOperationGraph(envelope.Operation, LifecycleScheduledRunnerSchema, envelope.Service)
	if err != nil {
		return err
	}
	return runner.Executor.ExecuteScheduledLifecycle(ctx, ScheduledLifecycleExecution{Envelope: envelope, Steps: steps})
}
