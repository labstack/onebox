package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/release"
)

func (e *Engine) writeReleaseManifest(ctx context.Context, manifest release.Manifest) error {
	command, input, err := release.ManifestWrite(e.names(), manifest)
	if err != nil {
		return err
	}
	result, err := e.mutateInput(ctx, command, input)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		return fmt.Errorf("write release manifest %s failed (exit %d): %s", manifest.ID, result.ExitCode, detail)
	}
	return nil
}

func (e *Engine) writeActivationCheckpoint(ctx context.Context, checkpoint release.ActivationCheckpoint) error {
	command, input, err := release.ActivationCheckpointWrite(e.names(), checkpoint)
	if err != nil {
		return err
	}
	result, err := e.mutateInput(ctx, command, input)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		return fmt.Errorf("write activation checkpoint failed (exit %d): %s", result.ExitCode, detail)
	}
	return nil
}

func (e *Engine) clearActivationCheckpoint(ctx context.Context) error {
	return e.mutateChecked(ctx, "clear activation checkpoint", "rm -f "+q(release.ActivationCheckpointPath(e.names())))
}

func (e *Engine) newApplicationManifest(ctx context.Context, releaseID string) (release.Manifest, error) {
	manifest, err := release.NewManifest(releaseID, release.KindApplication, e.Opts.Now())
	if err != nil {
		return release.Manifest{}, err
	}
	if err := e.writeReleaseManifest(ctx, manifest); err != nil {
		return release.Manifest{}, err
	}
	return manifest, nil
}

func (e *Engine) requireServingApplicationManifest(ctx context.Context, releaseID string) error {
	if releaseID == "" {
		return nil
	}
	manifest, err := release.ReadManifest(ctx, e.T, e.names(), releaseID)
	if err != nil {
		return fmt.Errorf("current release %s has no usable manifest: %w", releaseID, err)
	}
	if manifest.Kind != release.KindApplication || manifest.State != release.StateServing {
		return fmt.Errorf("current release %s has manifest state %s/%s; activation is incomplete", releaseID, manifest.Kind, manifest.State)
	}
	return nil
}

func (e *Engine) requireRecoverableApplicationManifest(ctx context.Context, releaseID string) error {
	if releaseID == "" {
		return nil
	}
	manifest, err := release.ReadManifest(ctx, e.T, e.names(), releaseID)
	if err != nil {
		return fmt.Errorf("recovery release %s has no usable manifest: %w", releaseID, err)
	}
	if manifest.Kind != release.KindApplication || (manifest.State != release.StateServing && manifest.State != release.StateSuperseded) {
		return fmt.Errorf("recovery release %s has manifest state %s/%s and is not recoverable", releaseID, manifest.Kind, manifest.State)
	}
	return nil
}

func (e *Engine) resumeApplicationManifest(ctx context.Context, releaseID string) (release.Manifest, error) {
	manifest, err := release.ReadManifest(ctx, e.T, e.names(), releaseID)
	if err != nil {
		return release.Manifest{}, fmt.Errorf("resume release %s has no usable manifest: %w", releaseID, err)
	}
	if manifest.Kind != release.KindApplication {
		return release.Manifest{}, fmt.Errorf("release %s is %s, not an application release", releaseID, manifest.Kind)
	}
	return manifest, nil
}

// ActivationRefusedError reports a release whose manifest state cannot be
// activated. It is typed so an agent can branch on activation_refused rather
// than parse prose. State is carried for diagnosis, not because the remedy
// varies: it is only constructed for states that cannot resume, and they all
// need `ob abort`.
type ActivationRefusedError struct {
	ReleaseID string
	State     release.State
}

func (err *ActivationRefusedError) Error() string {
	return fmt.Sprintf("release %s cannot activate from manifest state %s; recover it with `ob abort`", err.ReleaseID, err.State)
}

func (err *ActivationRefusedError) Code() string { return "activation_refused" }

// activationResumable reports whether a manifest can enter activation. Verified
// is included because activation writes that state before it switches the
// symlink: a runner that dies in between leaves a verified manifest, and the
// halt guidance tells the operator to fix forward and resume. Refusing verified
// made that instruction impossible to follow — only `ob abort` could clear it.
func activationResumable(state release.State) bool {
	return state == release.StateStaged || state == release.StateVerified
}

func (e *Engine) activateManifest(ctx context.Context, manifest *release.Manifest, predecessor string) error {
	if manifest == nil {
		return fmt.Errorf("activation requires an application manifest")
	}
	if !activationResumable(manifest.State) {
		return &ActivationRefusedError{ReleaseID: manifest.ID, State: manifest.State}
	}
	checkpoint, err := e.beginActivationCheckpoint(ctx, manifest.ID, predecessor)
	if err != nil {
		return err
	}
	// Already verified means this is a re-entry after a crash inside the
	// activation window. The state is durable, so record it once and carry on
	// from the checkpoint rather than transitioning a second time.
	if manifest.State == release.StateStaged {
		if err := manifest.Transition(release.StateVerified, e.Opts.Now(), ""); err != nil {
			return err
		}
		if err := e.writeReleaseManifest(ctx, *manifest); err != nil {
			return fmt.Errorf("record verified release: %w", err)
		}
	}
	if err := e.checkpointVerified(ctx, &checkpoint); err != nil {
		return err
	}
	return e.completeActivation(ctx, manifest, predecessor, &checkpoint)
}

