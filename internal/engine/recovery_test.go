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
	return &journal.Writer{T: engine.T, Dir: journal.Dir(engine.Names()), DeployID: engineTestDeployReleaseID, Epoch: 2}
}

func TestRecoveryRetryKeepsCheckpointUntilHealthyAndSweepsStaleRoles(t *testing.T) {
	target := happyFake()
	verifyCalls := 0
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
		case strings.Contains(command, "docker ps -a --filter label=onebox.app="):
			return transport.Result{Stdout: "NEW1|web|demo-web-1|running\nSTALE1|stale|demo-stale-1|running"}, true
		case strings.Contains(command, "docker ps -aq") && strings.Contains(command, "label=onebox.release='"+engineTestDeployReleaseID+"'"):
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
	var output bytes.Buffer
	engine := New(testConfig(), testProject(t), target, Options{
		Out: &output, Sleep: noSleep,
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
	commands := strings.Join(target.Commands, "\n")
	if strings.Index(commands, "docker rm -f STALE1") > strings.Index(commands, "--scale web=") ||
		strings.Index(commands, "docker rm -f NEW1") < strings.Index(commands, "--scale web=") {
		t.Fatalf("recovery cleanup order:\n%s", commands)
	}
	if strings.Count(output.String(), "✓ web rolling") != 1 {
		t.Fatalf("restored workload must leave one completed step before verification: %s", output.String())
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
			if strings.Contains(command, "docker ps -aq") && strings.Contains(command, "label=onebox.release='"+engineTestDeployReleaseID+"'") {
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
	if !strings.Contains(strings.Join(target.Commands, "\n"), "rm -f '/var/lib/onebox/app/current'") {
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
		if strings.Contains(command, "/onebox.snapshot.yml") {
			return transport.Result{Stdout: strings.Replace(engineProject, "name: sample", "name: other", 1)}, true
		}
		return transport.Result{}, false
	}
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	_, err := engine.engineFromReleaseSnapshot(context.Background(), engineTestPreviousReleaseID)
	if err == nil || !strings.Contains(err.Error(), "expected app") {
		t.Fatalf("cross-application snapshot was accepted: %v", err)
	}
}

func TestRecoveryEngineUsesSnapshotChoreography(t *testing.T) {
	snapshot := strings.Replace(engineProject, "    worker:\n", "    legacy:\n      role: Worker\n      image: ghcr.io/x/legacy:v1\n      command: legacy-work\n      strategy: Recreate\n    worker:\n", 1)
	snapshot = strings.Replace(snapshot, "order: [web, worker]", "order: [legacy, worker, web]", 1)
	target := happyFake()
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "/onebox.snapshot.yml") {
			return transport.Result{Stdout: snapshot}, true
		}
		if strings.Contains(command, "readlink") {
			return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
		}
		if strings.Contains(command, "service='legacy'") && strings.Contains(command, "onebox.release=") {
			return transport.Result{}, true
		}
		if strings.Contains(command, "service='legacy'") {
			for _, recorded := range target.Commands {
				if strings.Contains(recorded, "--force-recreate --timeout 30 legacy") {
					return transport.Result{Stdout: "L1\n"}, true
				}
			}
			return transport.Result{}, true
		}
		return base(command)
	}
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	replay, err := engine.engineFromReleaseSnapshot(context.Background(), engineTestPreviousReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(replay.Spec.Deployment.Order, ","); got != "legacy,worker,web" {
		t.Fatalf("recovery order = %q, want deployed snapshot order", got)
	}
	if got := strings.Join(engine.Spec.Deployment.Order, ","); got != "web,worker" {
		t.Fatalf("recovery mutated working-tree engine order: %q", got)
	}
	seedInterruptedRecoveryState(t, engine)
	if err := engine.recoverInterrupted(context.Background(), recoveryRequest{
		InterruptedID: engineTestDeployReleaseID,
		PreviousID:    engineTestPreviousReleaseID,
		TerminalState: release.StateAborted,
		GateCovered:   true,
		Phase:         "abort",
		Journal:       recoveryWriter(engine),
	}); err != nil {
		t.Fatalf("recover with deployed snapshot: %v\n%s", err, strings.Join(target.Commands, "\n"))
	}
	if commands := strings.Join(target.Commands, "\n"); !strings.Contains(commands, "--force-recreate --timeout 30 legacy") {
		t.Fatalf("recovery omitted snapshot-only role:\n%s", commands)
	}
}

func TestRestoreReleaseRolesRetainsMatchingTargetRevisions(t *testing.T) {
	target := happyFake()
	webRevision := "sha256:" + strings.Repeat("a", 64)
	workerRevision := "sha256:" + strings.Repeat("b", 64)
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "/compose.yaml") && strings.HasPrefix(command, "cat "):
			return transport.Result{Stdout: "services:\n  web:\n    labels:\n      onebox.workload-revision: " + webRevision + "\n  worker:\n    labels:\n      onebox.workload-revision: " + workerRevision + "\n"}, true
		case strings.Contains(command, "docker ps --filter label=onebox.app="):
			return transport.Result{Stdout: "WEB1|web|older-release|" + webRevision + "|Up 1 hour (healthy)\n" +
				"WORKER1|worker|oldest-release|" + workerRevision + "|Up 1 hour\n"}, true
		}
		return base(command)
	}
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := engine.restoreReleaseRoles(context.Background(), engine, engineTestPreviousReleaseID, engineTestDeployReleaseID); err != nil {
		t.Fatal(err)
	}
	for _, command := range target.Commands {
		if strings.Contains(command, " up -d ") || strings.Contains(command, "docker pull") || strings.Contains(command, "docker kill") || strings.Contains(command, "docker stop") {
			t.Fatalf("matching rollback target mutated a retained workload: %s", command)
		}
	}
}

