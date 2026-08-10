package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

func seedInterruptedRecoveryState(t *testing.T, engine *Engine) {
	t.Helper()
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	previous, err := release.NewManifest(engineTestPreviousReleaseID, release.KindApplication, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := previous.Transition(release.StateVerified, at, ""); err != nil {
		t.Fatal(err)
	}
	if err := previous.Transition(release.StateServing, at, ""); err != nil {
		t.Fatal(err)
	}
	interrupted, err := release.NewManifest(engineTestDeployReleaseID, release.KindApplication, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.writeReleaseManifest(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	if err := engine.writeReleaseManifest(context.Background(), interrupted); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := release.NewActivationCheckpoint(engineTestDeployReleaseID, engineTestPreviousReleaseID, release.ActivationPrepared, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.writeActivationCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
}

func recoveryWriter(engine *Engine) *journal.Writer {
	return &journal.Writer{T: engine.T, Names: engine.Names(), DeployID: engineTestDeployReleaseID, Epoch: 2}
}

func TestRecoveryRetryKeepsCheckpointUntilHealthyAndSweepsStaleRoles(t *testing.T) {
	target := happyFake()
	verifyCalls := 0
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
		case strings.Contains(command, "docker ps -aq") && strings.Contains(command, "label=ob.release='"+engineTestDeployReleaseID+"'"):
			var ids []string
			for _, pair := range []struct{ id, marker string }{{"NEW1", "docker rm -f NEW1"}, {"STALE1", "docker rm -f STALE1"}} {
				removed := false
				for _, recorded := range target.Commands {
					if strings.Contains(recorded, pair.marker) {
						removed = true
						break
					}
				}
				if !removed {
					ids = append(ids, pair.id)
				}
			}
			return transport.Result{Stdout: strings.Join(ids, "\n")}, true
		case strings.Contains(command, "curl -fsS"):
			verifyCalls++
			if verifyCalls == 1 {
				return transport.Result{ExitCode: 22, Stderr: "not healthy yet"}, true
			}
			return transport.Result{}, true
		}
		return base(command)
	}
	engine := New(testConfig(), testProject(t), target, Options{
		Out: &bytes.Buffer{}, Sleep: noSleep,
		Now: func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) },
	})
	seedInterruptedRecoveryState(t, engine)
	request := recoveryRequest{
		InterruptedID: engineTestDeployReleaseID,
		PreviousID:    engineTestPreviousReleaseID,
		TerminalState: release.StateAborted,
		GateCovered:   true,
		Phase:         "abort",
		Journal:       recoveryWriter(engine),
	}
	err := engine.recoverInterrupted(context.Background(), request)
	var incomplete *RecoveryIncompleteError
	if !errors.As(err, &incomplete) || incomplete.Code() != "recovery_incomplete" || incomplete.Phase != "verify" {
		t.Fatalf("first recovery error = %#v", err)
	}
	if _, err := release.ReadActivationCheckpoint(context.Background(), target, engine.Names()); err != nil {
		t.Fatalf("failed recovery cleared checkpoint: %v", err)
	}
	if commands := strings.Join(target.Commands, "\n"); !strings.Contains(commands, "docker rm -f NEW1") || !strings.Contains(commands, "docker rm -f STALE1") {
		t.Fatalf("exact release sweep missed a stale role:\n%s", commands)
	}

	if err := engine.recoverInterrupted(context.Background(), request); err != nil {
		t.Fatalf("retry recovery: %v\n%s", err, strings.Join(target.Commands, "\n"))
	}
	if _, err := release.ReadActivationCheckpoint(context.Background(), target, engine.Names()); !errors.Is(err, release.ErrActivationCheckpointMissing) {
		t.Fatalf("healthy recovery left checkpoint: %v", err)
	}
	manifest, err := release.ReadManifest(context.Background(), target, engine.Names(), engineTestDeployReleaseID)
	if err != nil || manifest.State != release.StateAborted || manifest.OperationOutcome != release.OutcomeAborted {
		t.Fatalf("interrupted manifest = %+v, %v", manifest, err)
	}
}

