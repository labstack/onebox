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
// as ok runs again, which is what makes a fix-forward resume work. Retention
// and schedule sync are idempotent; the post-deploy hook is not, which is the
// whole reason these keys exist.
const (
	finalizeRetentionSubStep  = journal.FinalizeSubStepPrefix + "retention"
	finalizeSchedulesSubStep  = journal.FinalizeSubStepPrefix + "schedules"
	finalizePostDeploySubStep = journal.FinalizeSubStepPrefix + "post_deploy"
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
	if err := e.requireActivationEvidence(ctx, manifest, done); err != nil {
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
		return e.failedOutcome(ctx, manifest, fmt.Errorf("verify serving release: %w", err))
	}
	vf(nil)
	if err := jw.Append(ctx, journal.Record{Phase: "verify", Event: "result", Status: "ok"}); err != nil {
		return fmt.Errorf("journal verify result: %w", err)
	}
	e.progress("verification", "succeeded", "")
	return e.runPostActivation(ctx, jw, manifest, done, remoteDir, remoteCompose)
}

// requireActivationEvidence proves from durable evidence alone that the release
// now serving was activated by the operation being finalized. Every source must
// agree; one disagreement refuses. A serving manifest on its own proves only
// that some operation activated this release, which is not the same claim.
func (e *Engine) requireActivationEvidence(ctx context.Context, manifest *release.Manifest, done map[string]bool) error {
	refuse := func(format string, a ...any) error {
		return &FinalizeRefusedError{ReleaseID: manifest.ID, Reason: fmt.Sprintf(format, a...)}
	}
	if !done[journal.DoneActivation] {
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
	_, checkpointErr := release.ReadActivationCheckpoint(ctx, e.T, e.names())
	switch {
	case checkpointErr == nil:
		return refuse("an activation checkpoint is still open")
	case !errors.Is(checkpointErr, release.ErrActivationCheckpointMissing):
		return fmt.Errorf("read activation checkpoint: %w", checkpointErr)
	}
	return e.requireLiveRelease(ctx, manifest.ID)
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
		return err
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
	if err := e.recordOutcome(ctx, manifest, release.OutcomeFailed); err != nil {
		return errors.Join(cause, err)
	}
	return cause
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
