package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

const generationProject = `api_version: onebox.run/v1
app: shop
base_path: /srv/onebox
environments:
  production: {server: deploy@example.invalid}
workloads:
  web:
    image: nginx
    port: 3000
    domain: shop.example.com
    env_files: [{file: web.enc.env, provider: sops}]
  worker:
    role: worker
    image: nginx
    env_files: [{file: worker.enc.env, provider: sops}]
`

const (
	oldSecretGeneration = "sg-111111111111111111111111"
	newSecretGeneration = "sg-222222222222222222222222"
)

type generationFakeState struct {
	containers    map[string]string
	generations   map[string]string
	sequence      int
	failNewWorker bool
	failOld       string
}

func newGenerationFake(t *testing.T, unchanged bool) (*transport.Fake, *generationFakeState) {
	t.Helper()
	state := &generationFakeState{
		containers:  map[string]string{"web": "W1", "worker": "K1"},
		generations: map[string]string{"W1": oldSecretGeneration, "K1": oldSecretGeneration},
		sequence:    1,
	}
	fake := &transport.Fake{HostName: "example.invalid", TargetName: "deploy@example.invalid"}
	fake.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "_host/owner"):
			return transport.Result{Stdout: "shop\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/20260809-120000-current\n"}, true
		case strings.Contains(command, "/ob.snapshot.yml"):
			return transport.Result{Stdout: generationProject}, true
		case strings.HasPrefix(strings.TrimSpace(command), "cat ") && strings.Contains(command, "/compose.yaml"):
			return transport.Result{Stdout: currentGenerationCompose(oldSecretGeneration)}, true
		case strings.Contains(command, "cmp -s"):
			if unchanged {
				return transport.Result{}, true
			}
			return transport.Result{ExitCode: 1}, true
		case strings.Contains(command, "docker compose") && strings.Contains(command, " up -d "):
			workload := generationWorkload(command)
			generation := generationFromEngineSecretCommand(command)
			if state.failNewWorker && workload == "worker" && generation == newSecretGeneration {
				state.failNewWorker = false
				return transport.Result{ExitCode: 31, Stderr: "injected replacement failure"}, true
			}
			if state.failOld == workload && generation == oldSecretGeneration {
				state.failOld = ""
				return transport.Result{ExitCode: 32, Stderr: "injected recovery failure"}, true
			}
			state.sequence++
			identifier := strings.ToUpper(workload[:1]) + fmt.Sprint(state.sequence)
			state.containers[workload] = identifier
			state.generations[identifier] = generation
			return transport.Result{}, true
		case strings.Contains(command, "docker ps -q"):
			workload := generationWorkload(command)
			return transport.Result{Stdout: state.containers[workload] + "\n"}, true
		case strings.Contains(command, "ob.secret-generation"):
			for identifier, generation := range state.generations {
				if strings.HasSuffix(command, " "+identifier) {
					return transport.Result{Stdout: generation + "\n"}, true
				}
			}
			return transport.Result{ExitCode: 1}, true
		case strings.Contains(command, "State.Status"):
			return transport.Result{Stdout: "running\n"}, true
		case strings.Contains(command, "{{.Name}}"):
			return transport.Result{Stdout: "container\n"}, true
		}
		return transport.Result{}, false
	}
	return fake, state
}

func generationWorkload(command string) string {
	if strings.Contains(command, "worker") {
		return "worker"
	}
	return "web"
}

func generationFromEngineSecretCommand(command string) string {
	const marker = "/.ob-secret-generations/"
	start := strings.Index(command, marker)
	if start < 0 {
		return ""
	}
	remainder := command[start+len(marker):]
	if end := strings.IndexByte(remainder, '/'); end >= 0 {
		return remainder[:end]
	}
	return ""
}

func requireGenerationProjectDirectory(t *testing.T, commands []string, generation string) {
	t.Helper()
	releaseDir := "/srv/onebox/shop/releases/20260809-120000-current"
	composePath := releaseDir + "/.ob-secret-generations/" + generation + "/compose.yaml"
	projectArg := "--project-directory '" + releaseDir + "'"
	found := false
	for _, command := range commands {
		if !strings.Contains(command, "docker compose") || !strings.Contains(command, " up -d ") || !strings.Contains(command, composePath) {
			continue
		}
		found = true
		if !strings.Contains(command, projectArg) {
			t.Errorf("generation %s was recreated without release project directory %q:\n%s", generation, projectArg, command)
		}
	}
	if !found {
		t.Errorf("generation %s had no Compose recreation command:\n%s", generation, strings.Join(commands, "\n"))
	}
}

