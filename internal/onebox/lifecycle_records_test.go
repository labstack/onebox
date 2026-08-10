package onebox

import (
	"bytes"
	"strings"
	"testing"
)

func TestLifecycleRecordKindsEncodeCompatibly(t *testing.T) {
	kinds := []LifecycleKind{
		LifecycleProtectionEnable, LifecycleProtectionDisable,
		LifecycleBackupCreate, LifecycleReplayArchive,
		LifecycleRestorePrepare, LifecycleRestoreCutover, LifecycleRestoreAbort,
		LifecycleRestoreTest, LifecycleHygieneRun, LifecycleAssuranceCheck,
	}
	var records []LifecycleRecord
	for _, kind := range kinds {
		records = append(records, validLifecycleResultRecord(kind, "postgres"))
	}
	for _, scope := range []string{"pre-protection", "protected"} {
		record := validLifecycleResultRecord(LifecycleServiceImagePatch, "postgres")
		record.PatchScope = scope
		records = append(records, record)
	}
	records = append(records, LifecycleRecord{
		SchemaVersion: LifecycleRecordSchemaVersion,
		Type:          LifecycleRecordEvent,
		OperationID:   "operation-20260807",
		Kind:          LifecycleBackupCreate,
		Service:       "postgres",
		Event: &LifecycleEvent{
			Sequence: 1, Time: "2026-08-07T19:59:00Z", Phase: "native-backup", State: "progress",
			EvidenceID: "evidence-42", NativeEvidence: validNativeEvidence(), Recovery: validRecoveryEnvelope(),
		},
	})
	records = append(records, LifecycleRecord{
		SchemaVersion: LifecycleRecordSchemaVersion,
		Type:          LifecycleRecordStatus,
		OperationID:   "status-20260807",
		Kind:          LifecycleServiceTierStatus,
		Service:       "postgres",
		Status: &ServiceTierStatus{
			Tier: "Managed", ObservedAt: "2026-08-07T20:00:00Z", EvidenceID: "evidence-42",
			NativeEvidence: validNativeEvidence(), Recovery: validRecoveryEnvelope(),
		},
	})

	encoded, err := EncodeLifecycleJSON(records)
	if err != nil {
		t.Fatal(err)
	}
	document, err := DecodeLifecycleJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != LifecycleDocumentSchemaVersion || len(document.Records) != len(records) {
		t.Fatalf("decoded lifecycle document = %#v", document)
	}

	var ndjson bytes.Buffer
	if err := EncodeLifecycleNDJSON(&ndjson, records); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLifecycleNDJSON(&ndjson)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(records) {
		t.Fatalf("decoded %d NDJSON records, want %d", len(decoded), len(records))
	}
	for i, record := range decoded {
		if record.SchemaVersion != LifecycleRecordSchemaVersion || record.Kind != records[i].Kind || record.OperationID != records[i].OperationID {
			t.Fatalf("record %d compatibility mismatch: %#v", i, record)
		}
	}
}

func TestLifecycleDecodersRejectUnknownFields(t *testing.T) {
	record := validLifecycleResultRecord(LifecycleBackupCreate, "postgres")
	encoded, err := EncodeLifecycleJSON([]LifecycleRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	unknownTop := strings.Replace(string(encoded), `"schema_version": "`+LifecycleDocumentSchemaVersion+`"`, `"unexpected": true, "schema_version": "`+LifecycleDocumentSchemaVersion+`"`, 1)
	if _, err := DecodeLifecycleJSON([]byte(unknownTop)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("top-level unknown field error = %v", err)
	}
	unknownNested := strings.Replace(string(encoded), `"terminal_state": "succeeded"`, `"unexpected": true, "terminal_state": "succeeded"`, 1)
	if _, err := DecodeLifecycleJSON([]byte(unknownNested)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("nested unknown field error = %v", err)
	}

	var ndjson bytes.Buffer
	if err := EncodeLifecycleNDJSON(&ndjson, []LifecycleRecord{record}); err != nil {
		t.Fatal(err)
	}
	line := strings.Replace(ndjson.String(), `"result":{`, `"result":{"unexpected":true,`, 1)
	if _, err := DecodeLifecycleNDJSON(strings.NewReader(line)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("NDJSON unknown field error = %v", err)
	}
}

func TestLifecycleRecordsRejectIncompatibleSchemasAndIncompleteFailures(t *testing.T) {
	record := validLifecycleResultRecord(LifecycleBackupCreate, "postgres")
	record.SchemaVersion = "onebox.run/lifecycle-record/v2"
	if err := record.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported lifecycle record schema") {
		t.Fatalf("future record schema error = %v", err)
	}

	record = validLifecycleResultRecord(LifecycleBackupCreate, "postgres")
	record.Result.TerminalState = "failed"
	if err := record.Validate(); err == nil || !strings.Contains(err.Error(), "stable error_code") {
		t.Fatalf("failure contract error = %v", err)
	}
	record.Result.ErrorCode = "backup_target_unreachable"
	record.Result.DiagnosticCommands = []string{"ob backup target inspect --output json"}
	if err := record.Validate(); err != nil {
		t.Fatalf("complete typed failure rejected: %v", err)
	}

	record.Result.DiagnosticCommands = nil
	record.Result.ResolvingCommands = []string{"ob status --output json"}
	if err := record.Validate(); err == nil || !strings.Contains(err.Error(), "classified as diagnostic") {
		t.Fatalf("misclassified read-only guidance error = %v", err)
	}
}

func validLifecycleResultRecord(kind LifecycleKind, service string) LifecycleRecord {
	record := LifecycleRecord{
		SchemaVersion: LifecycleRecordSchemaVersion,
		Type:          LifecycleRecordResult,
		OperationID:   "operation-20260807",
		Kind:          kind,
		Service:       service,
		Result: &LifecycleResult{
			TerminalState: "succeeded",
			FinishedAt:    "2026-08-07T20:00:00Z",
			EvidenceID:    "evidence-42",
		},
	}
	switch kind {
	case LifecycleBackupCreate, LifecycleReplayArchive,
		LifecycleRestorePrepare, LifecycleRestoreCutover, LifecycleRestoreAbort, LifecycleRestoreTest:
		record.Result.NativeEvidence = validNativeEvidence()
		record.Result.Recovery = validRecoveryEnvelope()
	}
	return record
}

func validNativeEvidence() *NativeEvidenceIdentity {
	return &NativeEvidenceIdentity{
		Driver: "postgres", Method: "pgbackrest", RepositoryID: "repository-1",
		GenerationID: "generation-42", ReplayStart: "wal:100", ReplayEnd: "wal:200",
	}
}

func validRecoveryEnvelope() *RecoveryEnvelope {
	return &RecoveryEnvelope{
		Kind: "pitr", LatestRecoveryPoint: "2026-08-07T19:58:00Z",
		WindowStart: "2026-08-01T00:00:00Z", WindowEnd: "2026-08-07T19:58:00Z",
		ObservedRPO: "2m", ExpectedInterruption: "none", EncryptionMode: "client-side",
	}
}