func TestRecoveryRollsPredecessorBeforeRemovingServingContainer(t *testing.T) {
	target := happyFake()
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
		case strings.Contains(command, "docker ps -a --filter label=onebox.app="):
			return transport.Result{Stdout: "NEW1|web|demo-web-1|running"}, true
		case strings.Contains(command, "docker ps -aq") && strings.Contains(command, "label=onebox.release='"+engineTestDeployReleaseID+"'"):
			if !strings.Contains(strings.Join(target.Commands, "\n"), "docker rm -f NEW1") {
				return transport.Result{Stdout: "NEW1"}, true
			}
			return transport.Result{}, true
		}
		return base(command)
	}
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	seedInterruptedRecoveryState(t, engine)
	err := engine.recoverInterrupted(context.Background(), recoveryRequest{
		InterruptedID: engineTestDeployReleaseID, PreviousID: engineTestPreviousReleaseID,
		TerminalState: release.StateAborted, GateCovered: true, Phase: "abort", Journal: recoveryWriter(engine),
	})
	if err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(target.Commands, "\n")
	restore := strings.Index(commands, "--scale web=")
	if restore < 0 {
		t.Fatalf("no predecessor roll:\n%s", commands)
	}
	for _, command := range target.Commands {
		if strings.Contains(command, "NEW1") && (strings.Contains(command, "docker stop") || strings.Contains(command, "docker rm")) {
			if strings.Index(commands, command) < restore {
				t.Fatalf("serving container removed before restore: %s", command)
			}
		}
	}
}

func TestRecoveryWithoutPredecessorRemovesEverythingFirst(t *testing.T) {
	target := happyFake()
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "docker ps -a --filter label=onebox.app=") {
			return transport.Result{Stdout: "NEW1|web|demo-web-1|running\nSTALE1|stale|demo-stale-1|exited"}, true
		}
		if strings.Contains(command, "docker ps -aq") && strings.Contains(command, "label=onebox.release='"+engineTestDeployReleaseID+"'") {
			return transport.Result{}, true
		}
		return base(command)
	}
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	seedInterruptedRecoveryState(t, engine)
	// This is a first-release recovery, so its checkpoint has no predecessor.
	checkpoint, err := release.NewActivationCheckpoint(engineTestDeployReleaseID, "", release.ActivationPrepared, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.writeActivationCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := engine.recoverInterrupted(context.Background(), recoveryRequest{
		InterruptedID: engineTestDeployReleaseID, TerminalState: release.StateAborted,
		GateCovered: true, Phase: "abort", Journal: recoveryWriter(engine),
	}); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(target.Commands, "\n")
	for _, id := range []string{"NEW1", "STALE1"} {
		removed := strings.Index(commands, "docker rm -f "+id)
		if removed < 0 || removed > strings.Index(commands, "readlink") {
			t.Fatalf("container %s not removed before finalization:\n%s", id, commands)
		}
	}
}

func TestRecoveryNonServingSelection(t *testing.T) {
	for _, tc := range []struct {
		name, service, state        string
		transient, noPrevious, keep bool
	}{
		{name: "running rolling", service: "web", state: "running", keep: true},
		{name: "stopped rolling", service: "web", state: "exited"},
		{name: "paused rolling", service: "web", state: "paused"},
		{name: "restarting rolling", service: "web", state: "restarting"},
		{name: "recreate", service: "worker", state: "running"},
		{name: "missing workload", service: "stale", state: "running"},
		{name: "unlabelled", state: "running"},
		{name: "transient", service: "web", state: "running", transient: true},
		{name: "first release", service: "web", state: "running", noPrevious: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &transport.Fake{}
			engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			name := "sample-web-1"
			if tc.transient {
				name = engine.names().TransientContainer(tc.service)
			}
			target.Dynamic = func(command string) (transport.Result, bool) {
				if strings.Contains(command, "docker ps -a ") {
					return transport.Result{Stdout: "NEW1|" + tc.service + "|" + name + "|" + tc.state}, true
				}
				return transport.Result{}, false
			}
			previous := engine
			if tc.noPrevious {
				previous = nil
			}
			if err := engine.removeInterruptedNonServing(context.Background(), engineTestDeployReleaseID, previous); err != nil {
				t.Fatal(err)
			}
			removed := strings.Contains(strings.Join(target.Commands, "\n"), "docker rm -f NEW1")
			if removed == tc.keep {
				t.Fatalf("removed = %v, want keep = %v", removed, tc.keep)
			}
		})
	}
}

