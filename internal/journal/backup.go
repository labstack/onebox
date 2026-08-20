package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

type IncompleteResource struct {
	Kind          string `json:"kind"`
	Identity      string `json:"identity"`
	CleanupState  string `json:"cleanup_state"`
	RetryEligible bool   `json:"retry_eligible"`
}

type RetryClassification struct {
	Class        string `json:"class"`
	ReasonCode   string `json:"reason_code"`
	RetryAfterMS int    `json:"retry_after_ms,omitempty"`
}

type HelperProvenance struct {
	Repository   string `json:"repository"`
	Digest       string `json:"digest"`
	SBOMDigest   string `json:"sbom_digest"`
	ProvenanceID string `json:"provenance_id"`
}

type BackupTerminalResult struct {
	State      string `json:"state"`
	EvidenceID string `json:"evidence_id,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
}

var (
	backupJournalMetadata = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	backupJournalDigest   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// BackupStepID derives a stable, secret-free identity from structural
// operation fields. Retries use the same ID and increment BackupAttempt.
func BackupStepID(operationKind, service, step string) (string, error) {
	for name, value := range map[string]string{"operation kind": operationKind, "service": service, "step": step} {
		if !backupJournalMetadata.MatchString(value) {
			return "", fmt.Errorf("%s is invalid backup journal metadata", name)
		}
	}
	sum := sha256.Sum256([]byte(operationKind + "\x00" + service + "\x00" + step))
	return "backup-step:" + hex.EncodeToString(sum[:16]), nil
}

// AppendBackup validates a backup record, skips an already-observed
// identical retry, and reconciles a transport error by reading the durable
// journal. This closes the "host appended, client lost output" ambiguity.
func (w *Writer) AppendBackup(ctx context.Context, record Record) error {
	if err := validateBackupRecord(record); err != nil {
		return err
	}
	if existing, ok, err := LookupBackupStep(ctx, w.T, w.Names, w.DeployID, record.BackupStepID); err != nil {
		return err
	} else if ok && backupRecordAlreadyApplied(existing, record) {
		return nil
	}
	appendErr := w.Append(ctx, record)
	if appendErr == nil {
		return nil
	}
	existing, ok, lookupErr := LookupBackupStep(ctx, w.T, w.Names, w.DeployID, record.BackupStepID)
	if lookupErr == nil && ok && backupRecordAlreadyApplied(existing, record) {
		return nil
	}
	if lookupErr != nil {
		return errors.Join(appendErr, fmt.Errorf("reconcile backup journal: %w", lookupErr))
	}
	return appendErr
}

// LookupBackupStep returns the latest durable attempt/state for one
// deterministic step. Torn or unrelated records remain tolerated by Read.
func LookupBackupStep(ctx context.Context, t transport.Transport, names app.Names, operationID, stepID string) (Record, bool, error) {
	records, err := Read(ctx, t, names, operationID)
	if err != nil {
		return Record{}, false, err
	}
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].BackupStepID == stepID {
			return records[index], true, nil
		}
	}
	return Record{}, false, nil
}

func LookupBackupTerminalResult(ctx context.Context, t transport.Transport, names app.Names, operationID string) (BackupTerminalResult, bool, error) {
	records, err := Read(ctx, t, names, operationID)
	if err != nil {
		return BackupTerminalResult{}, false, err
	}
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].TerminalResult != nil {
			return *records[index].TerminalResult, true, nil
		}
	}
	return BackupTerminalResult{}, false, nil
}

func validateBackupRecord(record Record) error {
	if !backupJournalMetadata.MatchString(record.OperationKind) || !backupJournalMetadata.MatchString(record.Service) {
		return errors.New("backup journal operation kind and service are required safe metadata")
	}
	if !strings.HasPrefix(record.BackupStepID, "backup-step:") || len(record.BackupStepID) != len("backup-step:")+32 {
		return errors.New("backup journal step identity is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(record.BackupStepID, "backup-step:")); err != nil {
		return errors.New("backup journal step identity is invalid")
	}
	if record.BackupAttempt <= 0 {
		return errors.New("backup journal attempt must be positive")
	}
	for _, resource := range record.IncompleteResources {
		if !stringOneOf(resource.Kind, "remote-partial", "local-staging", "helper") ||
			!backupJournalMetadata.MatchString(resource.Identity) ||
			!stringOneOf(resource.CleanupState, "pending", "cleaned", "retained") {
			return errors.New("backup journal contains an invalid incomplete resource")
		}
	}
	if record.Retry != nil {
		if !stringOneOf(record.Retry.Class, "retryable", "resumable", "terminal") ||
			!backupJournalMetadata.MatchString(record.Retry.ReasonCode) || record.Retry.RetryAfterMS < 0 {
			return errors.New("backup journal retry classification is invalid")
		}
	}
	if record.HelperProvenance != nil {
		helper := record.HelperProvenance
		if !backupJournalMetadata.MatchString(helper.Repository) ||
			!backupJournalDigest.MatchString(helper.Digest) ||
			!backupJournalDigest.MatchString(helper.SBOMDigest) ||
			!backupJournalMetadata.MatchString(helper.ProvenanceID) {
			return errors.New("backup journal helper provenance is incomplete or unpinned")
		}
	}
	if record.TerminalResult != nil {
		terminal := record.TerminalResult
		if !stringOneOf(terminal.State, "succeeded", "failed", "cancelled", "incomplete") {
			return errors.New("backup journal terminal state is invalid")
		}
		if terminal.EvidenceID != "" && !backupJournalMetadata.MatchString(terminal.EvidenceID) {
			return errors.New("backup journal terminal evidence identity is invalid")
		}
		if terminal.State == "succeeded" && terminal.ErrorCode != "" {
			return errors.New("successful backup journal result cannot have an error code")
		}
		if terminal.State != "succeeded" && !backupJournalMetadata.MatchString(terminal.ErrorCode) {
			return errors.New("non-success backup journal result requires an error code")
		}
	}
	return nil
}

func backupRecordAlreadyApplied(existing, candidate Record) bool {
	if existing.BackupAttempt != candidate.BackupAttempt || existing.Event != candidate.Event || existing.Status != candidate.Status {
		return false
	}
	return reflect.DeepEqual(existing.IncompleteResources, candidate.IncompleteResources) &&
		reflect.DeepEqual(existing.Retry, candidate.Retry) &&
		reflect.DeepEqual(existing.HelperProvenance, candidate.HelperProvenance) &&
		reflect.DeepEqual(existing.TerminalResult, candidate.TerminalResult)
}

func stringOneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
