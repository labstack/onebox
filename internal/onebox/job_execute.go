package onebox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/journal"
)

func (s *Service) executeJob(
	ctx context.Context,
	request ExecuteRequest,
	emit func(string, string, string),
) (string, *journal.JobResultEvidence, error) {
	plan := request.JobPlan
	if err := plan.Validate(); err != nil {
		return "", nil, fmt.Errorf("validate executable job plan: %w", err)
	}
	createdAt, err := parseOperationTime(plan.Operation.CreatedAt, "created_at")
	if err != nil {
		return "", nil, err
	}
	expiresAt, err := parseOperationTime(plan.Operation.ExpiresAt, "expires_at")
	if err != nil {
		return "", nil, err
	}
	now := s.now().UTC()
	if expiresAt.Before(now) {
		return "", nil, &PlanExpiredError{Kind: PlanKindJob, Job: plan.Artifact.Job, ExpiresAt: expiresAt}
	}
	if createdAt.After(now.Add(time.Minute)) {
		return "", nil, errors.New("job plan was created in the future — check the runner clock and re-plan")
	}

	emit("binding", "started", "")
	lp, err := s.loadProject(ctx, false)
	if err != nil {
		return "", nil, fmt.Errorf("load project: %w", err)
	}
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return "", nil, err
	}
	environmentConfig, err := lp.resolved.Environment(s.environment)
	if err != nil {
		return "", nil, err
	}
	if err := enforceRunnerPolicy(environmentConfig.Policy, s.runner, plan.SchemaVersion); err != nil {
		return "", nil, err
	}
	binding := plan.Operation.Binding
	if lp.resolved.Name != binding.Application || s.environment != binding.Environment {
		return "", nil, errors.New("job plan application or environment changed — re-plan")
	}
	if environmentConfig.Destination() != binding.Server {
		return "", nil, errors.New("job plan target changed — re-plan")
	}
	if engine.HashBytes(lp.configBytes) != binding.ConfigDigest {
		return "", nil, errors.New("configuration changed since job planning — re-plan")
	}
	job, ok := lp.resolved.Workloads[plan.Artifact.Job]
	if !ok || !job.IsJob() || job.When != "manual" || DataEffectClass(job.DataEffect) != plan.Artifact.DataEffect {
		return "", nil, errors.New("manual job declaration changed since planning — re-plan")
	}
	expectedBackup, err := migrationBackupRequirement(lp.resolved, environmentConfig.Policy, plan.Operation.Steps)
	if err != nil {
		return "", nil, err
	}
	if !reflect.DeepEqual(plan.MigrationBackup, expectedBackup) {
		return "", nil, errors.New("migration backup requirement changed since job planning — re-plan")
	}

	if plan.Operation.Approval != ApprovalNone {
		if request.Approval == nil {
			return "", nil, fmt.Errorf("%s approval is required for this exact job plan; record a local confirmation with `ob approve --plan PLAN`", plan.Operation.Approval)
		}
		if err := request.Approval.ValidateForPlan(plan, now); err != nil {
			return "", nil, fmt.Errorf("validate job approval: %w", err)
		}
	} else if request.Approval != nil {
		if err := request.Approval.ValidateForPlan(plan, now); err != nil {
			return "", nil, fmt.Errorf("validate job approval: %w", err)
		}
	}
	backupRequired := plan.MigrationBackup != nil
	migrationBackup, err := validateMigrationBackupForExecution(
		plan, request.BackupReport, request.MigrationBackupOverride, request.Approval,
		backupRequired, now,
	)
	if err != nil {
		return "", nil, fmt.Errorf("validate migration backup authorization: %w", err)
	}

	e, cleanup, target, err := s.engineWith(ctx, lp, s.environment, func(options *engine.Options) {
		options.ForceLock = request.BreakLock
		options.ApprovalDigest = ""
		options.ApprovedBy = ""
		options.ApprovalSource = ""
		options.ApprovalClass = ""
		options.AllowUnknownMigration = false
		if request.Approval != nil {
			options.ApprovalDigest = request.Approval.ApprovalDigest
			options.ApprovedBy = request.Approval.ApprovedBy
			options.ApprovalSource = request.Approval.Source
			options.ApprovalClass = string(request.Approval.Approval)
			options.AllowUnknownMigration = plan.Artifact.DataEffect == DataEffectMigration &&
				(request.Approval.Approval == ApprovalStrong || request.Approval.Approval == ApprovalBreakGlass)
		}
		options.MigrationBackupWasRequired = plan.MigrationBackup != nil
		options.MigrationBackup = migrationBackup
		options.Progress = emit
	})
	if err != nil {
		return "", nil, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()
	if target != binding.Server {
		return "", nil, fmt.Errorf("target changed from %q to %q — re-plan", binding.Server, target)
	}
	emit("binding", "succeeded", "")
	emit("execute", "started", "")
	evidenceID, result, err := e.RunJobWithJournalID(ctx, engine.JobRunRequest{
		OperationID: plan.Operation.ID, Job: plan.Artifact.Job,
		ExpectedRelease: plan.Artifact.CurrentRelease, ExpectedRuntimeDigest: plan.Artifact.RuntimeDigest,
		ExpectedDataEffect: plan.Artifact.DataEffect,
	})
	if err == nil {
		emit("execute", "succeeded", "")
	}
	return evidenceID, result, err
}
