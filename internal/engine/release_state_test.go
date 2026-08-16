package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

func releaseStateTestEngine(t *testing.T, target *transport.Fake) *Engine {
	t.Helper()
	return New(testConfig(), testProject(t), target, Options{
		Out: &bytes.Buffer{}, Sleep: noSleep,
		Now: func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) },
	})
}

func stagedManifest(t *testing.T, id string) release.Manifest {
	t.Helper()
	manifest, err := release.NewManifest(id, release.KindApplication, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestCurrentReleaseWithoutManifestFailsClosed(t *testing.T) {
	target := &transport.Fake{}
	engine := releaseStateTestEngine(t, target)
	err := engine.requireServingApplicationManifest(context.Background(), engineTestPreviousReleaseID)
	if err == nil {
		t.Fatal("manifest-less current release was accepted")
	}
	var typed *release.ManifestError
	if !errors.As(err, &typed) || typed.Code() != "manifest_missing" {
		t.Fatalf("error = %v, want manifest_missing", err)
	}
	if len(target.Inputs) != 0 || len(target.Uploads) != 0 {
		t.Fatalf("manifest validation caused a mutation: inputs=%d uploads=%d", len(target.Inputs), len(target.Uploads))
	}
}

func TestResumeReleaseWithoutManifestFailsClosed(t *testing.T) {
	target := &transport.Fake{}
	engine := releaseStateTestEngine(t, target)
	_, err := engine.resumeApplicationManifest(context.Background(), engineTestDeployReleaseID)
	if err == nil {
		t.Fatal("manifest-less interrupted release was materialized")
	}
	var typed *release.ManifestError
	if !errors.As(err, &typed) || typed.Code() != "manifest_missing" {
		t.Fatalf("error = %v, want manifest_missing", err)
	}
	if len(target.Inputs) != 0 || len(target.Uploads) != 0 {
		t.Fatalf("resume manifest validation caused a mutation: inputs=%d uploads=%d", len(target.Inputs), len(target.Uploads))
	}
}

func TestActivateManifestPersistsEveryBoundaryInOrder(t *testing.T) {
	target := happyFake()
	engine := releaseStateTestEngine(t, target)
	manifest := stagedManifest(t, engineTestDeployReleaseID)
	if err := engine.writeReleaseManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := engine.activateManifest(context.Background(), &manifest, ""); err != nil {
		t.Fatalf("activate: %v\n%s", err, strings.Join(target.Commands, "\n"))
	}

	var checkpointPhases []string
	var manifestStates []string
	for _, input := range target.Inputs {
		if checkpoint, err := release.DecodeActivationCheckpoint([]byte(input)); err == nil {
			checkpointPhases = append(checkpointPhases, string(checkpoint.Phase))
			continue
		}
		if persisted, err := release.DecodeManifest([]byte(input)); err == nil {
			manifestStates = append(manifestStates, string(persisted.State))
		}
	}
	wantPhases := []string{
		string(release.ActivationPrepared),
		string(release.ActivationVerified),
		string(release.ActivationSymlinkSwitched),
		string(release.ActivationServingRecorded),
		string(release.ActivationPredecessorSuperseded),
	}
	if strings.Join(checkpointPhases, ",") != strings.Join(wantPhases, ",") {
		t.Fatalf("checkpoint phases = %v, want %v", checkpointPhases, wantPhases)
	}
	if got := strings.Join(manifestStates, ","); got != "staged,verified,serving" {
		t.Fatalf("manifest states = %s", got)
	}
	commands := strings.Join(target.Commands, "\n")
	if !strings.Contains(commands, "ln -sfn 'releases/"+engineTestDeployReleaseID+"'") {
		t.Fatalf("activation did not switch the current symlink:\n%s", commands)
	}
	// The checkpoint deliberately outlives activateManifest. Clearing it here
	// would leave a window where the release is serving, the checkpoint is
	// gone, and nothing has journalled the activation — a state finalize
	// refuses on every retry while the release is healthy and live. The
	// caller clears it once that evidence is durable.
	if strings.Contains(commands, "rm -f '/var/lib/ob/sample/activation.json'") {
		t.Fatalf("activation cleared its own checkpoint before any evidence was journalled:\n%s", commands)
	}
	if _, err := release.ReadActivationCheckpoint(context.Background(), target, engine.Names()); err != nil {
		t.Fatalf("activation must leave its checkpoint for the caller to clear: %v", err)
	}
	stored, err := release.ReadManifest(context.Background(), target, engine.Names(), engineTestDeployReleaseID)
	if err != nil || stored.State != release.StateServing {
		t.Fatalf("stored manifest = %+v, %v", stored, err)
	}
}

func TestActivateManifestCrashAfterSupersedingPredecessorKeepsCheckpoint(t *testing.T) {
	target := happyFake()
	engine := releaseStateTestEngine(t, target)
	previous := stagedManifest(t, engineTestPreviousReleaseID)
	if err := previous.Transition(release.StateVerified, engine.Opts.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if err := previous.Transition(release.StateServing, engine.Opts.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if err := engine.writeReleaseManifest(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	manifest := stagedManifest(t, engineTestDeployReleaseID)
	if err := engine.writeReleaseManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		latestInput := ""
		if len(target.Inputs) > 0 {
			latestInput = target.Inputs[len(target.Inputs)-1]
		}
		if strings.Contains(command, "/activation.json.tmp") && strings.Contains(latestInput, `"phase": "predecessor_superseded"`) {
			return transport.Result{ExitCode: 74, Stderr: "simulated crash"}, true
		}
		return base(command)
	}
	if err := engine.activateManifest(context.Background(), &manifest, engineTestPreviousReleaseID); err == nil {
		t.Fatal("activation unexpectedly completed")
	}
	checkpoint, err := release.ReadActivationCheckpoint(context.Background(), target, engine.Names())
	if err != nil || checkpoint.Phase != release.ActivationServingRecorded {
		t.Fatalf("checkpoint = %+v, %v", checkpoint, err)
	}
	storedPrevious, err := release.ReadManifest(context.Background(), target, engine.Names(), engineTestPreviousReleaseID)
	if err != nil || storedPrevious.State != release.StateSuperseded {
		t.Fatalf("predecessor = %+v, %v", storedPrevious, err)
	}
	if strings.Contains(strings.Join(target.Commands, "\n"), "rm -f '/var/lib/ob/sample/activation.json'") {
		t.Fatal("failed final checkpoint was cleared")
	}
}

func TestActivateManifestCrashLeavesLastDurableBoundary(t *testing.T) {
	for _, test := range []struct {
		name           string
		failCommand    string
		failInput      string
		wantCheckpoint release.ActivationPhase
		wantManifest   release.State
	}{
		{name: "before symlink", failCommand: "ln -sfn", wantCheckpoint: release.ActivationVerified, wantManifest: release.StateVerified},
		{name: "before serving manifest", failCommand: "/manifest.json.tmp", failInput: `"state": "serving"`, wantCheckpoint: release.ActivationSymlinkSwitched, wantManifest: release.StateVerified},
		{name: "before serving checkpoint", failCommand: "/activation.json.tmp", failInput: `"phase": "serving_recorded"`, wantCheckpoint: release.ActivationSymlinkSwitched, wantManifest: release.StateServing},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := happyFake()
			base := target.Dynamic
			target.Dynamic = func(command string) (transport.Result, bool) {
				latestInput := ""
				if len(target.Inputs) > 0 {
					latestInput = target.Inputs[len(target.Inputs)-1]
				}
				if strings.Contains(command, test.failCommand) && (test.failInput == "" || strings.Contains(latestInput, test.failInput)) {
					return transport.Result{ExitCode: 74, Stderr: "simulated crash"}, true
				}
				return base(command)
			}
			engine := releaseStateTestEngine(t, target)
			manifest := stagedManifest(t, engineTestDeployReleaseID)
			if err := engine.writeReleaseManifest(context.Background(), manifest); err != nil {
				t.Fatal(err)
			}
			if err := engine.activateManifest(context.Background(), &manifest, ""); err == nil {
				t.Fatal("activation unexpectedly completed")
			}
			checkpoint, err := release.ReadActivationCheckpoint(context.Background(), target, engine.Names())
			if err != nil || checkpoint.Phase != test.wantCheckpoint {
				t.Fatalf("checkpoint = %+v, %v; want %s", checkpoint, err, test.wantCheckpoint)
			}
			stored, err := release.ReadManifest(context.Background(), target, engine.Names(), engineTestDeployReleaseID)
			if err != nil || stored.State != test.wantManifest {
				t.Fatalf("manifest = %+v, %v; want %s", stored, err, test.wantManifest)
			}
			if strings.Contains(strings.Join(target.Commands, "\n"), "rm -f '/var/lib/ob/sample/activation.json'") {
				t.Fatal("failed activation cleared its recovery checkpoint")
			}
		})
	}
}

func TestActivateManifestPredecessorWriteFailureLeavesTwoServingManifestsRecoverable(t *testing.T) {
	target := happyFake()
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		latestInput := ""
		if len(target.Inputs) > 0 {
			latestInput = target.Inputs[len(target.Inputs)-1]
		}
		if strings.Contains(command, "/manifest.json.tmp") &&
			strings.Contains(latestInput, `"id": "`+engineTestPreviousReleaseID+`"`) &&
			strings.Contains(latestInput, `"state": "superseded"`) {
			return transport.Result{ExitCode: 74, Stderr: "simulated predecessor manifest failure"}, true
		}
		return base(command)
	}
	engine := releaseStateTestEngine(t, target)
	manifest := stagedManifest(t, engineTestDeployReleaseID)
	if err := engine.writeReleaseManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := engine.activateManifest(context.Background(), &manifest, engineTestPreviousReleaseID); err == nil {
		t.Fatal("activation unexpectedly completed")
	}
	checkpoint, err := release.ReadActivationCheckpoint(context.Background(), target, engine.Names())
	if err != nil || checkpoint.Phase != release.ActivationServingRecorded {
		t.Fatalf("checkpoint = %+v, %v", checkpoint, err)
	}
	for _, releaseID := range []string{engineTestDeployReleaseID, engineTestPreviousReleaseID} {
		stored, err := release.ReadManifest(context.Background(), target, engine.Names(), releaseID)
		if err != nil || stored.State != release.StateServing {
			t.Fatalf("manifest %s = %+v, %v; want recoverable serving state", releaseID, stored, err)
		}
	}
	if strings.Contains(strings.Join(target.Commands, "\n"), "rm -f '/var/lib/ob/sample/activation.json'") {
		t.Fatal("two-serving-manifest crash state lost its recovery checkpoint")
	}
}

func TestRollbackRefusesSupersededManifestThatNeverProvedServing(t *testing.T) {
	target := happyFake()
	engine := releaseStateTestEngine(t, target)
	manifest := stagedManifest(t, engineTestDeployReleaseID)
	manifest.State = release.StateSuperseded
	manifest.Transitions[0].State = release.StateSuperseded
	if err := engine.reactivateManifest(context.Background(), &manifest, engineTestPreviousReleaseID); err == nil || !strings.Contains(err.Error(), "previously serving") {
		t.Fatalf("never-served rollback target was accepted: %v", err)
	}
	if len(target.Commands) != 0 {
		t.Fatalf("refused rollback touched target: %#v", target.Commands)
	}
}

// Activation writes the verified manifest before it switches the symlink, so a
// runner that dies in between leaves a verified manifest. The halt guidance
// tells the operator to fix forward and resume, which was impossible while
// activation accepted only staged manifests: every retry refused and `ob abort`
// was the sole way out.
func TestActivateManifestResumesFromAVerifiedManifest(t *testing.T) {
	target := happyFake()
	engine := releaseStateTestEngine(t, target)
	manifest := stagedManifest(t, engineTestDeployReleaseID)
	if err := manifest.Transition(release.StateVerified, engine.Opts.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if err := engine.writeReleaseManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	before := len(target.Inputs)

	if err := engine.activateManifest(context.Background(), &manifest, ""); err != nil {
		t.Fatalf("resume from verified: %v\n%s", err, strings.Join(target.Commands, "\n"))
	}
	if manifest.State != release.StateServing {
		t.Fatalf("manifest state = %q, want serving", manifest.State)
	}
	// The transition is recorded once. A second verified entry would mean the
	// re-entry rewrote history it had already written.
	verified := 0
	for _, input := range target.Inputs[before:] {
		if decoded, err := release.DecodeManifest([]byte(input)); err == nil && decoded.State == release.StateVerified {
			verified++
		}
	}
	if verified != 0 {
		t.Fatalf("re-entry rewrote the verified transition %d time(s)", verified)
	}
	if commands := strings.Join(target.Commands, "\n"); !strings.Contains(commands, "ln -sfn") {
		t.Fatalf("re-entry did not reach the symlink switch:\n%s", commands)
	}
}

func TestActivateManifestRefusesAStateItCannotResume(t *testing.T) {
	target := happyFake()
	engine := releaseStateTestEngine(t, target)
	manifest := stagedManifest(t, engineTestDeployReleaseID)
	for _, state := range []release.State{release.StateVerified, release.StateServing, release.StateSuperseded} {
		if err := manifest.Transition(state, engine.Opts.Now(), ""); err != nil {
			t.Fatal(err)
		}
	}
	err := engine.activateManifest(context.Background(), &manifest, "")
	var refused *ActivationRefusedError
	if !errors.As(err, &refused) || refused.Code() != "activation_refused" {
		t.Fatalf("error = %v, want a typed activation_refused", err)
	}
	if refused.State != release.StateSuperseded {
		t.Fatalf("refusal reported state %q", refused.State)
	}
}
