package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// The post-activation steps, journaled individually so a finalize replay
// repeats none that already succeeded — a step whose result was never recorded
// as ok runs again, which is what makes a fix-forward resume work. Retired
// workload cleanup, retention and schedule sync are idempotent; the post-deploy
// hook is not, which is the whole reason these keys exist.
const (
	finalizeRetiredWorkloadsSubStep = journal.FinalizeSubStepPrefix + "retired_workloads"
	finalizeRetentionSubStep        = journal.FinalizeSubStepPrefix + "retention"
	finalizeSchedulesSubStep        = journal.FinalizeSubStepPrefix + "schedules"
	finalizePostDeploySubStep       = journal.FinalizeSubStepPrefix + "post_deploy"
)

// FinalizeRefusedError reports that the post-activation steps cannot be
// completed because the durable evidence does not agree that this operation
// activated the release now serving. It is typed so an agent can branch on
// finalize_refused rather than parse prose, and it is raised instead of
// guessing: finalizing against a release some other operation activated would
// attribute one deploy's tail to another.
type FinalizeRefusedError struct {
	ReleaseID string
	Reason    string
}

func (err *FinalizeRefusedError) Error() string {
	return fmt.Sprintf("release %s cannot be finalized: %s; inspect `ob status`, then `ob abort` if the release must be reverted", err.ReleaseID, err.Reason)
}

func (err *FinalizeRefusedError) Code() string { return "finalize_refused" }

// named renders an absent release identifier as a word rather than an empty
// gap, so a refusal reason reads as a sentence in a log line.
func named(releaseID string) string {
	if releaseID == "" {
		return "(none)"
	}
	return releaseID
}

// finalizeActivated completes an operation whose release is already serving.
//
// Activation is the seam: before it a failure means the release never took
// effect, so resume replays the choreography; after it the release IS the
// truth, so replaying would re-activate something already live. What remains
// is the post-activation tail, and this is the only path that runs it a second
// time.
func (e *Engine) finalizeActivated(ctx context.Context, jw *journal.Writer, manifest *release.Manifest, done map[string]bool, remoteDir, remoteCompose string) error {
	if err := e.requireActivationEvidence(ctx, jw, manifest, done); err != nil {
		return err
	}
	e.logf("release %s is already serving; completing the post-activation steps only", manifest.ID)
	e.progress("verification", "started", "")
	vf := e.ui.Step("verify", false)
	if err := e.Verify(ctx); err != nil {
		vf(err)
		e.progress("verification", "failed", "verification failed; inspect journal evidence")
		// The release is still serving and this operation still failed, so the
		// manifest records that the same way a failed step does. Activation set
		// the outcome to succeeded; leaving it there would claim a finished
		// operation on a release that just failed its health gate.
		// Diagnostic guidance, not `ob resume`: resume re-enters this same
		// function, runs the same Verify, and fails identically. Every other
		// post-activation failure is genuinely completed by a re-run; this
		// one is self-perpetuating, and publishing a resolving command an
		// agent may execute would have it loop instead of stopping to look.
		return e.failedOutcomeWithGuidance(ctx, manifest, "ob audit --output json",
			fmt.Errorf("verify serving release: %w", err))
	}
	vf(nil)
	if err := jw.Append(ctx, journal.Record{Phase: "verify", Event: "result", Status: "ok"}); err != nil {
		// The release is serving and this operation still failed — the same
		// reasoning as the Verify branch one line up, which is why that one
		// goes through failedOutcome too.
		return e.failedOutcome(ctx, manifest, fmt.Errorf("journal verify result: %w", err))
	}
	e.progress("verification", "succeeded", "")
	return e.runPostActivation(ctx, jw, manifest, done, remoteDir, remoteCompose)
}