func TestRecoveryNonServingRejectsInvalidListingBeforeRemoval(t *testing.T) {
	for _, listing := range []string{"NEW1|stale|name|exited\nBAD;id|web|name|running", "NEW1|stale|name|exited\n|web|name|running", "NEW1|stale|name|exited\nbroken", "NEW1|stale|name|exited\n BAD1|web|name|running"} {
		t.Run(listing, func(t *testing.T) {
			target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if strings.Contains(command, "docker ps -a ") {
					return transport.Result{Stdout: listing}, true
				}
				return transport.Result{}, false
			}}
			engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			if err := engine.removeInterruptedNonServing(context.Background(), engineTestDeployReleaseID, engine); err == nil {
				t.Fatal("invalid listing accepted")
			}
			for _, command := range target.Commands {
				if strings.Contains(command, "docker rm") {
					t.Fatalf("mutated before validating listing: %s", command)
				}
			}
		})
	}
}

func TestRecoveryFakePreservesInterruptedOwnershipUntilRemoval(t *testing.T) {
	for _, removal := range []string{"docker rm OLD1", "if docker inspect OLD1 >/dev/null 2>&1; then docker rm -f OLD1; fi"} {
		t.Run(removal, func(t *testing.T) {
			target := happyFake()
			engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			ctx := context.Background()
			listing := "docker ps -a --filter label=onebox.app=" + q(engine.Spec.Name) +
				" --filter label=onebox.release=" + q(engineTestDeployReleaseID) +
				" --format " + q(`{{.ID}}|{{.Label "com.docker.compose.service"}}|{{.Names}}|{{.State}}`)
			if _, err := target.Run(ctx, listing); err != nil {
				t.Fatal(err)
			}
			// Simulate predecessor creation without retiring the interrupted replica.
			if _, err := target.Run(ctx, "docker compose up -d --scale web=2"); err != nil {
				t.Fatal(err)
			}
			ids, err := engine.exactReleaseContainerIDs(ctx, engineTestDeployReleaseID)
			if err != nil || strings.Join(ids, ",") != "OLD1" {
				t.Fatalf("surviving interrupted ids = %v, %v", ids, err)
			}
			if err := engine.assertNoReleaseContainers(ctx, engineTestDeployReleaseID); err == nil {
				t.Fatal("postcondition accepted surviving interrupted replica")
			}
			ids, err = engine.newcomerIDs(ctx, "web", engineTestPreviousReleaseID, "")
			if err != nil || strings.Join(ids, ",") != "NEW1" {
				t.Fatalf("predecessor ids = %v, %v", ids, err)
			}
			if _, err := target.Run(ctx, removal); err != nil {
				t.Fatal(err)
			}
			if err := engine.assertNoReleaseContainers(ctx, engineTestDeployReleaseID); err != nil {
				t.Fatal(err)
			}
			result, err := target.Run(ctx, listing)
			if err != nil || strings.TrimSpace(result.Stdout) != "" {
				t.Fatalf("removed interrupted replica reappeared: %+v, %v", result, err)
			}
			ids, err = engine.newcomerIDs(ctx, "web", engineTestPreviousReleaseID, "")
			if err != nil || strings.Join(ids, ",") != "NEW1" {
				t.Fatalf("removal changed predecessor ids = %v, %v", ids, err)
			}
		})
	}
}

func TestRestoreReleaseRolesRollsInterruptedOwnedMatchingRevision(t *testing.T) {
	target := happyFake()
	revision := "sha256:" + strings.Repeat("a", 64)
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "/compose.yaml") && strings.HasPrefix(command, "cat "):
			return transport.Result{Stdout: "services:\n  web:\n    labels:\n      onebox.workload-revision: " + revision + "\n"}, true
		case strings.Contains(command, "docker ps --filter label=onebox.app="):
			return transport.Result{Stdout: "OLD1|web|" + engineTestDeployReleaseID + "|" + revision + "|Up 1 hour (healthy)\n"}, true
		}
		return base(command)
	}
	cfg := testConfig()
	for name := range cfg.Workloads {
		if name != "web" {
			delete(cfg.Workloads, name)
		}
	}
	cfg.Deployment.Order = []string{"web"}
	engine := New(cfg, testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := engine.restoreReleaseRoles(context.Background(), engine, engineTestPreviousReleaseID, engineTestDeployReleaseID); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(target.Commands, "\n")
	last := -1
	for _, marker := range []string{"--scale web=", "docker exec OLD1 touch", "docker stop -t 30 OLD1", "docker rm OLD1"} {
		at := strings.Index(commands, marker)
		if at < 0 || at <= last {
			t.Fatalf("missing or out-of-order %q:\n%s", marker, commands)
		}
		last = at
	}
}
