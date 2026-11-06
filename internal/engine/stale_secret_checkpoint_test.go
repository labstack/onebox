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

const supersededSecretReleaseID = "20260801-120000-older"

// seedSupersededSecretCheckpoint writes a recovering checkpoint for a release
// that is no longer current, the state an interrupted push leaves once a later
// deploy replaces its release (issue #233).
func seedSupersededSecretCheckpoint(t *testing.T, fake *transport.Fake, engine *Engine) {
	t.Helper()
	checkpoint, err := release.NewSecretCheckpoint(
		supersededSecretReleaseID, oldSecretGeneration, newSecretGeneration,
		[]string{"server"},
		[]string{".onebox-decrypted-sops-secrets-production.env"},
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetPhase(release.SecretRecovering, time.Date(2026, 8, 1, 12, 0, 1, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	command, input, err := release.SecretCheckpointWrite(engine.Names(), checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := fake.RunInput(context.Background(), command, input); err != nil || result.ExitCode != 0 {
		t.Fatalf("seed checkpoint: result=%#v err=%v", result, err)
	}
	fake.Commands = nil
	fake.Inputs = nil
}

func TestSecretsPushDiscardsCheckpointFromSupersededRelease(t *testing.T) {
	fake, state := newGenerationFake(t, false)
	var out bytes.Buffer
	engine := generationEngine(t, fake, &out)
	seedSupersededSecretCheckpoint(t, fake, engine)

	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err != nil {
		t.Fatalf("a checkpoint from a superseded release must not block a push: %v", err)
	}
	if result.Generation != newSecretGeneration {
		t.Fatalf("push = %#v, want the fresh path's new generation", result)
	}
	for workload, identifier := range state.containers {
		if state.generations[identifier] != newSecretGeneration {
			t.Fatalf("%s ended on %q", workload, state.generations[identifier])
		}
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); !errors.Is(err, release.ErrSecretCheckpointMissing) {
		t.Fatalf("checkpoint after push: %v, want cleared", err)
	}
	if !strings.Contains(out.String(), supersededSecretReleaseID) {
		t.Fatalf("discarding the checkpoint was not reported:\n%s", out.String())
	}
}

func TestSecretsPushMismatchedCheckpointNamesTheFileAndRemedy(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	engine := generationEngine(t, fake, &bytes.Buffer{})
	checkpoint, err := release.NewSecretCheckpoint(
		"20260809-120000-current", oldSecretGeneration, newSecretGeneration,
		[]string{"web"},
		[]string{".onebox-decrypted-sops-web.enc.env"},
		time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	command, input, err := release.SecretCheckpointWrite(engine.Names(), checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := fake.RunInput(context.Background(), command, input); err != nil || result.ExitCode != 0 {
		t.Fatalf("seed checkpoint: result=%#v err=%v", result, err)
	}

	_, err = engine.SecretsPushBatch(context.Background(), generationPayloads())
	var mismatch *SecretCheckpointMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want secret_checkpoint_mismatch", err)
	}
	message := err.Error()
	for _, want := range []string{release.SecretCheckpointPath(engine.Names()), "20260809-120000-current", checkpoint.UpdatedAt} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not name %q", message, want)
		}
	}
	if strings.Contains(message, "retry `ob secrets push`") {
		t.Fatalf("error %q advises a retry that cannot succeed", message)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); err != nil {
		t.Fatalf("a checkpoint for the current release must be kept: %v", err)
	}
}

func TestDeployDiscardsSecretCheckpointFromReplacedRelease(t *testing.T) {
	f := happyFake()
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	seedSupersededSecretCheckpoint(t, f, e)

	if err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), f, e.Names()); !errors.Is(err, release.ErrSecretCheckpointMissing) {
		t.Fatalf("checkpoint after deploy: %v, want cleared", err)
	}
	if !strings.Contains(out.String(), supersededSecretReleaseID) {
		t.Fatalf("discarding the checkpoint was not reported:\n%s", out.String())
	}
}

// seedLegacySecretCheckpoint writes a checkpoint in the v1alpha1 schema that
// releases before the onebox naming change wrote (issue #209).
func seedLegacySecretCheckpoint(t *testing.T, fake *transport.Fake, engine *Engine, releaseID string) {
	t.Helper()
	valid, err := release.NewSecretCheckpoint(supersededSecretReleaseID, oldSecretGeneration, newSecretGeneration,
		[]string{"web"}, []string{".onebox-decrypted-sops-web.enc.env"}, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	command, _, err := release.SecretCheckpointWrite(engine.Names(), valid)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"schema_version":"onebox.run/secret-checkpoint/v1alpha1","release_id":"` + releaseID +
		`","old_generation":"` + oldSecretGeneration + `","new_generation":"` + newSecretGeneration +
		`","phase":"replacing","affected_workloads":["web"],"payload_paths":["legacy.env"],"updated_at":"2026-08-01T12:00:00Z"}`
	if result, err := fake.RunInput(context.Background(), command, legacy); err != nil || result.ExitCode != 0 {
		t.Fatalf("seed legacy checkpoint: result=%#v err=%v", result, err)
	}
	fake.Commands = nil
	fake.Inputs = nil
}

func TestSecretsPushDiscardsLegacyCheckpointFromSupersededRelease(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	var out bytes.Buffer
	engine := generationEngine(t, fake, &out)
	seedLegacySecretCheckpoint(t, fake, engine, supersededSecretReleaseID)

	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err != nil || result.Generation != newSecretGeneration {
		t.Fatalf("push = %#v, %v; a legacy checkpoint from a superseded release must not block it", result, err)
	}
	if !strings.Contains(out.String(), "onebox.run/secret-checkpoint/v1alpha1") {
		t.Fatalf("discarding the legacy checkpoint was not reported:\n%s", out.String())
	}
}

func TestSecretsPushRefusesLegacyCheckpointForCurrentRelease(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	engine := generationEngine(t, fake, &bytes.Buffer{})
	seedLegacySecretCheckpoint(t, fake, engine, "20260809-120000-current")

	_, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	var mismatch *SecretCheckpointMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want secret_checkpoint_mismatch", err)
	}
	for _, want := range []string{release.SecretCheckpointPath(engine.Names()), "v1alpha1", "2026-08-01T12:00:00Z"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err.Error(), want)
		}
	}
	commands := strings.Join(fake.Commands, "\n")
	if strings.Contains(commands, "--force-recreate") || strings.Contains(commands, "cp -R ") {
		t.Fatalf("refusal crossed the mutation boundary:\n%s", commands)
	}
}