// requireActivationEvidence proves from durable evidence alone that the release
// now serving was activated by the operation being finalized. Every source must
// agree; one disagreement refuses. A serving manifest on its own proves only
// that some operation activated this release, which is not the same claim.
func (e *Engine) requireActivationEvidence(ctx context.Context, jw *journal.Writer, manifest *release.Manifest, done map[string]bool) error {
	refuse := func(format string, a ...any) error {
		return &FinalizeRefusedError{ReleaseID: manifest.ID, Reason: fmt.Sprintf(format, a...)}
	}
	// A completed activation checkpoint is the other durable proof. The
	// journal record is written after the clear-worthy checkpoint reaches its
	// last phase, so a failed journal append leaves exactly this state:
	// symlink switched, manifest serving, checkpoint open at
	// ActivationPredecessorSuperseded, journal holding only the intent.
	// Refusing on the journal alone makes that permanent — nothing rewrites
	// the record, so every resume repeats the refusal while the release is
	// live and its outcome stays pending, and the only offered escape rolls a
	// healthy release back.
	activated := done[journal.DoneActivation]
	journalledFromCheckpoint := false
	if !activated {
		checkpoint, checkpointErr := release.ReadActivationCheckpoint(ctx, e.T, e.names())
		switch {
		case checkpointErr == nil && checkpoint.ReleaseID == manifest.ID &&
			checkpoint.Phase == release.ActivationPredecessorSuperseded:
			activated = true
			journalledFromCheckpoint = true
		case checkpointErr != nil && !errors.Is(checkpointErr, release.ErrActivationCheckpointMissing):
			return fmt.Errorf("read activation checkpoint: %w", checkpointErr)
		}
	}
	if !activated {
		return refuse("its journal records no successful activation")
	}
	current, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return err
	}
	if current != manifest.ID {
		return refuse("the current release is %s", named(current))
	}
	if manifest.Predecessor != e.journalPredecessor {
		return refuse("its manifest predecessor %s is not the predecessor %s the operation recorded",
			named(manifest.Predecessor), named(e.journalPredecessor))
	}
	// A checkpoint still on the host means activation stopped partway through
	// its own sequence. Recovery reconciles that from the checkpoint; finalize
	// must not step over it.
	staleCheckpoint := false
	checkpoint, checkpointErr := release.ReadActivationCheckpoint(ctx, e.T, e.names())
	switch {
	case checkpointErr == nil:
		// A checkpoint for THIS release at the sequence's LAST phase, once the
		// journal already proves the activation completed, is a clear that
		// did not land — the clear is
		// the last write of the sequence and can fail on its own. Refusing
		// would be permanent: nothing on the resume path retries it, so every
		// `ob resume` would repeat the refusal on a healthy live release, and
		// the only escapes are rolling it back or deploying over it. Finish
		// the interrupted step instead of stepping over it.
		// The phase is what decides this, not whose release it names. A
		// checkpoint at the last phase records an activation that finished;
		// the only thing left undone is the clear. A rollback writes one
		// carrying the ROLLBACK TARGET's id, so keying on manifest.ID here
		// left that case refusing on every resume until someone deployed
		// again. Earlier phases still refuse: those are genuinely interrupted
		// sequences, and recovery reconciles them from the checkpoint.
		if checkpoint.Phase == release.ActivationPredecessorSuperseded {
			// Cleared after the live check below, not here: a run that ends
			// in finalize_refused changed nothing it claimed to, and the
			// checkpoint is durable evidence that recovery and retention both
			// read. Destroying it on the way to a refusal is a mutation the
			// refusal says did not happen.
			staleCheckpoint = true
			break
		}
		// Anything else — another release, or an earlier phase — means
		// activation stopped partway through its own sequence. Recovery
		// reconciles that from the checkpoint; finalize must not step over it.
		return refuse("an activation checkpoint is still open")
	case !errors.Is(checkpointErr, release.ErrActivationCheckpointMissing):
		return fmt.Errorf("read activation checkpoint: %w", checkpointErr)
	}
	if err := e.requireLiveRelease(ctx, manifest.ID); err != nil {
		return err
	}
	// The checkpoint was the ONLY proof of activation, and it is about to be
	// deleted. Write the journal record it stood in for first: clearing
	// without it would leave a release whose activation nothing records, and
	// any later failure in this run — likely, since an unwritable journal is
	// what lost the record to begin with — would make every future resume
	// refuse permanently on a healthy, live release.
	if journalledFromCheckpoint {
		if err := jw.Append(ctx, journal.Record{
			Phase: "activation", Event: "result", Status: "ok", Detail: "release=" + manifest.ID,
		}); err != nil {
			// Reached only after requireLiveRelease proved the release is
			// live, and it is the path most likely to fail — it exists
			// because the journal was unwritable earlier. An untyped error
			// here reports cancelled/exit 2 for a serving release.
			return &PostActivationFailedError{
				ReleaseID: manifest.ID,
				Err:       fmt.Errorf("journal activation result reconstructed from checkpoint: %w", err),
			}
		}
	}
	// Every source agreed, so the clear that did not land can be completed.
	// rm -f is idempotent, so repeating it costs nothing.
	if staleCheckpoint {
		// Reached only once the release is proven live, so the same typing
		// applies: an interrupt here is not "nothing was changed".
		if err := e.clearActivationCheckpoint(ctx); err != nil {
			return e.failedOutcome(ctx, manifest, fmt.Errorf("clear activation checkpoint: %w", err))
		}
	}
	return nil
}

