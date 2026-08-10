package onebox

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/engine"
)

// ExecRequest is an intentionally small escape-hatch contract. It does not
// claim convergence, rollback, idempotence, or output redaction.
type ExecRequest struct {
	Target  string
	Command string
	Reason  string
}

type ExecResult struct {
	OperationID   string               `json:"operation_id"`
	Target        engine.RuntimeTarget `json:"target"`
	CommandDigest string               `json:"command_digest"`
	Reason        string               `json:"reason"`
	StartedAt     string               `json:"started_at"`
	FinishedAt    string               `json:"finished_at"`
	Outcome       string               `json:"outcome"`
}

// Exec runs arbitrary container commands through the canonical service
// boundary while persisting only safe invocation evidence.
func (s *Service) Exec(ctx context.Context, request ExecRequest, stdout, stderr io.Writer) (result ExecResult, err error) {
	started := s.now().UTC()
	result.StartedAt = started.Format(time.RFC3339Nano)
	result.Outcome = "failed"
	defer func() {
		result.FinishedAt = s.now().UTC().Format(time.RFC3339Nano)
		if err == nil {
			result.Outcome = "succeeded"
		} else if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.Outcome = "cancelled"
		}
	}()

	if err = engine.ValidateExecReason(request.Reason); err != nil {
		return result, err
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if strings.TrimSpace(request.Target) == "" || strings.TrimSpace(request.Command) == "" {
		return result, errors.New("exec target and command are required")
	}
	result.CommandDigest = engine.HashBytes([]byte(request.Command))
	result.Reason = request.Reason
	result.OperationID = s.newOperationID(started, gitShortSHA(ctx, filepath.Dir(s.configPath)), OperationKind("exec"))

	lp, err := s.loadProject(ctx, true)
	if err != nil {
		return result, err
	}
	if err = ensureEnvironment(lp.resolved, s.environment); err != nil {
		return result, err
	}
	environmentConfig, err := lp.resolved.Environment(s.environment)
	if err != nil {
		return result, err
	}
	if err = enforceRunnerPolicy(environmentConfig.Policy, s.runner, ExecutableDeployPlanSchemaVersion); err != nil {
		return result, err
	}
	e, cleanup, _, err := s.engine(ctx, lp, s.environment)
	if err != nil {
		return result, err
	}
	defer cleanup()
	result.Target, err = e.ResolveRuntimeTarget(request.Target)
	if err != nil {
		return result, err
	}
	err = e.ExecInAudited(ctx, result.OperationID, request.Target, request.Command, request.Reason, stdout, stderr)
	return result, err
}
