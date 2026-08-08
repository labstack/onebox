package onebox

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/journal"
)

func TestProtectionPublicSurfacesHaveNoCredentialOrDatabaseContentFields(t *testing.T) {
	credentialCanary := "credential-canary-value"
	databaseCanary := "customer@example.invalid: private database row"
	failure, err := NewLifecycleFailure("backup_target_unreachable")
	if err != nil {
		t.Fatal(err)
	}
	event := LifecycleRecord{
		SchemaVersion: LifecycleRecordSchemaVersion,
		Type:          LifecycleRecordEvent,
		OperationID:   "backup-1",
		Kind:          LifecycleBackupCreate,
		Service:       "database",
		Event: &LifecycleEvent{
			Sequence: 1, Time: "2026-08-07T12:00:00Z", Phase: "stream", State: "progress", EvidenceID: "generation-1",
		},
	}
	eventJSON, err := EncodeLifecycleJSON([]LifecycleRecord{event})
	if err != nil {
		t.Fatal(err)
	}
	var eventNDJSON bytes.Buffer
	if err := EncodeLifecycleNDJSON(&eventNDJSON, []LifecycleRecord{event}); err != nil {
		t.Fatal(err)
	}
	journalJSON, err := json.Marshal(journal.Record{
		DeployID: "backup-1", Phase: "backup", Event: "result", Status: "ok",
		OperationKind: "backup_create", Service: "database", ProtectionStepID: "protection-step:0123456789abcdef0123456789abcdef",
		ProtectionAttempt: 1, TerminalResult: &journal.ProtectionTerminalResult{State: "succeeded", EvidenceID: "generation-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	failureJSON, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	// BackupEvidenceReceipt is the existing sealed manifest/evidence surface. It
	// models identities and digests only; protection credentials and database
	// payload bytes have no destination in its public shape.
	manifestJSON, err := json.Marshal(BackupEvidenceReceipt{
		SchemaVersion: BackupEvidenceReceiptSchemaVersion,
		PlanDigest:    "sha256:plan", OperationDigest: "sha256:operation",
		Application: "example", Environment: "production", Target: "offsite",
		RecordedBy: "operator", RecordedAt: "2026-08-07T12:00:00Z", EvidenceDigest: "sha256:evidence",
	})
	if err != nil {
		t.Fatal(err)
	}

	surfaces := map[string][]byte{
		"events": eventJSON, "structured-output": eventNDJSON.Bytes(), "journals": journalJSON,
		"errors": failureJSON, "manifests": manifestJSON,
	}
	for name, encoded := range surfaces {
		for _, forbidden := range []string{credentialCanary, databaseCanary, "credential_value", "database_content"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Errorf("%s leaked %q: %s", name, forbidden, encoded)
			}
		}
	}
}