// requireLiveRelease checks the host agrees with the manifest: every
// long-running workload is running — jobs are not in ReleaseOrder and are not
// expected to be — and every container of it carries this release's label.
// Records can be written by a runner that then died before the workloads
// converged, so the live state is evidence in its own right.
func (e *Engine) requireLiveRelease(ctx context.Context, releaseID string) error {
	byService, err := e.projectContainers(ctx)
	if err != nil {
		return err
	}
	for _, roleName := range e.Spec.ReleaseOrder() {
		containers := byService[roleName]
		if len(containers) == 0 {
			return &FinalizeRefusedError{ReleaseID: releaseID, Reason: "workload " + roleName + " is not running"}
		}
		for _, container := range containers {
			if container.release != releaseID {
				return &FinalizeRefusedError{
					ReleaseID: releaseID,
					Reason:    fmt.Sprintf("workload %s runs release %s", roleName, named(container.release)),
				}
			}
		}
	}
	return nil
}

// runPostActivation is everything that happens after the release is durably
// serving: retention, schedule sync, and the post-deploy hook. A failure here
// records a failed operation outcome and leaves the release serving, because
// that is the truth — the code is live and healthy, and the work around it is
// what did not finish. `ob resume` then completes exactly the steps that remain.
func (e *Engine) runPostActivation(ctx context.Context, jw *journal.Writer, manifest *release.Manifest, done map[string]bool, remoteDir, remoteCompose string) error {
	e.progress("cleanup", "started", "")
	// A step returns, when and only when it succeeded, the note for work it
	// deliberately declined to do; that note becomes the journal detail so a
	// skip is not recorded as an unqualified success. label reproduces the error
	// prefix each step had before it moved into this table.
	for _, step := range []struct {
		key   string
		label string
		run   func() (string, error)
	}{
		// The new release is verified and serving before an old workload is
		// drained. Run this before retention so the immutable predecessor snapshot
		// that supplies the old workload's drain policy is still available.
		{finalizeRetiredWorkloadsSubStep, "retired workloads", func() (string, error) {
			return "", e.retireRemovedWorkloads(ctx, manifest.Predecessor)
		}},
		{finalizeRetentionSubStep, "prune", func() (string, error) { return e.pruneRetention(ctx) }},
		// After activation, because a timer invokes the job through `current`
		// and that pointer has only just moved. Before the post-deploy hook, so
		// a hook that inspects the schedule sees the one this release declares.
		{finalizeSchedulesSubStep, "schedules", func() (string, error) { return "", e.SyncSchedules(ctx) }},
		{finalizePostDeploySubStep, "post-deploy", func() (string, error) {
			return "", e.RunHook(ctx, "post_deploy", remoteDir, remoteCompose)
		}},
	} {
		if done[step.key] {
			e.logf("%s: already complete (resume)", step.key)
			continue
		}
		if err := jw.Append(ctx, journal.Record{Phase: "finalize", SubStep: step.key, Event: "intent"}); err != nil {
			return e.failedOutcome(ctx, manifest, fmt.Errorf("journal %s intent: %w", step.key, err))
		}
		detail, err := step.run()
		result := journal.Record{Phase: "finalize", SubStep: step.key, Event: "result", Status: "ok"}
		switch {
		case err != nil:
			result.Status, result.Detail = "fail", err.Error()
		case detail != "":
			// A step that declined to act did not complete. Recording it as a
			// result would mark it done and skip it on every later finalize,
			// even once whatever blocked it is fixed — so it is journaled as
			// its own event that carries the reason and nothing else.
			result.Event, result.Status, result.Detail = "skip", "", detail
			// Structured consumers see engine progress messages and never the
			// warning on the local path, and a skipped step is exactly what an
			// operator running under --output ndjson has to be told.
			e.progress("cleanup", "skipped", step.label+": "+detail)
		}
		if journalErr := jw.Append(ctx, result); journalErr != nil {
			return e.failedOutcome(ctx, manifest, errors.Join(err, fmt.Errorf("journal %s result: %w", step.key, journalErr)))
		}
		if err != nil {
			e.progress("cleanup", "failed", "post-activation step "+step.label+" failed; the release stays serving — `ob resume` completes the rest")
			return e.failedOutcome(ctx, manifest, fmt.Errorf("%s: %w", step.label, err))
		}
	}
	if err := e.recordOutcome(ctx, manifest, release.OutcomeSucceeded); err != nil {
		// Also an exit from the post-activation steps, and the interrupt case
		// is real: every step has already run, so a bare context.Canceled
		// here would ship as "cancelled, nothing was changed" for a release
		// that is live, journalled and fully finalized but one write.
		return &PostActivationFailedError{ReleaseID: manifest.ID, Err: err}
	}
	e.progress("cleanup", "succeeded", "")
	return nil
}

