package onebox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecurringScheduleMaterializesFreshOccurrenceEnvelope(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	template, _ := scheduledEnvelopeFixture(t)
	first, err := MaterializeScheduledOccurrence(template, now.Add(24*time.Hour), bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := MaterializeScheduledOccurrence(template, now.Add(48*time.Hour), bytes.NewReader(bytes.Repeat([]byte{2}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if first.OperationID == template.OperationID || second.OperationID == template.OperationID || first.OperationID == second.OperationID {
		t.Fatalf("occurrence identities were reused: template=%q first=%q second=%q", template.OperationID, first.OperationID, second.OperationID)
	}
	if first.Timing.RetryIdentity == second.Timing.RetryIdentity || first.EnvelopeDigest == second.EnvelopeDigest {
		t.Fatal("separate timer firings reused retry identity or sealed envelope")
	}
	if err := first.ValidateForRunner(CurrentScheduledRunnerCompatibility(), first.State.Digest, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("first occurrence was not fresh: %v", err)
	}
	if err := second.ValidateForRunner(CurrentScheduledRunnerCompatibility(), second.State.Digest, now.Add(48*time.Hour)); err != nil {
		t.Fatalf("second occurrence was not fresh: %v", err)
	}
}

func scheduledEnvelopeFixture(t *testing.T) (ScheduledOperationEnvelope, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	envelope, err := NewScheduledOperationEnvelope(ScheduledEnvelopeInput{
		CLIProtocol:     CurrentScheduledCLIProtocol,
		RunnerProtocols: ProtocolRange{Minimum: CurrentScheduledRunnerProtocol, Maximum: CurrentScheduledRunnerProtocol},
		OperationID:     "backup-20260807", Application: "example", Environment: "production", Service: "database",
		Operation: KindBackupCreate,
		Runner: ScheduledRunnerArtifactReference{
			Path:   "/var/lib/onebox/example/protection/runners/runner-a/ob-scheduled-runner",
			Digest: "sha256:" + strings.Repeat("e", 64), SBOMDigest: "sha256:" + strings.Repeat("f", 64),
			ProvenanceID: "onebox-runner-v1",
		},
		Timing: ScheduledTimingPolicy{
			ScheduledFor: now.Format(time.RFC3339Nano), NotBefore: now.Add(-time.Minute).Format(time.RFC3339Nano),
			ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano), MaxRuntime: "10m", RetryIdentity: "backup-window-20260807",
		},
		Artifacts: []OperationArtifactBinding{
			{Class: "inputs", Path: "/var/lib/onebox/example/protection/inputs.json", Mode: 0o600, Digest: "sha256:" + strings.Repeat("b", 64)},
			{Class: "backup-schedule", Path: "/var/lib/onebox/example/protection/backup.json", Mode: 0o644, Digest: "sha256:" + strings.Repeat("a", 64)},
		},
		State: ScheduledStateBinding{Path: "/var/lib/onebox/example/protection/state.json", Digest: "sha256:" + strings.Repeat("c", 64), Epoch: 7},
		SecretFiles: []SecretSlotReference{
			{Slot: "repository", File: "/var/lib/onebox/example/protection/repository.env", Entry: "RESTIC_PASSWORD"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return envelope, now
}

func TestScheduledEnvelopeIsSealedSortedAndSecretReferential(t *testing.T) {
	envelope, now := scheduledEnvelopeFixture(t)
	if envelope.Artifacts[0].Class != "backup-schedule" {
		t.Fatalf("artifacts are not sorted: %#v", envelope.Artifacts)
	}
	if ScheduledEnvelopeContainsSecretValue(envelope, "secret-value-canary", "database-content-canary") {
		t.Fatal("scheduled envelope contains secret or database content")
	}
	encoded, err := EncodeScheduledOperationEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeScheduledOperationEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.EnvelopeDigest != envelope.EnvelopeDigest {
		t.Fatal("scheduled envelope digest did not round-trip")
	}
	if err := decoded.ValidateForRunner(CurrentScheduledRunnerCompatibility(), envelope.State.Digest, now); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledEnvelopeRejectsTamperingAndUnknownFields(t *testing.T) {
	envelope, _ := scheduledEnvelopeFixture(t)
	tampered := envelope
	tampered.Service = "other"
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tamper error = %v", err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	encoded = []byte(strings.Replace(string(encoded), "{", `{"unknown":true,`, 1))
	if _, err := DecodeScheduledOperationEnvelope(encoded); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestScheduledEnvelopeRejectsOlderCLINewerRunnerAndNewerCLIOlderRunner(t *testing.T) {
	envelope, now := scheduledEnvelopeFixture(t)
	newerRunner := ScheduledRunnerCompatibility{
		RunnerProtocol: 2, CLIProtocols: ProtocolRange{Minimum: 1, Maximum: 2}, EnvelopeProtocols: ProtocolRange{Minimum: 1, Maximum: 2},
	}
	if err := envelope.ValidateForRunner(newerRunner, envelope.State.Digest, now); err == nil || !strings.Contains(err.Error(), "scheduled_runner_incompatible") {
		t.Fatalf("older CLI/newer runner error = %v", err)
	}
	newerCLI := envelope
	newerCLI.CLIProtocol = 2
	if err := newerCLI.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := newerCLI.ValidateForRunner(CurrentScheduledRunnerCompatibility(), newerCLI.State.Digest, now); err == nil || !strings.Contains(err.Error(), "scheduled_runner_incompatible") {
		t.Fatalf("newer CLI/older runner error = %v", err)
	}
	cli := CurrentScheduledCLICompatibility()
	if err := ValidateScheduledRunnerForCLI(envelope, newerRunner, cli); err == nil || !strings.Contains(err.Error(), "scheduled_runner_incompatible") {
		t.Fatalf("CLI-side newer runner error = %v", err)
	}
}

func TestScheduledEnvelopeRejectsStaleStateAndTiming(t *testing.T) {
	envelope, now := scheduledEnvelopeFixture(t)
	if err := envelope.ValidateForRunner(CurrentScheduledRunnerCompatibility(), "sha256:"+strings.Repeat("d", 64), now); err == nil || !strings.Contains(err.Error(), "scheduled_envelope_stale") {
		t.Fatalf("stale state error = %v", err)
	}
	if err := envelope.ValidateForRunner(CurrentScheduledRunnerCompatibility(), envelope.State.Digest, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "scheduled_envelope_stale") {
		t.Fatalf("stale timing error = %v", err)
	}
}

func TestScheduledEnvelopeRejectsNonScheduledOperation(t *testing.T) {
	envelope, _ := scheduledEnvelopeFixture(t)
	envelope.Operation = KindDeploy
	if err := envelope.Seal(); err == nil || !strings.Contains(err.Error(), "unsupported lifecycle operation") {
		t.Fatalf("deploy envelope error = %v", err)
	}
}

type recordingScheduledExecutor struct {
	executions []ScheduledLifecycleExecution
	err        error
	deadline   time.Time
}

func (executor *recordingScheduledExecutor) ExecuteScheduledLifecycle(ctx context.Context, execution ScheduledLifecycleExecution) error {
	executor.executions = append(executor.executions, execution)
	executor.deadline, _ = ctx.Deadline()
	return executor.err
}

func TestScheduledRunnerUsesCanonicalScheduledGraphOnce(t *testing.T) {
	envelope, now := scheduledEnvelopeFixture(t)
	executor := &recordingScheduledExecutor{}
	runner := ScheduledRunner{
		Compatibility: CurrentScheduledRunnerCompatibility(), Now: func() time.Time { return now },
		ObserveState: func(path string) (string, error) {
			if path != envelope.State.Path {
				return "", errors.New("wrong state path")
			}
			return envelope.State.Digest, nil
		},
		Executor: executor,
	}
	if err := runner.Execute(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if len(executor.executions) != 1 || len(executor.executions[0].Steps) == 0 || executor.executions[0].Steps[0].Kind != StepProtectionLock {
		t.Fatalf("scheduled executions = %#v", executor.executions)
	}
	if want := now.Add(10 * time.Minute); !executor.deadline.Equal(want) {
		t.Fatalf("scheduled deadline = %s, want %s", executor.deadline, want)
	}
}

func TestReadScheduledStateDigestValidatesWholeState(t *testing.T) {
	state, err := NewProtectionLifecycleState("example", "production", "database", 1)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveProtectionLifecycleState(path, state); err != nil {
		t.Fatal(err)
	}
	if digest, err := ReadScheduledStateDigest(path); err != nil || digest != state.StateDigest {
		t.Fatalf("read state digest = %q, %v", digest, err)
	}

	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tampered map[string]any
	if err := json.Unmarshal(encoded, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["service"] = "other"
	encoded, err = json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadScheduledStateDigest(path); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered state error = %v", err)
	}
}