func currentGenerationCompose(generation string) string {
	return fmt.Sprintf(`services:
  web:
    env_file: [.ob-secret-generations/%[1]s/.ob-decrypted-sops-web.enc.env]
    labels: {ob.app: shop, ob.release: 20260809-120000-current, ob.secret-generation: %[1]s}
  worker:
    env_file: [.ob-secret-generations/%[1]s/.ob-decrypted-sops-worker.enc.env]
    labels: {ob.app: shop, ob.release: 20260809-120000-current, ob.secret-generation: %[1]s}
`, generation)
}

func generationEngine(t *testing.T, fake *transport.Fake, output *bytes.Buffer) *Engine {
	t.Helper()
	resolved := resolvedSecretGraph(t, generationProject)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	return New(resolved, nil, fake, Options{
		Environment: "production", Out: output, Sleep: noSleep,
		Now:              func() time.Time { now = now.Add(time.Second); return now },
		SecretGeneration: func() (string, error) { return newSecretGeneration, nil },
	})
}

func generationPayloads() []SecretPayload {
	return []SecretPayload{
		{Path: ".ob-decrypted-sops-web.enc.env", Bytes: []byte("WEB=TOP_SECRET_NEW\n")},
		{Path: ".ob-decrypted-sops-worker.enc.env", Bytes: []byte("WORKER=TOP_SECRET_NEW\n")},
	}
}