// failedOutcome records that this operation failed after activation and returns
// the cause. Every exit from the post-activation steps goes through it,
// including the ones where the journal itself could not be written: the manifest
// is a different medium, and a release left claiming a finished operation is the
// state the outcome field exists to prevent.
func (e *Engine) failedOutcome(ctx context.Context, manifest *release.Manifest, cause error) error {
	return e.failedOutcomeWithGuidance(ctx, manifest, "", cause)
}

// failedOutcomeWithGuidance is failedOutcome for the exits whose published
// remedy is not the default `ob resume`.
func (e *Engine) failedOutcomeWithGuidance(ctx context.Context, manifest *release.Manifest, guidance string, cause error) error {
	if err := e.recordOutcome(ctx, manifest, release.OutcomeFailed); err != nil {
		// err is the failure to stamp the manifest, and it is the more
		// dangerous half: the release is left claiming a finished operation,
		// which is the state this outcome field exists to prevent. Joining
		// cause twice would report the step failure and lose it.
		return errors.Join(err, &PostActivationFailedError{ReleaseID: manifest.ID, Err: cause, Guidance: guidance})
	}
	return &PostActivationFailedError{ReleaseID: manifest.ID, Err: cause, Guidance: guidance}
}

// PostActivationFailedError reports that the work after activation did not
// finish. It is typed because the release is already serving: an interrupt here
// unwraps to context.Canceled, and without a code of its own the CLI reports
// outcome "cancelled" and exit 2 — "nothing was changed" — for a deploy whose
// release is live and whose manifest has just been stamped failed.
type PostActivationFailedError struct {
	ReleaseID string
	Err       error
	// Guidance overrides the published default. A rollback that could not
	// clear its checkpoint must not be handed `ob resume`: that fixes the
	// interrupted deploy forward, the opposite of the operation that just
	// ran, and finalize would refuse anyway because the checkpoint carries
	// the rollback target's id rather than the deploy's.
	Guidance string
}

func (err *PostActivationFailedError) Error() string {
	return fmt.Sprintf("release %s is serving, but the work after activation did not finish: %v", err.ReleaseID, err.Err)
}

func (err *PostActivationFailedError) Unwrap() error { return err.Err }

func (err *PostActivationFailedError) Code() string { return "post_activation_failed" }

func (err *PostActivationFailedError) GuidanceCommand() string {
	if err.Guidance != "" {
		return err.Guidance
	}
	return "ob resume --output ndjson"
}

// recordOutcome writes the terminal result of the post-activation steps without
// touching lifecycle state: a serving release stays serving even when the work
// after it failed. The outcome is evidence rather than an input — finalize finds
// the release through its serving state and an unfinished journal — and it is
// what keeps a healthy release distinguishable from a finished operation.
func (e *Engine) recordOutcome(ctx context.Context, manifest *release.Manifest, outcome release.OperationOutcome) error {
	if manifest.OperationOutcome == outcome {
		return nil
	}
	if err := manifest.RecordOperationOutcome(outcome, e.Opts.Now()); err != nil {
		return fmt.Errorf("record operation outcome: %w", err)
	}
	if err := e.writeReleaseManifest(ctx, *manifest); err != nil {
		return fmt.Errorf("record operation outcome: %w", err)
	}
	return nil
}
