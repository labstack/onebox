package onebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/secrets"
)

// Execute is the canonical local mutation boundary. It deliberately does not
// accept an approval token yet; M1 adds that binding before this method is
// exposed over MCP. The CLI is trusted local operator input in M0.
func (s *Service) Execute(ctx context.Context, request ExecuteRequest) (OperationResult, error) {
	started := s.now().UTC()
	if request.Kind == "" && request.Plan != nil {
		request.Kind = request.Plan.Operation.Kind
	}
	result := OperationResult{
		Kind: request.Kind, Status: "running",
		StartedAt: started.Format(time.RFC3339Nano),
	}
	if request.Plan != nil && request.Plan.Operation.ID != "" {
		result.ID = request.Plan.Operation.ID
	} else {
		identityKind := request.Kind
		if !validOperationKind(identityKind) {
			identityKind = OperationKind("operation")
		}
		result.ID = s.newOperationID(started, gitShortSHA(ctx, filepath.Dir(s.configPath)), identityKind)
	}
	sequence := 0
	emit := func(phase, status, message string) {
		if request.Events == nil {
			return
		}
		sequence++
		request.Events(OperationEvent{
			SchemaVersion: OperationEventSchemaVersion,
			OperationID:   result.ID, EvidenceID: result.EvidenceID, Sequence: sequence,
			Time: s.now().UTC().Format(time.RFC3339Nano), Kind: request.Kind,
			Phase: phase, Status: status, Message: message,
		})
	}
	finish := func(err error) (OperationResult, error) {
		result.FinishedAt = s.now().UTC().Format(time.RFC3339Nano)
		if err != nil {
			result.Status = "failed"
			// Detailed engine errors remain on the trusted local return path and in
			// journals. Structured events are safe for future MCP/dashboard sinks.
			emit("operation", "failed", "operation failed; inspect local output and journal evidence")
			return result, err
		}
		if result.NoOp {
			result.Status = "no_op"
		} else {
			result.Status = "succeeded"
		}
		emit("operation", result.Status, "")
		return result, nil
	}

	if !validOperationKind(request.Kind) {
		return finish(fmt.Errorf("unknown operation kind %q", request.Kind))
	}
	if request.Kind == KindDeploy {
		if request.Plan == nil {
			return finish(errors.New("deploy requires an executable plan"))
		}
		result.ReleaseID = request.Plan.Operation.ReleaseID
		emit("operation", "started", "")
		noOp, err := s.executeDeploy(ctx, request, emit, func() {
			result.EvidenceID = request.Plan.Operation.ReleaseID
		})
		result.NoOp = noOp
		return finish(err)
	}

	lenient := request.Kind == KindProxyApply || request.Kind == KindDestroy
	lp, err := loadProject(ctx, s.configPath, lenient)
	if err != nil {
		return finish(fmt.Errorf("load project: %w", err))
	}
	if err := ensureEnvironment(lp.config, s.environment); err != nil {
		return finish(err)
	}
	if err := s.verifyExecutionBinding(lp, request.ExpectedBinding); err != nil {
		return finish(err)
	}
	if request.Kind == KindRollback {
		if errs := compose.CheckRollable(lp.project, lp.config); len(errs) > 0 {
			return finish(fmt.Errorf("not rollable: %v", errs))
		}
	}
	operationID := result.ID
	emit("operation", "started", "")
	e, cleanup, _, err := s.engineWith(ctx, lp, s.environment, func(options *engine.Options) {
		options.ForceLock = request.Force
		options.NoRollback = request.NoRollback
	})
	if err != nil {
		return finish(fmt.Errorf("connect target: %w", err))
	}
	defer cleanup()

	emit("execute", "started", "")
	switch request.Kind {
	case KindResume:
		result.EvidenceID, err = e.ResumeWithJournalID(ctx)
	case KindAbort:
		result.EvidenceID, err = e.AbortWithJournalID(ctx, request.Force)
	case KindRollback:
		result.EvidenceID, err = e.RollbackWithJournalID(ctx)
		result.ReleaseID = result.EvidenceID
	case KindBootstrap:
		var staging string
		var cleanupStaging func()
		staging, cleanupStaging, err = stageExecution(ctx, lp, operationID)
		if err == nil {
			defer cleanupStaging()
			result.ReleaseID = operationID
			result.EvidenceID = operationID
			err = e.Bootstrap(ctx, operationID, staging)
		}
	case KindServiceApply:
		var staging string
		var cleanupStaging func()
		staging, cleanupStaging, err = stageExecution(ctx, lp, operationID)
		if err == nil {
			defer cleanupStaging()
			result.ReleaseID = operationID
			result.EvidenceID = operationID
			err = e.AccessoryApply(ctx, operationID, staging, request.Force)
		}
	case KindProxyApply:
		result.EvidenceID = operationID
		err = e.ProxyApply(ctx, operationID, request.Force)
	case KindSecretsPush:
		if lp.config.Secrets == nil {
			err = errors.New("no secrets.sops source declared")
			break
		}
		var envBytes []byte
		envBytes, err = secrets.RenderContext(ctx, filepath.Dir(lp.configPath), lp.config.Secrets.Sops)
		if err == nil {
			result.EvidenceID, err = e.SecretsPushWithJournalID(ctx, envBytes)
		}
	case KindDestroy:
		err = e.Destroy(ctx, request.RemoveVolumes, request.RemoveProxy)
	}
	if err == nil {
		emit("execute", "succeeded", "")
	}
	return finish(err)
}

