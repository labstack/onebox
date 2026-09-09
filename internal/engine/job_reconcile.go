package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/journal"
)

// orphanedJobRun is a sealed job run whose journal has a start and no terminal
// record. Either the client went away before it could write one, or the job is
// still running and this operation is the one that owns it.
type orphanedJobRun struct {
	OperationID string
	Job         string
	// ResultRecorded is true when the client got far enough to journal the
	// job's result. That is the only after-the-fact proof of the outcome: the
	// container is `--rm`, so once it exits nothing about it survives.
	ResultRecorded bool
	ResultOK       bool
}

// findOrphanedJobRuns reads every journal in one round trip and returns the job
// runs that never reached a terminal record.
func (e *Engine) findOrphanedJobRuns(ctx context.Context) ([]orphanedJobRun, error) {
	ids, byID, err := journal.Journals(ctx, e.T, e.names())
	if err != nil {
		return nil, err
	}
	var out []orphanedJobRun
	for _, id := range ids {
		if orphan, ok := orphanedJobRunOf(byID[id]); ok {
			orphan.OperationID = id
			out = append(out, orphan)
		}
	}
	return out, nil
}

// orphanedJobRunOf reduces one journal. A job run is orphaned when its start
// record has no matching finish — deploy journals and completed runs are not.
func orphanedJobRunOf(records []journal.Record) (orphanedJobRun, bool) {
	orphan, started := orphanedJobRun{}, false
	for _, r := range records {
		if r.OperationKind != "job_run" || r.Phase != "job" {
			continue
		}
		switch r.Event {
		case "start":
			started, orphan.Job = true, r.Service
		case "finish":
			return orphanedJobRun{}, false
		case "result":
			if r.SubStep == "job:"+orphan.Job {
				orphan.ResultRecorded = true
				orphan.ResultOK = r.Status == "ok"
			}
		}
	}
	return orphan, started
}

// reconcileOrphanedJobRuns closes what it can prove and refuses what it cannot.
//
// It runs under the application lock, before any mutation. A still-running
// container means this host is already executing a data-changing job that no
// process owns: the only safe answer is to refuse, whatever the lock's TTL says
// about the client that started it.
func (e *Engine) reconcileOrphanedJobRuns(ctx context.Context) error {
	orphans, err := e.findOrphanedJobRuns(ctx)
	if err != nil {
		return err
	}
	for _, orphan := range orphans {
		running, err := e.jobContainerIDs(ctx, orphan.OperationID)
		if err != nil {
			return err
		}
		if len(running) > 0 {
			return fmt.Errorf(
				"job %s from operation %s is still running on the host (container %.12s) with no process owning it; "+
					"wait for it, or stop it with `docker rm -f %.12s` once you have established what it did",
				orphan.Job, orphan.OperationID, running[0], running[0])
		}
		if err := e.closeOrphanedJobRun(ctx, orphan); err != nil {
			return err
		}
	}
	return nil
}

// closeOrphanedJobRun writes the terminal record the interrupted client could
// not. It never invents success: only a result the client itself journaled
// proves the job's outcome, and anything else is recorded as interrupted so the
// rollback debt an unresolved data-changing job carries is preserved.
func (e *Engine) closeOrphanedJobRun(ctx context.Context, orphan orphanedJobRun) error {
	record := journal.Record{
		Phase: "job", Event: "finish", Status: "fail", ErrorCode: "interrupted",
		OperationKind: "job_run", Service: orphan.Job,
	}
	if orphan.ResultRecorded && orphan.ResultOK {
		record.Status, record.ErrorCode = "ok", ""
	}
	writer := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: orphan.OperationID,
		Operator: journal.DefaultOperator(),
	}
	if err := writer.Append(ctx, record); err != nil {
		return fmt.Errorf("close interrupted job run %s: %w", orphan.OperationID, err)
	}
	e.logf("closed interrupted job run %s (%s) as %s", orphan.OperationID, orphan.Job, record.Status)
	return nil
}

// jobContainerIDs lists containers still running for one operation.
func (e *Engine) jobContainerIDs(ctx context.Context, operationID string) ([]string, error) {
	res, err := e.T.Run(ctx, "docker ps -q --filter label="+q(JobOperationLabel+"="+operationID))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list containers of operation %s (exit %d): %s",
			operationID, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return splitIDs(res.Stdout)
}
