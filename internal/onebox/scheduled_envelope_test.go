package onebox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

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
}

func (executor *recordingScheduledExecutor) ExecuteScheduledLifecycle(_ context.Context, execution ScheduledLifecycleExecution) error {
	executor.executions = append(executor.executions, execution)
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
}