func seedSecretCheckpoint(t *testing.T, fake *transport.Fake, engine *Engine, phase release.SecretPhase, replaced ...string) {
	t.Helper()
	at := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	checkpoint, err := release.NewSecretCheckpoint(
		"20260809-120000-current", oldSecretGeneration, newSecretGeneration,
		[]string{"web", "worker"},
		[]string{".ob-decrypted-sops-web.enc.env", ".ob-decrypted-sops-worker.enc.env"},
		at,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, workload := range replaced {
		at = at.Add(time.Second)
		if err := checkpoint.MarkReplaced(workload, at); err != nil {
			t.Fatal(err)
		}
	}
	if checkpoint.Phase != phase {
		checkpoint.Phase = phase
		checkpoint.UpdatedAt = at.Add(time.Second).Format(time.RFC3339Nano)
		if err := checkpoint.Validate(); err != nil {
			t.Fatal(err)
		}
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

func TestSecretGenerationChangedCommitsAllNewWithoutLeakingContent(t *testing.T) {
	fake, state := newGenerationFake(t, false)
	var output bytes.Buffer
	engine := generationEngine(t, fake, &output)
	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err != nil {
		t.Fatalf("push: %v\n%s", err, strings.Join(fake.Commands, "\n"))
	}
	if result.NoOp || result.Generation != newSecretGeneration {
		t.Fatalf("result = %#v", result)
	}
	for workload, identifier := range state.containers {
		if state.generations[identifier] != newSecretGeneration {
			t.Fatalf("%s retained generation %q", workload, state.generations[identifier])
		}
	}
	commands := strings.Join(fake.Commands, "\n")
	if strings.Count(commands, "--force-recreate") < 2 || !strings.Contains(commands, newSecretGeneration+"/compose.yaml") {
		t.Fatalf("generation-wide force replacement was not proved:\n%s", commands)
	}
	requireGenerationProjectDirectory(t, fake.Commands, newSecretGeneration)
	if !strings.Contains(commands, "clean orphaned secret generations") &&
		(!strings.Contains(commands, "-mindepth 1 -maxdepth 1 -type d") || !strings.Contains(commands, "! -name '"+oldSecretGeneration+"'")) {
		t.Fatalf("pre-checkpoint orphan generations were not swept:\n%s", commands)
	}
	allEvidence := commands + "\n" + strings.Join(fake.Inputs, "\n") + "\n" + output.String()
	for _, forbidden := range []string{"TOP_SECRET_NEW", "sha256:", HashBytesHex([]byte("WEB=TOP_SECRET_NEW\n"))} {
		if strings.Contains(allEvidence, forbidden) {
			t.Fatalf("secret evidence leaked %q", forbidden)
		}
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); !errors.Is(err, release.ErrSecretCheckpointMissing) {
		t.Fatalf("successful all-new state retained checkpoint: %v", err)
	}
}

func TestSecretGenerationUnchangedIsNoOpWithoutReplacement(t *testing.T) {
	fake, _ := newGenerationFake(t, true)
	engine := generationEngine(t, fake, &bytes.Buffer{})
	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err != nil {
		t.Fatal(err)
	}
	if !result.NoOp {
		t.Fatalf("unchanged push = %#v, want no-op", result)
	}
	commands := strings.Join(fake.Commands, "\n")
	if strings.Contains(commands, "--force-recreate") || strings.Contains(commands, "/secret-activation.json.tmp") || strings.Contains(commands, `"phase":"secrets-push"`) {
		t.Fatalf("unchanged push replaced a workload or opened a transaction:\n%s", commands)
	}
}

func TestSecretGenerationUploadCleanupFailureCannotReportSuccess(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	base := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "rm -rf") && strings.Contains(command, ".secret-upload-"+newSecretGeneration) {
			return transport.Result{ExitCode: 73, Stderr: "injected upload cleanup failure"}, true
		}
		return base(command)
	}
	engine := generationEngine(t, fake, &bytes.Buffer{})
	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err == nil || !strings.Contains(err.Error(), "clean secret upload") || result.Generation != newSecretGeneration {
		t.Fatalf("cleanup failure result = %#v, %v", result, err)
	}
	if !strings.Contains(strings.Join(fake.Commands, "\n"), `"phase":"secrets-push","event":"finish","status":"fail"`) {
		t.Fatal("cleanup failure was not journaled as a failed operation")
	}
}

func TestSecretGenerationJournalFailurePrecedesCandidateOrLiveMutation(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	base := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "/journal/") && strings.Contains(command, "secrets-push") {
			return transport.Result{ExitCode: 73, Stderr: "injected journal failure"}, true
		}
		return base(command)
	}
	engine := generationEngine(t, fake, &bytes.Buffer{})
	_, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err == nil || !strings.Contains(err.Error(), "journal secrets push start") {
		t.Fatalf("error = %v, want journal start failure", err)
	}
	commands := strings.Join(fake.Commands, "\n")
	for _, forbidden := range []string{"cp -R ", "/secret-activation.json.tmp", "--force-recreate"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("journal failure crossed mutation boundary %q:\n%s", forbidden, commands)
		}
	}
}

func TestSecretGenerationCheckpointFailureRemovesInstalledCandidate(t *testing.T) {
	fake, _ := newGenerationFake(t, false)
	base := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "/secret-activation.json.tmp") {
			return transport.Result{ExitCode: 74, Stderr: "injected checkpoint failure"}, true
		}
		return base(command)
	}
	engine := generationEngine(t, fake, &bytes.Buffer{})
	_, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err == nil || !strings.Contains(err.Error(), "write secret checkpoint failed") {
		t.Fatalf("checkpoint failure = %v", err)
	}
	commands := strings.Join(fake.Commands, "\n")
	installed := strings.Index(commands, "cp -R")
	removed := strings.LastIndex(commands, "rm -rf '/srv/onebox/shop/releases/20260809-120000-current/.ob-secret-generations/"+newSecretGeneration+"'")
	if installed < 0 || removed <= installed {
		t.Fatalf("installed plaintext candidate survived checkpoint failure:\n%s", commands)
	}
}

func TestSecretGenerationPartialFailureRecoversEveryWorkloadToOld(t *testing.T) {
	fake, state := newGenerationFake(t, false)
	state.failNewWorker = true
	engine := generationEngine(t, fake, &bytes.Buffer{})
	_, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err == nil || !strings.Contains(err.Error(), "injected replacement failure") {
		t.Fatalf("got %v, want injected transition failure", err)
	}
	var incomplete *SecretRecoveryIncompleteError
	if errors.As(err, &incomplete) {
		t.Fatalf("recovery completed but returned incomplete: %v", err)
	}
	for workload, identifier := range state.containers {
		if state.generations[identifier] != oldSecretGeneration {
			t.Fatalf("%s ended on %q after recovery", workload, state.generations[identifier])
		}
	}
	requireGenerationProjectDirectory(t, fake.Commands, newSecretGeneration)
	requireGenerationProjectDirectory(t, fake.Commands, oldSecretGeneration)
	if _, checkpointErr := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); !errors.Is(checkpointErr, release.ErrSecretCheckpointMissing) {
		t.Fatalf("all-old recovery retained checkpoint: %v", checkpointErr)
	}
}