func (s *Service) executeDeploy(
	ctx context.Context,
	request ExecuteRequest,
	emit func(string, string, string),
	markJournal func(),
) (bool, error) {
	plan := request.Plan
	if err := plan.Validate(); err != nil {
		return false, fmt.Errorf("validate executable deploy plan: %w", err)
	}
	createdAt, err := parseOperationTime(plan.Operation.CreatedAt, "created_at")
	if err != nil {
		return false, err
	}
	expiresAt, err := parseOperationTime(plan.Operation.ExpiresAt, "expires_at")
	if err != nil {
		return false, err
	}
	now := s.now().UTC()
	if expiresAt.Before(now) {
		return false, fmt.Errorf("deployment plan expired at %s — re-plan", expiresAt.Format(time.RFC3339))
	}
	if createdAt.After(now.Add(time.Minute)) {
		return false, errors.New("deployment plan was created in the future — check the runner clock and re-plan")
	}
	emit("binding", "started", "")
	lp, err := loadProject(ctx, s.configPath, false)
	if err != nil {
		return false, fmt.Errorf("load project: %w", err)
	}
	if err := ensureEnvironment(lp.config, s.environment); err != nil {
		return false, err
	}
	if err := lp.config.RunPreflight(filepath.Dir(lp.configPath)); err != nil {
		return false, err
	}
	if errs := compose.CheckRollable(lp.project, lp.config); len(errs) > 0 {
		return false, fmt.Errorf("not rollable: %v", errs)
	}
	expectedGraph, err := DeploymentGraph(lp.config, plan.Operation.ReleaseID)
	if err != nil {
		return false, fmt.Errorf("build expected operation graph: %w", err)
	}
	if !reflect.DeepEqual(plan.Operation.Steps, expectedGraph) {
		return false, errors.New("operation graph differs from the resolved configuration — re-plan")
	}
	expectedRisk, expectedReversibility, expectedApproval := classifyDeployment(expectedGraph, plan.Artifact.HostState.CurrentRelease)
	if plan.Operation.Risk != expectedRisk ||
		plan.Operation.Reversibility != expectedReversibility ||
		plan.Operation.Approval != expectedApproval {
		return false, errors.New("operation risk classification differs from the resolved configuration — re-plan")
	}
	binding := plan.Operation.Binding
	if lp.config.App != binding.Application {
		return false, fmt.Errorf("plan application is %q, local application is %q — re-plan", binding.Application, lp.config.App)
	}
	if s.environment != binding.Environment {
		return false, fmt.Errorf("plan environment is %q, executing %q — re-plan", binding.Environment, s.environment)
	}
	if engine.HashBytes(lp.configBytes) != binding.ConfigDigest {
		return false, errors.New("configuration changed since plan — re-plan")
	}
	if engine.HashBytes(lp.composeBytes) != binding.ComposeDigest {
		return false, errors.New("Compose file changed since plan — re-plan")
	}
	applyPinnedImages(lp.project, plan.Artifact.PinnedImages)
	e, cleanup, target, err := s.engineWith(ctx, lp, s.environment, func(options *engine.Options) {
		options.ForceLock = request.Force
		options.NoRollback = request.NoRollback
		options.DeployPrecondition = func(preconditionContext context.Context, locked *engine.Engine) error {
			if expiresAt.Before(s.now().UTC()) {
				return errors.New("deployment plan expired before mutation — re-plan")
			}
			return verifyRemoteDeployBinding(preconditionContext, locked, plan, s.environment, lp.configBytes, lp.config.App)
		}
	})
	if err != nil {
		return false, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()
	if target != binding.Target {
		return false, fmt.Errorf("target changed from %q to %q — re-plan", binding.Target, target)
	}
	if err := verifyRemoteDeployBinding(ctx, e, plan, s.environment, lp.configBytes, lp.config.App); err != nil {
		return false, err
	}
	emit("binding", "succeeded", "")

	emit("stage", "started", "")
	staging, cleanupStaging, err := stageExecution(ctx, lp, plan.Operation.ReleaseID)
	if err != nil {
		return false, err
	}
	defer cleanupStaging()
	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return false, fmt.Errorf("read staged compose: %w", err)
	}
	renderedRedacted, err := compose.RedactEnvYAML(rendered)
	if err != nil {
		return false, fmt.Errorf("redact staged compose: %w", err)
	}
	if strings.TrimSpace(string(renderedRedacted)) != strings.TrimSpace(plan.Artifact.RenderedCompose) {
		return false, errors.New("rendered Compose differs from the plan — re-plan")
	}
	payloadDigest, err := engine.LocalPayloadDigestContext(ctx, staging)
	if err != nil {
		return false, fmt.Errorf("hash staged payload: %w", err)
	}
	if payloadDigest != binding.PayloadDigest {
		return false, errors.New("release payload differs from the plan — re-plan")
	}
	emit("stage", "succeeded", "")
	if plan.NoOp && !request.Redeploy {
		if err := e.ValidateDeployNoOp(ctx, plan.Operation.ID); err != nil {
			return false, err
		}
		return true, nil
	}

	emit("execute", "started", "")
	markJournal()
	if err := e.Deploy(ctx, plan.Operation.ReleaseID, staging); err != nil {
		return false, err
	}
	emit("execute", "succeeded", "")
	return false, nil
}

func verifyRemoteDeployBinding(ctx context.Context, e *engine.Engine, plan *DeployPlan, environment string, configBytes []byte, app string) error {
	fresh, err := e.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	if err := plan.Artifact.VerifyBinding(environment, configBytes, fresh); err != nil {
		return err
	}
	binding := plan.Operation.Binding
	_, liveComposeDigest, err := readLiveComposeState(ctx, e, app, fresh.CurrentRelease)
	if err != nil {
		return err
	}
	if fresh.CurrentRelease == "" {
		return nil
	}
	if liveComposeDigest != binding.LiveComposeDigest {
		return errors.New("live Compose changed since plan — re-plan")
	}
	livePayloadDigest, err := e.RemotePayloadDigest(ctx, fresh.CurrentRelease)
	if err != nil {
		return fmt.Errorf("hash live payload: %w", err)
	}
	if livePayloadDigest != binding.LivePayloadDigest {
		return errors.New("live payload changed since plan — re-plan")
	}
	return nil
}