func TestRecoveryMigrationGateStopsBeforeMutationUnlessBreakGlass(t *testing.T) {
	target := happyFake()
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	request := recoveryRequest{
		InterruptedID: engineTestDeployReleaseID,
		PreviousID:    engineTestPreviousReleaseID,
		TerminalState: release.StateAborted,
		Phase:         "abort",
		Journal:       recoveryWriter(engine),
	}
	err := engine.recoverInterrupted(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("closed-gate error = %v", err)
	}
	for _, command := range target.Commands {
		if strings.Contains(command, "docker ps -aq") || strings.Contains(command, `"phase":"abort","event":"intent"`) {
			t.Fatalf("closed gate mutated or journaled recovery intent: %s", command)
		}
	}

	t.Run("break glass", func(t *testing.T) {
		target := happyFake()
		base := target.Dynamic
		target.Dynamic = func(command string) (transport.Result, bool) {
			if strings.Contains(command, "readlink") {
				return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
			}
			if strings.Contains(command, "docker ps -aq") && strings.Contains(command, "label=ob.release='"+engineTestDeployReleaseID+"'") {
				return transport.Result{}, true
			}
			return base(command)
		}
		engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
		seedInterruptedRecoveryState(t, engine)
		request := recoveryRequest{
			InterruptedID: engineTestDeployReleaseID,
			PreviousID:    engineTestPreviousReleaseID,
			TerminalState: release.StateAborted,
			BreakGlass:    true,
			Phase:         "abort",
			Journal:       recoveryWriter(engine),
		}
		if err := engine.recoverInterrupted(context.Background(), request); err != nil {
			t.Fatalf("break-glass recovery: %v", err)
		}
		if !strings.Contains(strings.Join(target.Commands, "\n"), `"phase":"abort","event":"intent"`) {
			t.Fatal("authorized break-glass recovery did not start")
		}
	})
}

func TestFinalizeRecoveredFirstReleaseClearsCurrentAndFailsManifest(t *testing.T) {
	target := happyFake()
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "readlink") {
			return transport.Result{Stdout: "releases/" + engineTestDeployReleaseID + "\n"}, true
		}
		return base(command)
	}
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Now: func() time.Time {
		return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	}})
	manifest, err := release.NewManifest(engineTestDeployReleaseID, release.KindApplication, engine.Opts.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.writeReleaseManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := engine.finalizeRecoveredRelease(context.Background(), engineTestDeployReleaseID, "", release.StateFailed); err != nil {
		t.Fatal(err)
	}
	stored, err := release.ReadManifest(context.Background(), target, engine.Names(), engineTestDeployReleaseID)
	if err != nil || stored.State != release.StateFailed || stored.OperationOutcome != release.OutcomeFailed {
		t.Fatalf("finalized manifest = %+v, %v", stored, err)
	}
	if !strings.Contains(strings.Join(target.Commands, "\n"), "rm -f '/var/lib/ob/sample/current'") {
		t.Fatal("first-release recovery did not clear current")
	}
}

func TestFinalizeRecoveredReleaseReactivatesSupersededPredecessor(t *testing.T) {
	target := happyFake()
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "readlink") {
			return transport.Result{Stdout: "releases/" + engineTestDeployReleaseID + "\n"}, true
		}
		return base(command)
	}
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Now: func() time.Time { return now }})
	previous, err := release.NewManifest(engineTestPreviousReleaseID, release.KindApplication, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []release.State{release.StateVerified, release.StateServing, release.StateSuperseded} {
		if err := previous.Transition(state, now, ""); err != nil {
			t.Fatal(err)
		}
	}
	interrupted, err := release.NewManifest(engineTestDeployReleaseID, release.KindApplication, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := interrupted.Transition(release.StateVerified, now, ""); err != nil {
		t.Fatal(err)
	}
	if err := interrupted.Transition(release.StateServing, now, engineTestPreviousReleaseID); err != nil {
		t.Fatal(err)
	}
	for _, manifest := range []release.Manifest{previous, interrupted} {
		if err := engine.writeReleaseManifest(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.finalizeRecoveredRelease(context.Background(), engineTestDeployReleaseID, engineTestPreviousReleaseID, release.StateFailed); err != nil {
		t.Fatal(err)
	}
	previous, err = release.ReadManifest(context.Background(), target, engine.Names(), engineTestPreviousReleaseID)
	if err != nil || previous.State != release.StateServing {
		t.Fatalf("predecessor = %+v, %v", previous, err)
	}
	interrupted, err = release.ReadManifest(context.Background(), target, engine.Names(), engineTestDeployReleaseID)
	if err != nil || interrupted.State != release.StateSuperseded || interrupted.OperationOutcome != release.OutcomeFailed {
		t.Fatalf("interrupted = %+v, %v", interrupted, err)
	}
	if !strings.Contains(strings.Join(target.Commands, "\n"), "ln -sfn 'releases/"+engineTestPreviousReleaseID+"'") {
		t.Fatal("recovery did not reactivate predecessor")
	}
}

func TestRecoverySnapshotRejectsAnotherApplication(t *testing.T) {
	target := happyFake()
	target.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "/ob.snapshot.yml") {
			return transport.Result{Stdout: strings.Replace(engineProject, "app: sample", "app: other", 1)}, true
		}
		return transport.Result{}, false
	}
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	_, err := engine.engineFromReleaseSnapshot(context.Background(), engineTestPreviousReleaseID)
	if err == nil || !strings.Contains(err.Error(), "expected app") {
		t.Fatalf("cross-application snapshot was accepted: %v", err)
	}
}