func TestSecretGenerationCheckpointResumesAfterEveryForwardCrashBoundary(t *testing.T) {
	for _, test := range []struct {
		phase    release.SecretPhase
		replaced []string
	}{
		{phase: release.SecretPrepared},
		{phase: release.SecretReplacing, replaced: []string{"web"}},
		{phase: release.SecretVerifying, replaced: []string{"web", "worker"}},
		{phase: release.SecretCommitting, replaced: []string{"web", "worker"}},
	} {
		t.Run(string(test.phase), func(t *testing.T) {
			fake, state := newGenerationFake(t, false)
			engine := generationEngine(t, fake, &bytes.Buffer{})
			for _, workload := range test.replaced {
				state.generations[state.containers[workload]] = newSecretGeneration
			}
			seedSecretCheckpoint(t, fake, engine, test.phase, test.replaced...)

			result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if result.Generation != newSecretGeneration {
				t.Fatalf("resume generated a different target: %#v", result)
			}
			for workload, identifier := range state.containers {
				if state.generations[identifier] != newSecretGeneration {
					t.Fatalf("%s did not resume to new generation", workload)
				}
			}
			if !strings.Contains(strings.Join(fake.Commands, "\n"), "resume old_generation=") {
				t.Fatal("resumed transaction was not journaled")
			}
		})
	}
}

func TestSecretGenerationCommittedCheckpointFinalizesWithoutReplacement(t *testing.T) {
	fake, state := newGenerationFake(t, false)
	engine := generationEngine(t, fake, &bytes.Buffer{})
	for _, identifier := range state.containers {
		state.generations[identifier] = newSecretGeneration
	}
	seedSecretCheckpoint(t, fake, engine, release.SecretCommitted, "web", "worker")
	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err != nil || result.Generation != newSecretGeneration {
		t.Fatalf("finalize = %#v, %v", result, err)
	}
	if strings.Contains(strings.Join(fake.Commands, "\n"), "--force-recreate") {
		t.Fatalf("committed checkpoint replaced a workload:\n%s", strings.Join(fake.Commands, "\n"))
	}
}

func TestSecretGenerationIncompleteRecoveryStaysRetryable(t *testing.T) {
	fake, state := newGenerationFake(t, false)
	state.failNewWorker = true
	state.failOld = "web"
	engine := generationEngine(t, fake, &bytes.Buffer{})
	_, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	var incomplete *SecretRecoveryIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("error = %v, want secret_recovery_incomplete", err)
	}
	checkpoint, checkpointErr := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names())
	if checkpointErr != nil || checkpoint.Phase != release.SecretRecovering {
		t.Fatalf("checkpoint = %#v, %v; want recovering", checkpoint, checkpointErr)
	}

	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	var rolledBack *SecretRotationRolledBackError
	if !errors.As(err, &rolledBack) || result.Generation != oldSecretGeneration {
		t.Fatalf("recovery retry = %#v, %v; want explicit safe-rollback retry error", result, err)
	}
	for workload, identifier := range state.containers {
		if state.generations[identifier] != oldSecretGeneration {
			t.Fatalf("%s ended on %q after recovery retry", workload, state.generations[identifier])
		}
	}
	if _, checkpointErr := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); !errors.Is(checkpointErr, release.ErrSecretCheckpointMissing) {
		t.Fatalf("completed recovery retained checkpoint: %v", checkpointErr)
	}

	result, err = engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err != nil || result.Generation != newSecretGeneration {
		t.Fatalf("fresh retry after recovery = %#v, %v", result, err)
	}
}

