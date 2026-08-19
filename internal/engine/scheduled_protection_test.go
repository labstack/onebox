package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/transport"
)

func scheduledProtectionTestEngine(t *testing.T, target transport.Transport) *Engine {
	t.Helper()
	return New(
		&app.Resolved{Spec: &app.Spec{Name: "example", BasePath: t.TempDir()}, Env: "production"},
		nil,
		target,
		Options{
			Out: io.Discard, LockTTL: time.Minute, Sleep: func(time.Duration) {},
			Now: func() time.Time { return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC) },
		},
	)
}

func scheduledProtectionRequest(operationID string) ScheduledProtectionRequest {
	digest := "sha256:" + strings.Repeat("a", 64)
	return ScheduledProtectionRequest{
		OperationID: operationID, OperationKind: "backup_create", Service: "database", RetryIdentity: "daily-20260807",
		HelperProvenance: &journal.HelperProvenance{
			Repository: "example/backup-helper", Digest: digest, SBOMDigest: digest, ProvenanceID: "onebox/catalog/test-helper/v1",
		},
	}
}

func readScheduledProtectionJournal(t *testing.T, engine *Engine, operationID string) []journal.Record {
	t.Helper()
	records, err := journal.Read(context.Background(), engine.T, engine.names(), operationID)
	if err != nil {
		t.Fatalf("read scheduled protection journal: %v", err)
	}
	return records
}

func TestExecuteScheduledProtectionUsesCanonicalLocksFencesAndJournal(t *testing.T) {
	engine := scheduledProtectionTestEngine(t, transport.NewLocal())
	request := scheduledProtectionRequest("scheduled-backup-1")
	actionCalls := 0

	err := engine.ExecuteScheduledProtection(context.Background(), request, func(ctx context.Context, executing *Engine) (string, error) {
		actionCalls++
		if executing.lockVal == "" || executing.fenceVal == "" || executing.protectionLockVals[request.Service] == "" || executing.protectionFenceVals[request.Service] == "" {
			t.Fatal("scheduled action ran outside the canonical lock and fence boundary")
		}
		if result, err := executing.ProtectionMutate(ctx, request.Service, "true"); err != nil || result.ExitCode != 0 {
			t.Fatalf("fenced scheduled mutation = %#v, %v", result, err)
		}
		return "backup-generation-1", nil
	})
	if err != nil {
		t.Fatalf("execute scheduled backup: %v", err)
	}
	if actionCalls != 1 {
		t.Fatalf("action calls = %d, want 1", actionCalls)
	}
	records := readScheduledProtectionJournal(t, engine, request.OperationID)
	if len(records) != 2 {
		t.Fatalf("journal records = %d, want start and result", len(records))
	}
	result := records[1]
	if result.TerminalResult == nil || result.TerminalResult.State != "succeeded" || result.TerminalResult.EvidenceID != "backup-generation-1" {
		t.Fatalf("terminal result = %#v", result.TerminalResult)
	}
	if result.HelperProvenance == nil || result.HelperProvenance.Digest != request.HelperProvenance.Digest {
		t.Fatalf("helper provenance = %#v", result.HelperProvenance)
	}
	if engine.lockVal != "" || len(engine.protectionLockVals) != 0 {
		t.Fatal("scheduled execution retained lock ownership after completion")
	}
}

func TestExecuteScheduledProtectionRetriesAfterCrashWithSameIdentity(t *testing.T) {
	engine := scheduledProtectionTestEngine(t, transport.NewLocal())
	request := scheduledProtectionRequest("scheduled-backup-crash")

	if err := engine.ExecuteScheduledProtection(context.Background(), request, func(context.Context, *Engine) (string, error) {
		return "", ErrScheduledRunnerCrash
	}); !errors.Is(err, ErrScheduledRunnerCrash) {
		t.Fatalf("crash error = %v", err)
	}
	if records := readScheduledProtectionJournal(t, engine, request.OperationID); len(records) != 1 || records[0].ProtectionAttempt != 1 || records[0].TerminalResult != nil {
		t.Fatalf("crash journal = %#v", records)
	}

	if err := engine.ExecuteScheduledProtection(context.Background(), request, func(context.Context, *Engine) (string, error) {
		return "backup-generation-after-retry", nil
	}); err != nil {
		t.Fatalf("retry after crash: %v", err)
	}
	records := readScheduledProtectionJournal(t, engine, request.OperationID)
	if len(records) != 3 || records[1].ProtectionAttempt != 2 || records[2].ProtectionAttempt != 2 || records[2].TerminalResult == nil || records[2].TerminalResult.State != "succeeded" {
		t.Fatalf("retry journal = %#v", records)
	}
}

