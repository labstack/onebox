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

// FindIncomplete returns the newest deploy journal when it started but never
// finished or aborted — the deploy a crashed runner left behind.
//
// Only the newest deploy is ever actionable. Once a later deploy reaches a
// terminal state it has rolled every role and activated its own release, so an
// older interrupted deploy has nothing left to complete: resuming it would
// re-activate a superseded release, and aborting it would revert to a
// predecessor two releases stale. Its records remain in `ob audit` as history;
// what they are not is work waiting to be done.
func (e *Engine) FindIncomplete(ctx context.Context) (journal.Summary, error) {
	ids, byID, err := journal.Journals(ctx, e.T, e.names())
	if err != nil {
		return journal.Summary{}, err
	}
	for i := len(ids) - 1; i >= 0; i-- {
		s := journal.Summarize(byID[ids[i]])
		if !s.Started {
			continue // a job or service journal, not a deploy
		}
		if s.Finished || s.Aborted || s.Recovered {
			return journal.Summary{}, ErrNoIncomplete
		}
		return s, nil
	}
	return journal.Summary{}, ErrNoIncomplete
}

// Resume continues an interrupted deploy from the journal: completed phases
// and roles skip; the half-rolled role is adopted via its ob.release label.
// A NEW lock epoch is taken, which fences the old runner if it still lives.
//
// A deploy interrupted AFTER activation is resumed too, but nothing is
// replayed: the release is already the truth, so resume completes only the
// post-activation steps that remain (see finalizeActivated).
func (e *Engine) Resume(ctx context.Context) error {
	_, err := e.ResumeWithJournalID(ctx)
	return err
}

// ResumeWithJournalID resumes the incomplete deploy and returns the journal
// identity it operated on. The identity is returned even when execution fails
// after the incomplete journal has been resolved.
func (e *Engine) ResumeWithJournalID(ctx context.Context) (string, error) {
	if err := e.RequireHostOwner(ctx); err != nil {
		return "", err
	}
	s, err := e.FindIncomplete(ctx)
	if err != nil {
		return "", err
	}
	e.logf("resuming %s (started %s by %s; transfer complete=%v)",
		s.DeployID, s.StartedAt, s.Operator, s.Done["transfer"])
	e.gateOpen = s.GateOpen
	e.rollbackCovered = s.RollbackCovered // preserve the interrupted deploy's effect policy
	e.journalPredecessor = s.PrevRelease
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
//
// force is that gate and nothing else. A previous release whose snapshot cannot
// be read is not a gate an operator may assert past: without it there is no
// record of that release's choreography, so there is nothing to replay.
func (e *Engine) Abort(ctx context.Context, force bool) error {
	_, err := e.AbortWithJournalID(ctx, force)
	return err
}

// AbortWithJournalID aborts the incomplete deploy and returns the journal
// identity it operated on. The identity is returned even when the abort fails
// after the incomplete journal has been resolved.
func (e *Engine) AbortWithJournalID(ctx context.Context, force bool) (string, error) {
	if err := e.RequireHostOwner(ctx); err != nil {
		return "", err
	}
	s, err := e.FindIncomplete(ctx)
	if err != nil {
		return "", err
	}
	return s.DeployID, e.abort(ctx, s, force)
}

func (e *Engine) abort(ctx context.Context, s journal.Summary, force bool) (err error) {
	if !s.RollbackCovered && !force {
		return fmt.Errorf("abort refused — HALT-AND-PAGE: deploy %s ran a job or lifecycle hook with rollback-unknown data effects not covered by a safe result or migration_policy. Fix-forward + `ob resume`, or `ob abort --break-migration-gate` if you know the data is compatible", s.DeployID)
	}
	epoch, err := e.AcquireLock(ctx, s.DeployID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, s.DeployID, epoch); err != nil {
		return err
	}
	jw := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: s.DeployID, Epoch: epoch,
		Operator: journal.DefaultOperator(), Runner: &e.Opts.Runner,
		ApprovalDigest: s.ApprovalDigest, ApprovalClass: s.ApprovalClass,
		ApprovedBy: s.ApprovedBy, ApprovalSource: s.ApprovalSource,
		AllowUnknownMigration:   s.AllowUnknownMigration,
		MigrationBackupRequired: s.MigrationBackupRequired,
		MigrationBackup:         s.MigrationBackup,
	}
	return e.recoverInterrupted(ctx, recoveryRequest{
		InterruptedID: s.DeployID,
		PreviousID:    s.PrevRelease,
		TerminalState: release.StateAborted,
		GateCovered:   s.RollbackCovered,
		BreakGlass:    force,
		Phase:         "abort",
		Journal:       jw,
	})
}
