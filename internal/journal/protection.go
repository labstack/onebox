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

type ProtectionTerminalResult struct {
	State      string `json:"state"`
	EvidenceID string `json:"evidence_id,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
}

var (
	protectionJournalMetadata = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	protectionJournalDigest   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ProtectionStepID derives a stable, secret-free identity from structural
// operation fields. Retries use the same ID and increment ProtectionAttempt.
func ProtectionStepID(operationKind, service, step string) (string, error) {
	for name, value := range map[string]string{"operation kind": operationKind, "service": service, "step": step} {
		if !protectionJournalMetadata.MatchString(value) {
			return "", fmt.Errorf("%s is invalid protection journal metadata", name)
		}
	}
	sum := sha256.Sum256([]byte(operationKind + "\x00" + service + "\x00" + step))
	return "protection-step:" + hex.EncodeToString(sum[:16]), nil
}

// AppendProtection validates a protection record, skips an already-observed
// identical retry, and reconciles a transport error by reading the durable
// journal. This closes the "host appended, client lost output" ambiguity.
func (w *Writer) AppendProtection(ctx context.Context, record Record) error {
	if err := validateProtectionRecord(record); err != nil {
		return err
	}
	if existing, ok, err := LookupProtectionStep(ctx, w.T, w.Names, w.DeployID, record.ProtectionStepID); err != nil {
		return err
	} else if ok && protectionRecordAlreadyApplied(existing, record) {
		return nil
	}
	appendErr := w.Append(ctx, record)
	if appendErr == nil {
		return nil
	}
	existing, ok, lookupErr := LookupProtectionStep(ctx, w.T, w.Names, w.DeployID, record.ProtectionStepID)
	if lookupErr == nil && ok && protectionRecordAlreadyApplied(existing, record) {
		return nil
	}
	if lookupErr != nil {
		return errors.Join(appendErr, fmt.Errorf("reconcile protection journal: %w", lookupErr))
	}
	return appendErr
}

// LookupProtectionStep returns the latest durable attempt/state for one
// deterministic step. Torn or unrelated records remain tolerated by Read.
func LookupProtectionStep(ctx context.Context, t transport.Transport, names app.Names, operationID, stepID string) (Record, bool, error) {
	records, err := Read(ctx, t, names, operationID)
	if err != nil {
		return Record{}, false, err
	}
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].ProtectionStepID == stepID {
			return records[index], true, nil
		}
	}
	return Record{}, false, nil
}

func LookupProtectionTerminalResult(ctx context.Context, t transport.Transport, names app.Names, operationID string) (ProtectionTerminalResult, bool, error) {
	records, err := Read(ctx, t, names, operationID)
	if err != nil {
		return ProtectionTerminalResult{}, false, err
	}
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].TerminalResult != nil {
			return *records[index].TerminalResult, true, nil
		}
	}
	return ProtectionTerminalResult{}, false, nil
}

func validateProtectionRecord(record Record) error {
	if !protectionJournalMetadata.MatchString(record.OperationKind) || !protectionJournalMetadata.MatchString(record.Service) {
		return errors.New("protection journal operation kind and service are required safe metadata")
	}
	if !strings.HasPrefix(record.ProtectionStepID, "protection-step:") || len(record.ProtectionStepID) != len("protection-step:")+32 {
		return errors.New("protection journal step identity is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(record.ProtectionStepID, "protection-step:")); err != nil {
		return errors.New("protection journal step identity is invalid")
	}
	if record.ProtectionAttempt <= 0 {
		return errors.New("protection journal attempt must be positive")
	}
	for _, resource := range record.IncompleteResources {
		if !stringOneOf(resource.Kind, "remote-partial", "local-staging", "helper") ||
			!protectionJournalMetadata.MatchString(resource.Identity) ||
			!stringOneOf(resource.CleanupState, "pending", "cleaned", "retained") {
			return errors.New("protection journal contains an invalid incomplete resource")
		}
	}
	if record.Retry != nil {
		if !stringOneOf(record.Retry.Class, "retryable", "resumable", "terminal") ||
			!protectionJournalMetadata.MatchString(record.Retry.ReasonCode) || record.Retry.RetryAfterMS < 0 {
			return errors.New("protection journal retry classification is invalid")
		}
	}
	if record.HelperProvenance != nil {
		helper := record.HelperProvenance
		if !protectionJournalMetadata.MatchString(helper.Repository) ||
			!protectionJournalDigest.MatchString(helper.Digest) ||
			!protectionJournalDigest.MatchString(helper.SBOMDigest) ||
			!protectionJournalMetadata.MatchString(helper.ProvenanceID) {
			return errors.New("protection journal helper provenance is incomplete or unpinned")
		}
	}
	if record.TerminalResult != nil {
		terminal := record.TerminalResult
		if !stringOneOf(terminal.State, "succeeded", "failed", "cancelled", "incomplete") {
			return errors.New("protection journal terminal state is invalid")
		}
		if terminal.EvidenceID != "" && !protectionJournalMetadata.MatchString(terminal.EvidenceID) {
			return errors.New("protection journal terminal evidence identity is invalid")
		}
		if terminal.State == "succeeded" && terminal.ErrorCode != "" {
			return errors.New("successful protection journal result cannot have an error code")
		}
		if terminal.State != "succeeded" && !protectionJournalMetadata.MatchString(terminal.ErrorCode) {
			return errors.New("non-success protection journal result requires an error code")
		}
	}
	return nil
}

func protectionRecordAlreadyApplied(existing, candidate Record) bool {
	if existing.ProtectionAttempt != candidate.ProtectionAttempt || existing.Event != candidate.Event || existing.Status != candidate.Status {
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
