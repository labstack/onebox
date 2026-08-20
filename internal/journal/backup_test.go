package journal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestBackupStepIdentityIsDeterministicAndStructural(t *testing.T) {
	first, err := BackupStepID("backup_create", "database", "stream-artifact")
	if err != nil {
		t.Fatal(err)
	}
	second, err := BackupStepID("backup_create", "database", "stream-artifact")
	if err != nil {
		t.Fatal(err)
	}
	different, err := BackupStepID("restore_test", "database", "stream-artifact")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == different {
		t.Fatalf("step identities = %q, %q, %q", first, second, different)
	}
}

func TestAppendBackupReconcilesRetryAfterStreamDisconnect(t *testing.T) {
	stepID, err := BackupStepID("backup_create", "database", "stream-artifact")
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	record := Record{
		DeployID:      "backup-1",
		Epoch:         3,
		Phase:         "backup",
		Event:         "result",
		Status:        "fail",
		TS:            "2026-08-07T12:00:00Z",
		OperationKind: "backup_create",
		Service:       "database",
		BackupStepID:  stepID,
		BackupAttempt: 1,
		IncompleteResources: []IncompleteResource{{
			Kind: "remote-partial", Identity: "upload-7", CleanupState: "retained", RetryEligible: true,
		}},
		Retry: &RetryClassification{Class: "resumable", ReasonCode: "stream-disconnected", RetryAfterMS: 250},
		HelperProvenance: &HelperProvenance{
			Repository: "restic/restic", Digest: digest, SBOMDigest: digest, ProvenanceID: "onebox/catalog/restic/v1",
		},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	readCount := 0
	fake := &transport.Fake{
		Err: func(command string) error {
			if strings.Contains(command, "printf '%s\\n'") {
				return errors.New("ssh stream disconnected after remote append")
			}
			return nil
		},
		Dynamic: func(command string) (transport.Result, bool) {
			if strings.HasPrefix(command, "cat ") && strings.Contains(command, "backup-1.jsonl") {
				readCount++
				if readCount == 1 {
					return transport.Result{}, true
				}
				return transport.Result{Stdout: string(encoded) + "\n"}, true
			}
			return transport.Result{}, false
		},
	}
	writer := &Writer{T: fake, Names: app.Names{App: "example", BasePath: "/var/lib/ob"}, DeployID: "backup-1", Epoch: 3}

	candidate := record
	candidate.DeployID, candidate.Epoch, candidate.TS = "", 0, ""
	if err := writer.AppendBackup(context.Background(), candidate); err != nil {
		t.Fatalf("append backup after output loss: %v", err)
	}
	if readCount != 2 {
		t.Fatalf("journal reads = %d, want preflight plus reconciliation", readCount)
	}
}

func TestLookupBackupTerminalResultAfterClientOutputLoss(t *testing.T) {
	stepID, err := BackupStepID("backup_create", "database", "record-result")
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		DeployID: "backup-2", Phase: "backup", Event: "finish", Status: "ok", TS: "2026-08-07T12:01:00Z",
		OperationKind: "backup_create", Service: "database", BackupStepID: stepID, BackupAttempt: 1,
		TerminalResult: &BackupTerminalResult{State: "succeeded", EvidenceID: "backup-generation-9"},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.HasPrefix(command, "cat ") && strings.Contains(command, "backup-2.jsonl") {
			return transport.Result{Stdout: string(encoded) + "\n"}, true
		}
		return transport.Result{}, false
	}}

	result, ok, err := LookupBackupTerminalResult(
		context.Background(), fake, app.Names{App: "example", BasePath: "/var/lib/ob"}, "backup-2",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || result.State != "succeeded" || result.EvidenceID != "backup-generation-9" {
		t.Fatalf("terminal lookup = %#v, %v", result, ok)
	}
}

func TestBackupJournalRejectsUnpinnedHelperAndInvalidIncompleteResource(t *testing.T) {
	stepID, err := BackupStepID("backup_create", "database", "stream-artifact")
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		OperationKind: "backup_create", Service: "database", BackupStepID: stepID, BackupAttempt: 1,
		IncompleteResources: []IncompleteResource{{Kind: "database-row", Identity: "unsafe", CleanupState: "pending"}},
		HelperProvenance:    &HelperProvenance{Repository: "restic/restic", Digest: "latest"},
	}
	if err := validateBackupRecord(record); err == nil {
		t.Fatal("invalid backup journal metadata was accepted")
	}
}