func TestDeployDiscardsLegacySecretCheckpoint(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	seedLegacySecretCheckpoint(t, f, e, supersededSecretReleaseID)

	if err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), f, e.Names()); !errors.Is(err, release.ErrSecretCheckpointMissing) {
		t.Fatalf("legacy checkpoint after deploy: %v, want cleared", err)
	}
}

// A deploy keeps workloads whose inputs did not change, so it must not start
// over an unfinished push for the current release: a kept workload could stay
// half-rotated with the only record of the rotation gone.
func TestDeployPreconditionRefusesUnfinishedPushForCurrentRelease(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	engine := generationEngine(t, fake, &bytes.Buffer{})
	seedSecretCheckpoint(t, fake, engine, release.SecretReplacing, "web")

	err := engine.requireNoUnfinishedSecretPush(context.Background(), "20260809-120000-current")
	var unfinished *SecretPushUnfinishedError
	if !errors.As(err, &unfinished) || unfinished.ReleaseID != "20260809-120000-current" {
		t.Fatalf("error = %v, want secret_push_unfinished for the current release", err)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); err != nil {
		t.Fatalf("refusal must keep the checkpoint: %v", err)
	}
}

func TestDeployPreconditionDiscardsSupersededCheckpoint(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	engine := generationEngine(t, fake, &bytes.Buffer{})
	seedSupersededSecretCheckpoint(t, fake, engine)

	if err := engine.requireNoUnfinishedSecretPush(context.Background(), "20260809-120000-current"); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); !errors.Is(err, release.ErrSecretCheckpointMissing) {
		t.Fatalf("checkpoint: %v, want cleared", err)
	}
}

