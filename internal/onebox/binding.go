package onebox

import (
	"context"
	"fmt"

	"github.com/labstack/onebox/internal/engine"
)

// ResolveExecutionBinding resolves local authority without contacting the
// target. It is used to bind destructive confirmation to the exact project,
// environment, target, and source revision that Execute will recheck.
func (s *Service) ResolveExecutionBinding(ctx context.Context, kind OperationKind) (ExecutionBinding, error) {
	if !validOperationKind(kind) {
		return ExecutionBinding{}, fmt.Errorf("unknown operation kind %q", kind)
	}
	lenient := kind == KindProxyApply || kind == KindDestroy
	lp, err := s.loadProject(ctx, lenient)
	if err != nil {
		return ExecutionBinding{}, fmt.Errorf("load project: %w", err)
	}
	return s.executionBinding(lp)
}

func (s *Service) executionBinding(lp *loadedProject) (ExecutionBinding, error) {
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return ExecutionBinding{}, err
	}
	environment, err := lp.resolved.Environment(s.environment)
	if err != nil {
		return ExecutionBinding{}, err
	}
	return ExecutionBinding{
		Application: lp.resolved.App, Environment: s.environment, Target: environment.Target(),
		ConfigDigest: engine.HashBytes(lp.configBytes), ComposeDigest: engine.HashBytes(lp.composeBytes),
	}, nil
}

func (s *Service) verifyExecutionBinding(lp *loadedProject, expected *ExecutionBinding) error {
	if expected == nil {
		return nil
	}
	actual, err := s.executionBinding(lp)
	if err != nil {
		return err
	}
	if actual != *expected {
		return fmt.Errorf("execution binding changed after confirmation — resolve and confirm the operation again")
	}
	return nil
}
