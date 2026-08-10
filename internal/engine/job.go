package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// RunJobWithJournalID executes one current-release manual job under the same
// lock, fence, approval evidence, result protocol, and journal authority used
// by deployment jobs. The expected release/runtime checks run after the lock
// is held, closing the plan-to-execution race before container creation.
func (e *Engine) RunJobWithJournalID(
	ctx context.Context,
	operationID, job, expectedRelease, expectedRuntimeDigest, expectedDataEffect string,
) (string, *journal.JobResultEvidence, error) {
	if err := e.RequireHostOwner(ctx); err != nil {
		return operationID, nil, err
	}
	workload, ok := e.Spec.Workloads[job]
	if !ok || !workload.IsJob() {
		return operationID, nil, fmt.Errorf("unknown job %q", job)
	}
	if workload.When != "manual" {
		return operationID, nil, fmt.Errorf("job %q is not a manual job", job)
	}
	if workload.DataEffect != expectedDataEffect {
		return operationID, nil, errors.New("job data effect changed since planning — re-plan")
	}
	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return operationID, nil, err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return operationID, nil, err
	}
	stopHeartbeat := e.StartHeartbeat(ctx)
	defer stopHeartbeat()

	current, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return operationID, nil, err
	}
	if current != expectedRelease {
		return operationID, nil, fmt.Errorf("job plan is stale: current release changed from %q to %q — re-plan", expectedRelease, current)
	}
	remoteDir := release.PathsFor(e.names()).Releases + "/" + current
	remoteCompose := remoteDir + "/compose.yaml"
	res, err := e.T.Run(ctx, "cat "+q(remoteCompose)+" 2>/dev/null")
	if err != nil {
		return operationID, nil, err
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return operationID, nil, fmt.Errorf("current release %q runtime is unavailable", current)
	}
	if actual := HashBytes([]byte(res.Stdout)); actual != expectedRuntimeDigest {
		return operationID, nil, errors.New("job plan is stale: current release runtime changed — re-plan")
	}

	writer := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: operationID, Epoch: epoch,
		Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash,
		ApprovalDigest: e.Opts.ApprovalDigest, ApprovalClass: e.Opts.ApprovalClass,
		ApprovedBy: e.Opts.ApprovedBy, ApprovalSource: e.Opts.ApprovalSource,
		AllowUnknownMigration:   e.Opts.AllowUnknownMigration,
		Runner:                  &e.Opts.Runner,
		MigrationBackupRequired: e.Opts.MigrationBackupWasRequired,
		MigrationBackup:         e.Opts.MigrationBackup,
	}
	start := journal.Record{
		Phase: "job", Event: "start", Status: "ok", OperationKind: "job_run", Service: job,
		Detail: "release=" + current,
	}
	if err := writer.Append(ctx, start); err != nil {
		return operationID, nil, fmt.Errorf("journal job start: %w", err)
	}
	finish := func(runErr error) error {
		record := journal.Record{Phase: "job", Event: "finish", Status: "ok", OperationKind: "job_run", Service: job}
		if runErr != nil {
			record.Status = "fail"
			record.Detail = runErr.Error()
		}
		if journalErr := writer.Append(ctx, record); journalErr != nil {
			return errors.Join(runErr, fmt.Errorf("journal job finish: %w", journalErr))
		}
		return runErr
	}

	if expectedDataEffect == "migration" && e.Opts.MigrationBackupWasRequired {
		if err := validateMigrationBackupEvidence(e.Opts.MigrationBackup, e.Opts.Now().UTC()); err != nil {
			_ = writer.Append(ctx, journal.Record{
				Phase: "job", SubStep: journal.MigrationBackupSubStep, Event: "result", Status: "fail",
				ErrorCode: "migration_backup_required", Detail: "migration backup authorization rejected",
			})
			return operationID, nil, finish(err)
		}
		if err := writer.Append(ctx, journal.Record{
			Phase: "job", SubStep: journal.MigrationBackupSubStep, Event: "result", Status: "ok",
			Detail: "migration backup authorization accepted",
		}); err != nil {
			return operationID, nil, finish(fmt.Errorf("journal migration backup authorization: %w", err))
		}
	}

	e.gateOpen = true
	e.rollbackCovered = true
	runErr := e.runJobPhase(ctx, writer, nil, remoteDir, remoteCompose, "job", []string{job})
	var result *journal.JobResultEvidence
	if evidence, ok := e.jobResults[job]; ok {
		copy := evidence
		result = &copy
	}
	return operationID, result, finish(runErr)
}