// A file in another schema that names no valid release id cannot be proven
// superseded, so it is refused rather than deleted.
func TestUnsupportedCheckpointWithoutReleaseIsRefused(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	engine := generationEngine(t, fake, &bytes.Buffer{})
	seedLegacySecretCheckpoint(t, fake, engine, "not-a-release")

	_, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	var mismatch *SecretCheckpointMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want secret_checkpoint_mismatch", err)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); errors.Is(err, release.ErrSecretCheckpointMissing) {
		t.Fatal("a checkpoint that names no release was deleted")
	}
}

// current is read before the lock. A deploy that lands in between must stop
// the push before it discards or rotates anything for the new release.
func TestSecretsPushStopsWhenCurrentChangesWhileLocking(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	engine := generationEngine(t, fake, &bytes.Buffer{})
	seedSupersededSecretCheckpoint(t, fake, engine)
	base := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "readlink") && strings.Contains(strings.Join(fake.Commands, "\n"), "/fence") {
			return transport.Result{Stdout: "releases/20260810-120000-newer\n"}, true
		}
		return base(command)
	}

	_, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err == nil || !strings.Contains(err.Error(), "changed from 20260809-120000-current to 20260810-120000-newer") {
		t.Fatalf("error = %v, want a current-release change refusal", err)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); err != nil {
		t.Fatalf("push acting on a stale current release discarded the checkpoint: %v", err)
	}
}

func TestRollbackDiscardsCheckpointFromRolledBackRelease(t *testing.T) {
	f := happyFake()
	seedRollbackState(t, f)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/20260102-000000-bbb222\n"}, true
		}
		if strings.Contains(cmd, "ls -1") {
			return transport.Result{Stdout: "20260101-000000-aaa111\n20260102-000000-bbb222\n"}, true
		}
		return base(cmd)
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	seedLegacySecretCheckpoint(t, f, e, "20260102-000000-bbb222")

	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), f, e.Names()); !errors.Is(err, release.ErrSecretCheckpointMissing) {
		t.Fatalf("checkpoint after rollback: %v, want cleared", err)
	}
}

// A checkpoint file whose mode or content is unusable must not tell the
// operator to retry: neither a retry nor a deploy can fix it.
func TestUnusableCheckpointNamesTheManualRemedy(t *testing.T) {
	for _, body := range []string{"mode=644\n{}", "mode=600\n{\"schema_version\":"} {
		fake, _ := newGenerationFake(t, false)
		base := fake.Dynamic
		fake.Dynamic = func(command string) (transport.Result, bool) {
			if strings.Contains(command, "/secret-activation.json") && strings.Contains(command, "mode=%s") {
				return transport.Result{Stdout: body}, true
			}
			return base(command)
		}
		engine := generationEngine(t, fake, &bytes.Buffer{})

		_, pushErr := engine.SecretsPushBatch(context.Background(), generationPayloads())
		deployErr := engine.requireNoUnfinishedSecretPush(context.Background(), "20260809-120000-current")
		for name, err := range map[string]error{"push": pushErr, "deploy": deployErr} {
			var mismatch *SecretCheckpointMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("%q %s: error = %v, want secret_checkpoint_mismatch", body, name, err)
			}
			if !strings.Contains(err.Error(), "back the file up, remove it") || strings.Contains(err.Error(), "retry `ob secrets push`") {
				t.Fatalf("%q %s: error %q lacks the manual remedy", body, name, err)
			}
		}
		if strings.Contains(strings.Join(fake.Commands, "\n"), "rm -f '"+release.SecretCheckpointPath(engine.Names())+"'") {
			t.Fatalf("%q: an unusable checkpoint was deleted", body)
		}
	}
}
