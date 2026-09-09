package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// RunJobWithJournalID executes one current-release manual job under the same
// lock, fence, approval evidence, result protocol, and journal authority used
// by deployment jobs. The expected release/runtime checks run after the lock
// is held, closing the plan-to-execution race before container creation.
type JobRunRequest struct {
	OperationID           string
	Job                   string
	ExpectedRelease       string
	ExpectedRuntimeDigest string
	ExpectedDataEffect    app.DataEffect
}

func (e *Engine) RunJobWithJournalID(ctx context.Context, request JobRunRequest) (string, *journal.JobResultEvidence, error) {
	operationID := request.OperationID
	job := request.Job
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
	if workload.DataEffect != request.ExpectedDataEffect {
		return operationID, nil, errors.New("job data effect changed since planning — re-plan")
	}
	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return operationID, nil, err
	}
	// Released unless an interrupted run left this operation's container alive.
	// ReleaseLock runs on its own background context, so on Ctrl-C it succeeds
	// while the terminal journal append — which uses the cancelled one — does
	// not: ownership would be dropped, immediately and silently, over a
	// container still changing data.
	holdLockForLiveContainer := false
	defer func() {
		if holdLockForLiveContainer {
			e.warnf("operation %s was interrupted while its container is still running; "+
				"keeping the application lock so nothing else mutates alongside it. "+
				"Inspect with `docker ps --filter label=%s=%s`; the lock expires on its own after %s",
				operationID, JobOperationLabel, operationID, e.lockTTL())
			return
		}
		e.ReleaseLock(ctx)
	}()
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return operationID, nil, err
	}
	stopHeartbeat := e.StartHeartbeat(ctx)
	defer stopHeartbeat()

	// Under the lock, before anything mutates and before any host write: a job
	// container from an earlier operation may still be running with no process
	// owning it. Read-only, so it is safe on this side of the plan boundary.
	if err := e.refuseForeignJobContainers(ctx, operationID); err != nil {
		return operationID, nil, err
	}

	current, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return operationID, nil, err
	}
	if current != request.ExpectedRelease {
		return operationID, nil, fmt.Errorf("job plan is stale: current release changed from %q to %q — re-plan", request.ExpectedRelease, current)
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
	if actual := HashBytes([]byte(res.Stdout)); actual != request.ExpectedRuntimeDigest {
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
		// A cancelled context is exactly when the terminal record matters most,
		// and exactly when appending on that context cannot work. `ob exec`
		// already writes its own on a bounded background context; without the
		// same here an interrupted job stays INCOMPLETE in `ob audit` forever,
		// with no record that it was ever interrupted. Append redacts Detail on
		// a failure, so the reason has to ride on ErrorCode.
		// Two independent questions. WHERE to append: a cancelled caller context
		// cannot carry the write, whatever the run did, so a job that finished
		// cleanly a moment before Ctrl-C still records its success. WHAT to
		// record: only a run that ended because the client went away is
		// interrupted — an outcome the job itself produced is its own.
		journalContext := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			journalContext, cancel = context.WithTimeout(context.Background(), journalCleanupTimeout)
			defer cancel()
		}
		if interruptedRun(ctx, runErr) {
			record = journal.Record{
				Phase: "job", Event: "finish", Status: "fail", ErrorCode: "interrupted",
				OperationKind: "job_run", Service: job,
			}
		}
		journalErr := writer.Append(journalContext, record)
		if journalErr != nil && journalContext == ctx && ctx.Err() != nil {
			// Cancellation can land during the write as easily as before it, and
			// the check above only sees a context that was already gone. Retried
			// only when the context died in the meantime: any other failure —
			// a full disk, a refused write — may have landed on the host after
			// reporting an error, and appending a second terminal record is
			// worse than reporting the first failure.
			retryContext, cancel := context.WithTimeout(context.Background(), journalCleanupTimeout)
			defer cancel()
			journalErr = writer.Append(retryContext, record)
		}
		if journalErr != nil {
			return errors.Join(runErr, fmt.Errorf("journal job finish: %w", journalErr))
		}
		return runErr
	}

	if request.ExpectedDataEffect == app.DataEffectMigration && e.Opts.MigrationBackupWasRequired {
		if err := validateMigrationBackupEvidence(e.Opts.MigrationBackup, e.Opts.Now().UTC()); err != nil {
			journalErr := writer.Append(ctx, journal.Record{
				Phase: "job", SubStep: journal.MigrationBackupSubStep, Event: "result", Status: "fail",
				ErrorCode: "migration_backup_required", Detail: "migration backup authorization rejected",
			})
			if journalErr != nil {
				err = errors.Join(err, fmt.Errorf("journal migration backup rejection: %w", journalErr))
			}
			return operationID, nil, finish(err)
		}
		if err := writer.Append(ctx, journal.Record{
			Phase: "job", SubStep: journal.MigrationBackupSubStep, Event: "result", Status: "ok",
			Detail: "migration backup authorization accepted",
		}); err != nil {
			return operationID, nil, finish(fmt.Errorf("journal migration backup authorization: %w", err))
		}
	}

	// Past the staleness checks, so a plan that will not execute writes nothing.
	if err := e.closeInterruptedJobRuns(ctx); err != nil {
		return operationID, nil, err
	}

	e.gateOpen = true
	e.rollbackCovered = true
	runErr := e.runJobPhase(ctx, writer, nil, remoteDir, remoteCompose, "job", []string{job})
	if interruptedRun(ctx, runErr) {
		// Cancelling the client kills at most the wrapper shell; the container
		// belongs to the daemon and keeps running.
		holdLockForLiveContainer = e.jobContainerRunning(operationID)
	}
	var result *journal.JobResultEvidence
	if evidence, ok := e.jobResults[job]; ok {
		resultCopy := evidence
		result = &resultCopy
	}
	return operationID, result, finish(runErr)
}

const journalCleanupTimeout = 5 * time.Second

// interruptedRun reports a run that ended because the client went away rather
// than because the job finished.
//
// A run that produced no error finished, whatever became of the client
// afterwards — its outcome is its own and must be recorded as such. Only once
// the run failed does a gone context mean the client is why. The transport does
// not always surface a cancelled context as context.Canceled, so a failure
// under a cancelled context counts even when the error says something else.
func interruptedRun(ctx context.Context, runErr error) bool {
	if runErr == nil {
		return false
	}
	return ctx.Err() != nil ||
		errors.Is(runErr, context.Canceled) ||
		errors.Is(runErr, context.DeadlineExceeded)
}

// jobContainerRunning answers whether this operation's one-off container is
// still alive, on a context of its own because the caller's is already gone.
// An unreadable answer is reported as running: keeping the lock over a
// container that has in fact exited costs an operator one `--break-lock`, while
// releasing it over one that has not costs them concurrent writers.
func (e *Engine) jobContainerRunning(operationID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), journalCleanupTimeout)
	defer cancel()
	res, err := e.T.Run(ctx, "docker ps -q --filter label="+q(JobOperationLabel+"="+operationID))
	if err != nil {
		return true
	}
	if res.ExitCode != 0 {
		return true
	}
	return strings.TrimSpace(res.Stdout) != ""
}