// A cleanup failure after a verified commit must not be mistaken for a failed
// rotation. Recovering there would roll live workloads back off secrets that
// are already applied.
func TestSecretGenerationPostCommitSweepFailureKeepsTheNewGeneration(t *testing.T) {
	fake, state := newGenerationFake(t, false)
	base := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "rm -rf") && strings.Contains(command, "/.ob-secret-generations/"+oldSecretGeneration) {
			return transport.Result{ExitCode: 73, Stderr: "injected retired generation sweep failure"}, true
		}
		return base(command)
	}
	engine := generationEngine(t, fake, &bytes.Buffer{})

	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	var pending *SecretCleanupPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("error = %v, want secret_cleanup_pending", err)
	}
	if result.Generation != newSecretGeneration {
		t.Fatalf("result generation = %q, want the applied new generation", result.Generation)
	}
	for workload, identifier := range state.containers {
		if state.generations[identifier] != newSecretGeneration {
			t.Fatalf("%s was rolled back to %q despite a verified commit", workload, state.generations[identifier])
		}
	}
	// The checkpoint is cleared before the sweep, so a sweep failure leaves no
	// transaction to resume — only an orphaned directory, which the next push
	// sweeps on entry.
	if _, checkpointErr := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); checkpointErr == nil {
		t.Fatal("a sweep failure retained the checkpoint; recovery could later target a swept generation")
	}
	if !strings.Contains(strings.Join(fake.Commands, "\n"), `"phase":"secrets-push","event":"finish","status":"ok"`) {
		t.Fatal("a committed rotation was journaled as a failed transition")
	}

	fake.Dynamic = base
	before := len(fake.Commands)
	result, err = engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err != nil || result.Generation != newSecretGeneration {
		t.Fatalf("cleanup retry = %#v, %v", result, err)
	}
	// The checkpoint was already cleared, so this retry is a fresh rotation
	// rather than a resumed one. What matters is that the orphan left by the
	// failed sweep is collected on entry.
	retry := strings.Join(fake.Commands[before:], "\n")
	if !strings.Contains(retry, oldSecretGeneration) {
		t.Fatalf("retry did not sweep the orphaned generation:\n%s", retry)
	}
}

// The other half of the same guard: when clearing the checkpoint fails the
// rotation is still applied, and the retained checkpoint must resume cleanup
// rather than roll a live, verified generation back.
func TestSecretGenerationCheckpointClearFailureResumesCleanup(t *testing.T) {
	fake, state := newGenerationFake(t, false)
	base := fake.Dynamic
	fail := true
	fake.Dynamic = func(command string) (transport.Result, bool) {
		// Match only the clear, not the temp-file cleanup inside the write.
		if fail && strings.Contains(command, "rm -f") && strings.Contains(command, "secret-activation.json'") && !strings.Contains(command, ".tmp") {
			return transport.Result{ExitCode: 73, Stderr: "injected checkpoint clear failure"}, true
		}
		return base(command)
	}
	engine := generationEngine(t, fake, &bytes.Buffer{})

	result, err := engine.SecretsPushBatch(context.Background(), generationPayloads())
	var pending *SecretCleanupPendingError
	if !errors.As(err, &pending) || result.Generation != newSecretGeneration {
		t.Fatalf("clear failure = %#v, %v; want secret_cleanup_pending on the new generation", result, err)
	}
	for workload, identifier := range state.containers {
		if state.generations[identifier] != newSecretGeneration {
			t.Fatalf("%s was rolled back to %q despite a verified commit", workload, state.generations[identifier])
		}
	}
	checkpoint, checkpointErr := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names())
	if checkpointErr != nil || checkpoint.Phase != release.SecretCommitted {
		t.Fatalf("checkpoint = %#v, %v; want a retained committed checkpoint", checkpoint, checkpointErr)
	}

	fail = false
	before := len(fake.Commands)
	result, err = engine.SecretsPushBatch(context.Background(), generationPayloads())
	if err != nil || result.Generation != newSecretGeneration {
		t.Fatalf("cleanup retry = %#v, %v", result, err)
	}
	if retry := strings.Join(fake.Commands[before:], "\n"); strings.Contains(retry, "--force-recreate") {
		t.Fatalf("cleanup retry replaced a workload:\n%s", retry)
	}
	if _, err := release.ReadSecretCheckpoint(context.Background(), fake, engine.Names()); err == nil {
		t.Fatal("cleanup retry left the checkpoint in place")
	}
}
