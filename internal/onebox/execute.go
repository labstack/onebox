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
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/secrets"
)

// Execute is the canonical local mutation boundary. Deploy approvals are
// checked here, after the local plan binding is re-derived and before any
// connection or write-capable transport operation.
func (s *Service) Execute(ctx context.Context, request ExecuteRequest) (OperationResult, error) {
	started := s.now().UTC()
	if request.Kind == "" && request.Plan != nil {
		request.Kind = request.Plan.Operation.Kind
	}
	if request.Kind == "" && request.JobPlan != nil {
		request.Kind = request.JobPlan.Operation.Kind
	}
	result := OperationResult{
		Kind: request.Kind, Status: OperationStatusRunning,
		StartedAt: started.Format(time.RFC3339Nano), Runner: s.runner,
	}
	if request.Approval != nil {
		result.ApprovalDigest = request.Approval.ApprovalDigest
	}
	if request.MigrationBackupOverride != nil {
		result.MigrationBackupOverrideDigest = request.MigrationBackupOverride.OverrideDigest
	}
	if request.Plan != nil && request.Plan.Operation.ID != "" {
		result.ID = request.Plan.Operation.ID
	} else if request.JobPlan != nil && request.JobPlan.Operation.ID != "" {
		result.ID = request.JobPlan.Operation.ID
	} else {
		identityKind := request.Kind
		if !validOperationKind(identityKind) {
			identityKind = OperationKind("operation")
		}
		result.ID = s.newOperationID(started, gitShortSHA(ctx, filepath.Dir(s.configPath)), identityKind)
	}
	sequence := 0
	emitCanonical := func(phase string, status OperationStatus, message string) {
		if request.Events == nil {
			return
		}
		sequence++
		request.Events(OperationEvent{
			SchemaVersion: OperationEventSchemaVersion,
			OperationID:   result.ID, EvidenceID: result.EvidenceID, Sequence: sequence,
			Time: s.now().UTC().Format(time.RFC3339Nano), Kind: request.Kind,
			Phase: phase, Status: status, Message: message,
			Runner: s.runner,
		})
	}
	// Engine progress strings are an internal vocabulary. Public non-terminal
	// records always use running; only finish below can emit a terminal status.
	emitProgress := func(phase, _ string, message string) {
		emitCanonical(phase, OperationStatusRunning, message)
	}
	finish := func(err error) (OperationResult, error) {
		result.FinishedAt = s.now().UTC().Format(time.RFC3339Nano)
		if err != nil {
			result.Status = OperationStatusError
			// Detailed engine errors remain on the trusted local return path and in
			// journals. Structured events are safe for future MCP/dashboard sinks.
			emitCanonical("operation", OperationStatusError, "operation failed; inspect local output and journal evidence")
			return result, err
		}
		if result.NoOp {
			result.Status = OperationStatusNoOp
		} else {
			result.Status = OperationStatusSuccess
		}
		emitCanonical("operation", result.Status, "")
		return result, nil
	}
	if err := request.Validate(); err != nil {
		return finish(err)
	}
	if request.BackupReport != nil {
		digest, err := request.BackupReport.ComputeDigest()
		if err != nil {
			return finish(fmt.Errorf("compute backup report digest: %w", err))
		}
		result.BackupReportDigest = digest
	}

	if !validOperationKind(request.Kind) {
		return finish(fmt.Errorf("unknown operation kind %q", request.Kind))
	}
	if request.Kind == KindDeploy {
		if request.Plan == nil {
			return finish(errors.New("deploy requires an executable plan"))
		}
		result.ReleaseID = request.Plan.Operation.ReleaseID
		emitProgress("operation", "started", "")
		noOp, err := s.executeDeploy(ctx, request, emitProgress, func() {
			result.EvidenceID = request.Plan.Operation.ReleaseID
		})
		result.NoOp = noOp
		return finish(err)
	}
	if request.Kind == KindJobRun {
		if request.JobPlan == nil {
			return finish(errors.New("job run requires an executable job plan"))
		}
		result.ReleaseID = request.JobPlan.Operation.ReleaseID
		emitProgress("operation", "started", "")
		evidenceID, jobResult, jobErr := s.executeJob(ctx, request, emitProgress)
		result.EvidenceID = evidenceID
		result.JobResult = jobResult
		return finish(jobErr)
	}

	lenient := operationUsesInspectionRuntime(request.Kind)
	lp, err := s.loadProject(ctx, lenient)
	if err != nil {
		return finish(fmt.Errorf("load project: %w", err))
	}
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return finish(err)
	}
	environmentConfig, err := lp.resolved.Environment(s.environment)
	if err != nil {
		return finish(err)
	}
	if err := enforceRunnerPolicy(environmentConfig.Policy, s.runner, ExecutableDeployPlanSchemaVersion); err != nil {
		return finish(err)
	}
	if err := s.verifyExecutionBinding(lp, request.ExpectedBinding); err != nil {
		return finish(err)
	}
	operationID := result.ID
	emitProgress("operation", "started", "")
	e, cleanup, _, err := s.engineWith(ctx, lp, s.environment, func(options *engine.Options) {
		options.ForceLock = request.BreakLock
		options.NoRollback = request.NoRollback
		options.Progress = emitProgress
	})
	if err != nil {
		return finish(fmt.Errorf("connect target: %w", err))
	}
	defer cleanup()

	emitProgress("execute", "started", "")
	switch request.Kind {
	case KindResume:
		result.EvidenceID, err = e.ResumeWithJournalID(ctx)
	case KindAbort:
		result.EvidenceID, err = e.AbortWithJournalID(ctx, request.BreakMigrationGate)
	case KindRollback:
		result.EvidenceID, err = e.RollbackWithJournalID(ctx)
		result.ReleaseID = result.EvidenceID
	case KindBootstrap:
		result.ReleaseID = operationID
		result.EvidenceID = operationID
		err = e.Bootstrap(ctx, operationID)
	case KindServiceApply:
		// Services are not staged into a release: they are their own Compose
		// projects, and nothing a release can remove.
		result.ReleaseID = operationID
		result.EvidenceID = operationID
		err = e.ServiceApply(ctx, operationID, request.AllowDestructiveMounts)
	case KindProtectionEnable:
		// Enablement restarts the service under the protected image and does
		// not finish until the first base backup exists, because WAL archiving
		// with nothing to replay onto can recover nothing.
		result.EvidenceID = operationID
		err = executeProtectionEnable(ctx, e, lp.resolved, lp.configPath, s.environment, request.Service, operationID)
	case KindBackupCreate:
		result.EvidenceID = operationID
		err = underProtectionLocks(ctx, e, request.Service, operationID, func(ctx context.Context) error {
			return e.BackupService(ctx, request.Service)
		})
	case KindProtectionDisable:
		result.EvidenceID = operationID
		err = executeProtectionDisable(ctx, e, lp.resolved, s.environment, request.Service, operationID)
	case KindBackupPrune:
		result.EvidenceID = operationID
		err = underProtectionLocks(ctx, e, request.Service, operationID, func(ctx context.Context) error {
			return e.PruneServiceBackups(ctx, request.Service)
		})
	case KindAssuranceCheck:
		result.EvidenceID = operationID
		err = underProtectionLocks(ctx, e, request.Service, operationID, func(ctx context.Context) error {
			return e.VerifyServiceArchive(ctx, request.Service)
		})
	case KindRestoreTest, KindRestoreCutover:
		// One path, two endings. A drill stops after proving the recovered
		// cluster answers; a restore goes on to put it in service.
		result.EvidenceID = operationID
		err = executeRecovery(ctx, e, request.Service, request.RecoveryTarget,
			request.Kind == KindRestoreCutover, operationID)
	case KindProxyApply:
		result.EvidenceID = operationID
		err = e.ProxyApply(ctx, operationID)
	case KindSecretsPush:
		entries := encryptedEntries(lp.resolved)
		externalProjections := externalConnectionProjections(lp.resolved)
		if len(entries) == 0 && len(externalProjections) == 0 {
			err = errors.New("no encrypted env_files entry declared")
			break
		}
		// Render the complete graph before entering the engine. The engine then
		// compares, checkpoints and replaces it as one generation-wide unit.
		payloads := make([]engine.SecretPayload, 0, len(entries)+len(externalProjections))
		for _, entry := range entries {
			var envBytes []byte
			envBytes, err = secrets.RenderContext(ctx, filepath.Dir(lp.configPath), entry.File)
			if err != nil {
				break
			}
			payloads = append(payloads, engine.SecretPayload{Path: entry.StagedPath(), Bytes: envBytes})
		}
		decryptedSources := map[string][]byte{}
		for _, projection := range externalProjections {
			if err != nil {
				break
			}
			cacheKey := projection.Source.Provider + "\x00" + projection.Source.File
			source, ok := decryptedSources[cacheKey]
			if !ok {
				source, err = secrets.RenderContext(ctx, filepath.Dir(lp.configPath), projection.Source.File)
				if err != nil {
					break
				}
				decryptedSources[cacheKey] = source
			}
			var envBytes []byte
			envBytes, err = secrets.ProjectEnvironment(source, projection.Entries)
			if err == nil {
				payloads = append(payloads, engine.SecretPayload{Path: projection.Path, Bytes: envBytes})
			}
		}
		if err == nil {
			var push engine.SecretPushResult
			push, err = e.SecretsPushBatch(ctx, payloads)
			result.EvidenceID = push.ReleaseID
			result.NoOp = push.NoOp
		}
	case KindDestroy:
		err = e.Destroy(ctx, request.RemoveVolumes, request.RemoveProxy)
	}
	if err == nil {
		emitProgress("execute", "succeeded", "")
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
		return false, &PlanExpiredError{Kind: PlanKindDeployment, ExpiresAt: expiresAt}
	}
	if createdAt.After(now.Add(time.Minute)) {
		return false, errors.New("deployment plan was created in the future — check the runner clock and re-plan")
	}
	emit("binding", "started", "")
	// The images the plan *rendered* with, not the ones it pinned: pinning
	// resolves a tag to a digest after the render, so reloading with the pins
	// would produce a different document than the plan bound and the binding
	// check would refuse its own plan. The pins are applied further down, as
	// planning applied them.
	lp, err := s.loadProjectWith(ctx, false, plan.Artifact.BuildImages)
	if err != nil {
		return false, fmt.Errorf("load project: %w", err)
	}
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return false, err
	}
	hasSecretGraph := len(lp.resolved.SecretDeclarationGraph()) > 0
	if hasSecretGraph != (plan.Artifact.SecretGeneration != "") || (hasSecretGraph && !release.IsSecretGeneration(plan.Artifact.SecretGeneration)) {
		return false, errors.New("deployment plan secret generation does not match the resolved configuration — re-plan")
	}
	environmentConfig, err := lp.resolved.Environment(s.environment)
	if err != nil {
		return false, err
	}
	if err := enforceRunnerPolicy(environmentConfig.Policy, s.runner, plan.SchemaVersion); err != nil {
		return false, err
	}
	if err := lp.resolved.Spec.RunPreflight(filepath.Dir(lp.configPath)); err != nil {
		return false, err
	}
	expectedGraph, err := DeploymentGraph(lp.resolved, plan.Operation.ReleaseID)
	if err != nil {
		return false, fmt.Errorf("build expected operation graph: %w", err)
	}
	if !reflect.DeepEqual(plan.Operation.Steps, expectedGraph) {
		return false, errors.New("operation graph differs from the resolved configuration — re-plan")
	}
	expectedMigrationBackup, err := migrationBackupRequirement(lp.resolved, environmentConfig.Policy, expectedGraph)
	if err != nil {
		return false, fmt.Errorf("build expected migration backup requirement: %w", err)
	}
	if !reflect.DeepEqual(plan.MigrationBackup, expectedMigrationBackup) {
		return false, errors.New("migration backup requirement differs from the resolved configuration — re-plan")
	}
	expectedRisk, expectedReversibility, expectedApproval := classifyDeploymentForPolicy(
		expectedGraph,
		plan.Artifact.HostState.CurrentRelease,
		environmentConfig.Policy.RequireApproval,
	)
	if plan.Operation.Risk != expectedRisk ||
		plan.Operation.Reversibility != expectedReversibility ||
		plan.Operation.Approval != expectedApproval {
		return false, errors.New("operation risk classification differs from the resolved configuration — re-plan")
	}
	binding := plan.Operation.Binding
	if lp.resolved.Name != binding.Application {
		return false, fmt.Errorf("plan application is %q, local application is %q — re-plan", binding.Application, lp.resolved.Name)
	}
	if s.environment != binding.Environment {
		return false, fmt.Errorf("plan environment is %q, executing %q — re-plan", binding.Environment, s.environment)
	}
	if engine.HashBytes(lp.configBytes) != binding.ConfigDigest {
		return false, errors.New("configuration changed since plan — re-plan")
	}
	// The runtime is generated, so this catches every input that feeds
	// generation — a referenced Compose file, a resolved image, the base path,
	// and a change in how this binary renders. Naming only the Compose file
	// would send the operator to look at one of five things.
	if engine.HashBytes(lp.composeBytes) != binding.ComposeDigest {
		return false, errors.New(
			"the runtime this project generates is not the one the plan bound — " +
				"a referenced Compose file, a resolved image, the base path, or this binary's " +
				"generation changed since planning — re-plan")
	}
	approvalRequired := environmentConfig.Policy.RequireApproval && (!plan.NoOp || request.Redeploy)
	if approvalRequired {
		emit("approval", "started", "")
		if request.Approval == nil {
			return false, fmt.Errorf(
				"%s approval is required for this exact deployment plan; record a local confirmation with `ob approve --plan PLAN` and apply it with `ob deploy --plan PLAN --approval APPROVAL`",
				plan.Operation.Approval,
			)
		}
		if err := request.Approval.ValidateForPlan(plan, s.now().UTC()); err != nil {
			return false, fmt.Errorf("validate deployment approval: %w", err)
		}
		emit("approval", "succeeded", "")
	} else if request.Approval != nil {
		// A supplied grant is never ignored: accepting a mismatched receipt just
		// because policy no longer requires it would make audit evidence lie.
		if err := request.Approval.ValidateForPlan(plan, s.now().UTC()); err != nil {
			return false, fmt.Errorf("validate deployment approval: %w", err)
		}
	}
	backupRequired := plan.MigrationBackup != nil && (!plan.NoOp || request.Redeploy)
	if backupRequired || request.BackupReport != nil || request.MigrationBackupOverride != nil {
		emit("migration_backup", "started", "")
	}
	migrationBackupAudit, err := validateMigrationBackupForExecution(
		plan, request.BackupReport, request.MigrationBackupOverride, request.Approval,
		backupRequired, s.now().UTC(),
	)
	if err != nil {
		return false, fmt.Errorf("validate migration backup authorization: %w", err)
	}
	if migrationBackupAudit != nil {
		emit("migration_backup", "succeeded", "")
	}
	applyPinnedImages(lp.compose, plan.Artifact.PinnedImages)
	e, cleanup, target, err := s.engineWith(ctx, lp, s.environment, func(options *engine.Options) {
		options.ForceLock = request.BreakLock
		options.NoRollback = request.NoRollback
		options.Progress = emit
		options.MigrationBackupWasRequired = plan.MigrationBackup != nil
		options.MigrationBackup = migrationBackupAudit
		if request.Approval != nil {
			options.ApprovalDigest = request.Approval.ApprovalDigest
			options.ApprovedBy = request.Approval.ApprovedBy
			options.ApprovalSource = request.Approval.Source
			options.ApprovalClass = string(request.Approval.Approval)
			options.AllowUnknownMigration = hasMigrationStep(plan.Operation.Steps) &&
				(request.Approval.Approval == ApprovalStrong || request.Approval.Approval == ApprovalBreakGlass)
		}
		options.DeployPrecondition = func(preconditionContext context.Context, locked *engine.Engine) error {
			if expiresAt.Before(s.now().UTC()) {
				return &PlanExpiredError{Kind: PlanKindDeployment, ExpiresAt: expiresAt}
			}
			return verifyRemoteDeployBinding(preconditionContext, locked, plan, s.environment, lp.configBytes, lp.resolved.Name)
		}
	})
	if err != nil {
		return false, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()
	if target != binding.Server {
		return false, fmt.Errorf("target changed from %q to %q — re-plan", binding.Server, target)
	}
	if err := verifyRemoteDeployBinding(ctx, e, plan, s.environment, lp.configBytes, lp.resolved.Name); err != nil {
		return false, err
	}
	emit("binding", "succeeded", "")

	emit("stage", "started", "")
	staging, cleanupStaging, err := stageExecution(ctx, lp, s.environment, plan.Operation.ReleaseID, plan.Artifact.SecretGeneration, plan.Artifact.PinnedImages)
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
	payloadDigest, err := engine.LocalPayloadDigestContext(ctx, lp.resolved, staging)
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
	_, liveComposeDigest, err := readLiveComposeState(ctx, e, fresh.CurrentRelease)
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