func (e *Engine) reactivateManifest(ctx context.Context, manifest *release.Manifest, predecessor string) error {
	if manifest == nil || manifest.State != release.StateSuperseded || !releaseManifestProvesServing(*manifest) {
		return fmt.Errorf("rollback activation requires a previously serving superseded application manifest")
	}
	checkpoint, err := e.beginActivationCheckpoint(ctx, manifest.ID, predecessor)
	if err != nil {
		return err
	}
	if err := e.checkpointVerified(ctx, &checkpoint); err != nil {
		return err
	}
	return e.completeActivation(ctx, manifest, predecessor, &checkpoint)
}

func releaseManifestProvesServing(manifest release.Manifest) bool {
	for _, transition := range manifest.Transitions {
		if transition.State == release.StateServing {
			return true
		}
	}
	return false
}

func (e *Engine) beginActivationCheckpoint(ctx context.Context, releaseID, predecessor string) (release.ActivationCheckpoint, error) {
	checkpoint, err := release.NewActivationCheckpoint(releaseID, predecessor, release.ActivationPrepared, e.Opts.Now())
	if err != nil {
		return release.ActivationCheckpoint{}, err
	}
	if err := e.writeActivationCheckpoint(ctx, checkpoint); err != nil {
		return release.ActivationCheckpoint{}, fmt.Errorf("checkpoint prepared release: %w", err)
	}
	return checkpoint, nil
}

func (e *Engine) checkpointVerified(ctx context.Context, checkpoint *release.ActivationCheckpoint) error {
	if err := checkpoint.Advance(release.ActivationVerified, e.Opts.Now()); err != nil {
		return err
	}
	if err := e.writeActivationCheckpoint(ctx, *checkpoint); err != nil {
		return fmt.Errorf("checkpoint verified release: %w", err)
	}
	return nil
}

func (e *Engine) completeActivation(ctx context.Context, manifest *release.Manifest, predecessor string, checkpoint *release.ActivationCheckpoint) error {
	if err := e.activate(ctx, manifest.ID); err != nil {
		return err
	}
	if err := checkpoint.Advance(release.ActivationSymlinkSwitched, e.Opts.Now()); err != nil {
		return err
	}
	if err := e.writeActivationCheckpoint(ctx, *checkpoint); err != nil {
		return fmt.Errorf("checkpoint current symlink: %w", err)
	}
	if err := manifest.Transition(release.StateServing, e.Opts.Now(), predecessor); err != nil {
		return err
	}
	if err := e.writeReleaseManifest(ctx, *manifest); err != nil {
		return fmt.Errorf("record serving release: %w", err)
	}
	if err := checkpoint.Advance(release.ActivationServingRecorded, e.Opts.Now()); err != nil {
		return err
	}
	if err := e.writeActivationCheckpoint(ctx, *checkpoint); err != nil {
		return fmt.Errorf("checkpoint serving manifest: %w", err)
	}

	if predecessor != "" {
		previous, err := release.ReadManifest(ctx, e.T, e.names(), predecessor)
		if err != nil {
			return fmt.Errorf("read predecessor manifest %s: %w", predecessor, err)
		}
		if previous.Kind != release.KindApplication || previous.State != release.StateServing {
			return fmt.Errorf("predecessor %s is not a serving application release", predecessor)
		}
		if err := previous.Transition(release.StateSuperseded, e.Opts.Now(), ""); err != nil {
			return err
		}
		if err := e.writeReleaseManifest(ctx, previous); err != nil {
			return fmt.Errorf("record superseded predecessor: %w", err)
		}
	}
	if err := checkpoint.Advance(release.ActivationPredecessorSuperseded, e.Opts.Now()); err != nil {
		return err
	}
	if err := e.writeActivationCheckpoint(ctx, *checkpoint); err != nil {
		return fmt.Errorf("checkpoint predecessor superseded: %w", err)
	}
	// The checkpoint is NOT cleared here. Callers clear it once the operation's
	// own journal records the activation, because those two writes bracket the
	// only unrecoverable window in the sequence: cleared checkpoint + serving
	// manifest + no journal evidence is a state finalize refuses on every
	// retry ("its journal records no successful activation") while the release
	// is healthy and live, and the refusal's guidance would roll it back.
	// Clearing last leaves the opposite window instead — checkpoint open,
	// activation journalled — which recovery is built to reconcile.
	return nil
}
