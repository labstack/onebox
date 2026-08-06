package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// ErrNoIncomplete means the journal shows no deploy needing resume/abort — a
// normal state. It is a distinct sentinel so `ob status` can tell it apart from
// a genuine journal-read failure (which must not be reported as "all in sync").
var ErrNoIncomplete = errors.New("no incomplete deploy found in the journal")

// FindIncomplete returns the newest journal that started but never finished
// or aborted — the deploy a crashed runner left behind.
func (e *Engine) FindIncomplete(ctx context.Context) (journal.Summary, error) {
	ids, byID, err := journal.Journals(ctx, e.T, e.names())
	if err != nil {
		return journal.Summary{}, err
	}
	for i := len(ids) - 1; i >= 0; i-- {
		s := journal.Summarize(byID[ids[i]])
		if s.Started && !s.Finished && !s.Aborted {
			return s, nil
		}
	}
	return journal.Summary{}, ErrNoIncomplete
}

// Resume continues an interrupted deploy from the journal: completed phases
// and roles skip; the half-rolled role is adopted via its ob.release label.
// A NEW lock epoch is taken, which fences the old runner if it still lives.
func (e *Engine) Resume(ctx context.Context) error {
	_, err := e.ResumeWithJournalID(ctx)
	return err
}

// ResumeWithJournalID resumes the incomplete deploy and returns the journal
// identity it operated on. The identity is returned even when execution fails
// after the incomplete journal has been resolved.
func (e *Engine) ResumeWithJournalID(ctx context.Context) (string, error) {
	s, err := e.FindIncomplete(ctx)
	if err != nil {
		return "", err
	}
	e.logf("resuming %s (started %s by %s; done: transfer=%v migrate=%v)",
		s.DeployID, s.StartedAt, s.Operator, s.Done["transfer"], s.Done["migrate"])
	e.gateOpen = s.GateOpen
	e.rollbackCovered = s.RollbackCovered // preserve the interrupted deploy's effect policy
	e.Opts.MigrationBackupWasRequired = s.MigrationBackupRequired
	e.Opts.MigrationBackup = s.MigrationBackup
	e.Opts.ApprovalDigest = s.ApprovalDigest
	e.Opts.ApprovalClass = s.ApprovalClass
	e.Opts.ApprovedBy = s.ApprovedBy
	e.Opts.ApprovalSource = s.ApprovalSource
	e.Opts.AllowUnknownMigration = s.AllowUnknownMigration
	e.jobResults = make(map[string]journal.JobResultEvidence, len(s.JobResults))
	for job, result := range s.JobResults {
		e.jobResults[job] = result
	}
	replay, err := e.engineFromReleaseSnapshot(ctx, s.DeployID)
	if err != nil {
		return s.DeployID, err
	}
	return s.DeployID, replay.deployCore(ctx, s.DeployID, "", s.Done)
}

// Abort reverts an interrupted deploy to the previous release. The migration
// gate governs abort exactly like auto-rollback: aborting
// after a schema change is the same hazard.
func (e *Engine) Abort(ctx context.Context, force bool) error {
	_, err := e.AbortWithJournalID(ctx, force)
	return err
}

// AbortWithJournalID aborts the incomplete deploy and returns the journal
// identity it operated on. The identity is returned even when the abort fails
// after the incomplete journal has been resolved.
func (e *Engine) AbortWithJournalID(ctx context.Context, force bool) (string, error) {
	s, err := e.FindIncomplete(ctx)
	if err != nil {
		return "", err
	}
	return s.DeployID, e.abort(ctx, s, force)
}

func (e *Engine) abort(ctx context.Context, s journal.Summary, force bool) (err error) {
	if !s.RollbackCovered && !force {
		return fmt.Errorf("abort refused — HALT-AND-PAGE: deploy %s ran a job or lifecycle hook with rollback-unknown data effects not covered by a safe result or migration_policy. Fix-forward + `ob resume`, or `ob abort --force` if you know the data is compatible", s.DeployID)
	}
	interrupted, err := e.engineFromReleaseSnapshot(ctx, s.DeployID)
	if err != nil {
		return err
	}
	replay := interrupted
	if s.PrevRelease != "" {
		replay, err = e.engineFromReleaseSnapshot(ctx, s.PrevRelease)
		if err != nil {
			return err
		}
	}
	epoch, err := e.AcquireLock(ctx, s.DeployID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, s.DeployID, epoch); err != nil {
		return err
	}
	interrupted.fenceVal = e.fenceVal
	replay.fenceVal = e.fenceVal
	jw := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: s.DeployID, Epoch: epoch,
		Operator: journal.DefaultOperator(), Runner: &e.Opts.Runner,
		ApprovalDigest: s.ApprovalDigest, ApprovalClass: s.ApprovalClass,
		ApprovedBy: s.ApprovedBy, ApprovalSource: s.ApprovalSource,
		AllowUnknownMigration:   s.AllowUnknownMigration,
		MigrationBackupRequired: s.MigrationBackupRequired,
		MigrationBackup:         s.MigrationBackup,
	}
	if err := jw.Append(ctx, journal.Record{Phase: "abort", Event: "intent", Detail: "to=" + s.PrevRelease}); err != nil {
		return fmt.Errorf("journal abort intent: %w", err)
	}
	defer func() {
		result := journal.Record{Phase: "abort", Event: "abort", Status: "ok"}
		if err != nil {
			result.Status = "fail"
			result.Detail = err.Error()
		}
		if journalErr := jw.Append(ctx, result); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal abort result: %w", journalErr))
		}
	}()

	if s.PrevRelease == "" {
		e.logf("abort: first deploy — removing its containers, nothing to restore")
		if err := interrupted.removeNewcomers(ctx, s.DeployID); err != nil {
			return err
		}
		return nil
	}

	// Replay the previous release through the normal choreography: RollRole
	// adopts prev-labeled containers as no-ops and drains this deploy's
	// newcomers as "old" — a zero-downtime abort for rolling roles. Recreate
	// roles are skipped when they already run the previous release.
	prevCompose := release.PathsFor(e.names()).Releases + "/" + s.PrevRelease + "/compose.yaml"
	for _, roleName := range replay.Spec.ReleaseOrder() {
		role := replay.Spec.Workloads[roleName]
		if role.Mode() == "recreate" {
			prevIDs, err := replay.newcomerIDs(ctx, roleName, s.PrevRelease)
			if err != nil {
				return err
			}
			if len(prevIDs) > 0 {
				e.logf("abort: %s already runs %s — untouched", roleName, s.PrevRelease)
				continue
			}
		}
		e.logf("abort: reverting %s to %s", roleName, s.PrevRelease)
		var rerr error
		if role.Mode() == "rolling" {
			rerr = replay.RollRole(ctx, roleName, prevCompose)
		} else {
			rerr = replay.RecreateRole(ctx, roleName, prevCompose)
		}
		if rerr != nil {
			return fmt.Errorf("abort %s: %w — intervene manually", roleName, rerr)
		}
	}
	// straggler sweep: failed-join leftovers of the aborted release
	if err := interrupted.removeNewcomers(ctx, s.DeployID); err != nil {
		return err
	}
	if err := replay.Verify(ctx); err != nil {
		return fmt.Errorf("abort verify: %w", err)
	}
	e.logf("aborted %s — %s serving", s.DeployID, s.PrevRelease)
	return nil
}