func TestExecuteScheduledProtectionPersistsCancellationAndDoesNotRerunTerminalIdentity(t *testing.T) {
	engine := scheduledProtectionTestEngine(t, transport.NewLocal())
	request := scheduledProtectionRequest("scheduled-backup-cancel")
	ctx, cancel := context.WithCancel(context.Background())

	err := engine.ExecuteScheduledProtection(ctx, request, func(actionContext context.Context, _ *Engine) (string, error) {
		cancel()
		return "", actionContext.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	records := readScheduledProtectionJournal(t, engine, request.OperationID)
	if len(records) != 2 || records[1].TerminalResult == nil || records[1].TerminalResult.State != "cancelled" || records[1].TerminalResult.ErrorCode != "operation_cancelled" {
		t.Fatalf("cancellation journal = %#v", records)
	}

	reran := false
	err = engine.ExecuteScheduledProtection(context.Background(), request, func(context.Context, *Engine) (string, error) {
		reran = true
		return "unexpected", nil
	})
	var terminal *scheduledTerminalError
	if !errors.As(err, &terminal) || terminal.Code() != "operation_cancelled" || reran {
		t.Fatalf("terminal retry = %v, reran = %v", err, reran)
	}
}

func TestExecuteScheduledProtectionRecordsLockContention(t *testing.T) {
	engine := scheduledProtectionTestEngine(t, transport.NewLocal())
	request := scheduledProtectionRequest("scheduled-backup-conflict")
	if err := os.MkdirAll(engine.protectionLockDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	holder := protectionLockMeta{
		Owner: "operator", OperationID: "restore-running", Service: request.Service,
		Epoch: 3, TTLSeconds: 60, AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
	encoded, _ := json.Marshal(holder)
	if err := os.WriteFile(engine.protectionLockPath(request.Service), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	err := engine.ExecuteScheduledProtection(context.Background(), request, func(context.Context, *Engine) (string, error) {
		t.Fatal("contended scheduled action ran")
		return "", nil
	})
	if !errors.Is(err, ErrProtectionConflict) {
		t.Fatalf("contention error = %v", err)
	}
	records := readScheduledProtectionJournal(t, engine, request.OperationID)
	if len(records) != 2 || records[1].TerminalResult == nil || records[1].TerminalResult.State != "incomplete" || records[1].TerminalResult.ErrorCode != "backup_conflict" || records[1].Retry == nil || records[1].Retry.Class != "retryable" {
		t.Fatalf("contention journal = %#v", records)
	}
}

type disconnectAfterResultAppend struct {
	transport.Transport
	disconnected bool
}

func (target *disconnectAfterResultAppend) Run(ctx context.Context, command string) (transport.Result, error) {
	result, err := target.Transport.Run(ctx, command)
	if err == nil && !target.disconnected && strings.Contains(command, "printf '%s\\n'") && strings.Contains(command, `"event":"result"`) {
		target.disconnected = true
		return result, errors.New("client disconnected after durable append")
	}
	return result, err
}

func TestExecuteScheduledProtectionReconcilesDisconnectedClient(t *testing.T) {
	target := &disconnectAfterResultAppend{Transport: transport.NewLocal()}
	engine := scheduledProtectionTestEngine(t, target)
	request := scheduledProtectionRequest("scheduled-backup-disconnect")
	actionCalls := 0

	if err := engine.ExecuteScheduledProtection(context.Background(), request, func(context.Context, *Engine) (string, error) {
		actionCalls++
		return "backup-generation-disconnect", nil
	}); err != nil {
		t.Fatalf("reconcile disconnected client: %v", err)
	}
	if !target.disconnected || actionCalls != 1 {
		t.Fatalf("disconnect/action = %v/%d", target.disconnected, actionCalls)
	}
	if err := engine.ExecuteScheduledProtection(context.Background(), request, func(context.Context, *Engine) (string, error) {
		actionCalls++
		return "unexpected", nil
	}); err != nil {
		t.Fatalf("terminal retry after disconnect: %v", err)
	}
	if actionCalls != 1 {
		t.Fatalf("action calls after terminal retry = %d, want 1", actionCalls)
	}
}

type scheduledTestFailure struct {
	message string
}

func (failure scheduledTestFailure) Error() string { return failure.message }
func (scheduledTestFailure) Code() string          { return "backup_target_unreachable" }
func (scheduledTestFailure) Retryable() bool       { return true }

func TestExecuteScheduledProtectionRedactsFailureJournal(t *testing.T) {
	engine := scheduledProtectionTestEngine(t, transport.NewLocal())
	request := scheduledProtectionRequest("scheduled-backup-redaction")
	const secret = "storage-secret-canary"

	err := engine.ExecuteScheduledProtection(context.Background(), request, func(context.Context, *Engine) (string, error) {
		return "", scheduledTestFailure{message: "provider rejected " + secret}
	})
	if err == nil {
		t.Fatal("scheduled failure unexpectedly succeeded")
	}
	records := readScheduledProtectionJournal(t, engine, request.OperationID)
	encoded, marshalErr := json.Marshal(records)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("scheduled journal leaked secret: %s", encoded)
	}
	if len(records) != 2 || records[1].ErrorCode != "backup_target_unreachable" || records[1].Detail != "operation failed; inspect trusted local diagnostics" {
		t.Fatalf("redacted failure journal = %#v", records)
	}
}
