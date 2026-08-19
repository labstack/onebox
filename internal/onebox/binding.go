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
	lenient := operationUsesInspectionRuntime(kind)
	lp, err := s.loadProject(ctx, lenient)
	if err != nil {
		return ExecutionBinding{}, fmt.Errorf("load project: %w", err)
	}
	return s.executionBinding(lp)
}

// operationUsesInspectionRuntime identifies mutations that need the contract's
// shape but never execute its locally rendered application runtime. Their local
// view may therefore carry fail-closed placeholder images; recovery operations
// replay the immutable runtime already stored on the host. Deploy and job
// execution are deliberately absent: anything they can stage must have real
// release images.
func operationUsesInspectionRuntime(kind OperationKind) bool {
	switch kind {
	case KindResume, KindAbort, KindRollback, KindBootstrap, KindServiceApply,
		KindProxyApply, KindSecretsPush, KindDestroy,
		// Protection operates on a service's data, never on the application's
		// release images, so a placeholder image must not stop a backup.
		KindProtectionEnable, KindBackupCreate:
		return true
	default:
		return false
	}
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
		Application: lp.resolved.Name, Environment: s.environment, Server: environment.Destination(),
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
