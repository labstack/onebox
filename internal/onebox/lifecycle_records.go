package onebox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const (
	LifecycleRecordSchemaVersion   = "onebox.run/lifecycle-record/v1alpha2"
	LifecycleDocumentSchemaVersion = "onebox.run/lifecycle-document/v1alpha1"
)

type LifecycleKind string

const (
	LifecycleProtectionEnable  LifecycleKind = "protection_enable"
	LifecycleProtectionDisable LifecycleKind = "protection_disable"
	LifecycleServiceImagePatch LifecycleKind = "service_image_patch"
	LifecycleBackupCreate      LifecycleKind = "backup_create"
	LifecycleBackupPrune       LifecycleKind = "backup_prune"
	LifecycleReplayArchive     LifecycleKind = "replay_archive"
	LifecycleRestorePrepare    LifecycleKind = "restore_prepare"
	LifecycleRestoreCutover    LifecycleKind = "restore_cutover"
	LifecycleRestoreAbort      LifecycleKind = "restore_abort"
	LifecycleRestoreTest       LifecycleKind = "restore_test"
	LifecycleHygieneRun        LifecycleKind = "hygiene_run"
	LifecycleAssuranceCheck    LifecycleKind = "assurance_check"
	LifecycleServiceTierStatus LifecycleKind = "service_tier_status"
)

type LifecycleRecordType string

const (
	LifecycleRecordEvent  LifecycleRecordType = "event"
	LifecycleRecordResult LifecycleRecordType = "result"
	LifecycleRecordStatus LifecycleRecordType = "service-tier-status"
)

type LifecycleRecord struct {
	SchemaVersion string              `json:"schema_version"`
	Type          LifecycleRecordType `json:"type"`
	OperationID   string              `json:"operation_id"`
	Kind          LifecycleKind       `json:"kind"`
	PatchScope    string              `json:"patch_scope,omitempty"`
	Service       string              `json:"service"`
	Event         *LifecycleEvent     `json:"event,omitempty"`
	Result        *LifecycleResult    `json:"result,omitempty"`
	Status        *ServiceTierStatus  `json:"status,omitempty"`
}

type LifecycleEvent struct {
	Sequence       int                     `json:"sequence"`
	Time           string                  `json:"time"`
	Phase          string                  `json:"phase"`
	State          string                  `json:"state"`
	EvidenceID     string                  `json:"evidence_id,omitempty"`
	NativeEvidence *NativeEvidenceIdentity `json:"native_evidence,omitempty"`
	Recovery       *RecoveryEnvelope       `json:"recovery,omitempty"`
}

type LifecycleResult struct {
	TerminalState      string                  `json:"terminal_state"`
	FinishedAt         string                  `json:"finished_at"`
	EvidenceID         string                  `json:"evidence_id,omitempty"`
	NativeEvidence     *NativeEvidenceIdentity `json:"native_evidence,omitempty"`
	Recovery           *RecoveryEnvelope       `json:"recovery,omitempty"`
	ErrorCode          string                  `json:"error_code,omitempty"`
	DiagnosticCommands []string                `json:"diagnostic_commands,omitempty"`
	NextCommands       []string                `json:"next_commands,omitempty"`
	ResolvingCommands  []string                `json:"resolving_commands,omitempty"`
}

type ServiceTierStatus struct {
	Tier               string                  `json:"tier"`
	ObservedAt         string                  `json:"observed_at"`
	EvidenceID         string                  `json:"evidence_id,omitempty"`
	NativeEvidence     *NativeEvidenceIdentity `json:"native_evidence,omitempty"`
	Recovery           *RecoveryEnvelope       `json:"recovery,omitempty"`
	Codes              []string                `json:"codes,omitempty"`
	DiagnosticCommands []string                `json:"diagnostic_commands,omitempty"`
	NextCommands       []string                `json:"next_commands,omitempty"`
	ResolvingCommands  []string                `json:"resolving_commands,omitempty"`
}

type NativeEvidenceIdentity struct {
	Driver        string `json:"driver"`
	Method        string `json:"method"`
	RepositoryID  string `json:"repository_id,omitempty"`
	GenerationID  string `json:"generation_id,omitempty"`
	ReplayStart   string `json:"replay_start,omitempty"`
	ReplayEnd     string `json:"replay_end,omitempty"`
	ObservationID string `json:"observation_id,omitempty"`
}

type RecoveryEnvelope struct {
	Kind                 string `json:"kind"`
	LatestRecoveryPoint  string `json:"latest_recovery_point,omitempty"`
	WindowStart          string `json:"window_start,omitempty"`
	WindowEnd            string `json:"window_end,omitempty"`
	ObservedRPO          string `json:"observed_rpo,omitempty"`
	ExpectedInterruption string `json:"expected_interruption"`
	EncryptionMode       string `json:"encryption_mode"`
}

type LifecycleDocument struct {
	SchemaVersion string            `json:"schema_version"`
	Records       []LifecycleRecord `json:"records"`
}

var lifecycleMetadata = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,511}$`)

func EncodeLifecycleJSON(records []LifecycleRecord) ([]byte, error) {
	for i := range records {
		if err := records[i].Validate(); err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
	}
	return json.MarshalIndent(LifecycleDocument{SchemaVersion: LifecycleDocumentSchemaVersion, Records: records}, "", "  ")
}

func DecodeLifecycleJSON(body []byte) (LifecycleDocument, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var document LifecycleDocument
	if err := decoder.Decode(&document); err != nil {
		return LifecycleDocument{}, fmt.Errorf("decode lifecycle document: %w", err)
	}
	if err := lifecycleJSONEOF(decoder); err != nil {
		return LifecycleDocument{}, err
	}
	if document.SchemaVersion != LifecycleDocumentSchemaVersion {
		return LifecycleDocument{}, fmt.Errorf("unsupported lifecycle document schema %q", document.SchemaVersion)
	}
	for i := range document.Records {
		if err := document.Records[i].Validate(); err != nil {
			return LifecycleDocument{}, fmt.Errorf("record %d: %w", i, err)
		}
	}
	return document, nil
}

func EncodeLifecycleNDJSON(writer io.Writer, records []LifecycleRecord) error {
	encoder := json.NewEncoder(writer)
	for i := range records {
		if err := records[i].Validate(); err != nil {
			return fmt.Errorf("record %d: %w", i, err)
		}
		if err := encoder.Encode(records[i]); err != nil {
			return fmt.Errorf("encode lifecycle record %d: %w", i, err)
		}
	}
	return nil
}

func DecodeLifecycleNDJSON(reader io.Reader) ([]LifecycleRecord, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, 16<<20))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var records []LifecycleRecord
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var record LifecycleRecord
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("decode lifecycle record %d: %w", len(records), err)
		}
		if err := lifecycleJSONEOF(decoder); err != nil {
			return nil, fmt.Errorf("decode lifecycle record %d: %w", len(records), err)
		}
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("record %d: %w", len(records), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read lifecycle records: %w", err)
	}
	return records, nil
}

func (record LifecycleRecord) Validate() error {
	if record.SchemaVersion != LifecycleRecordSchemaVersion {
		return fmt.Errorf("unsupported lifecycle record schema %q", record.SchemaVersion)
	}
	if !validLifecycleKind(record.Kind) {
		return fmt.Errorf("unsupported lifecycle kind %q", record.Kind)
	}
	if !safeLifecycleMetadata(record.OperationID) {
		return errors.New("operation_id is required and must be secret-free metadata")
	}
	if !safeLifecycleMetadata(record.Service) {
		return errors.New("service is required and must be secret-free metadata")
	}
	if record.Kind == LifecycleServiceImagePatch {
		if record.PatchScope != "pre-protection" && record.PatchScope != "protected" {
			return errors.New("service_image_patch requires patch_scope pre-protection or protected")
		}
	} else if record.PatchScope != "" {
		return errors.New("patch_scope belongs only to service_image_patch")
	}

	set := 0
	if record.Event != nil {
		set++
	}
	if record.Result != nil {
		set++
	}
	if record.Status != nil {
		set++
	}
	if set != 1 {
		return errors.New("a lifecycle record must contain exactly one event, result, or status")
	}
	switch record.Type {
	case LifecycleRecordEvent:
		if record.Event == nil {
			return errors.New("event record has no event")
		}
		return record.Event.validate()
	case LifecycleRecordResult:
		if record.Result == nil {
			return errors.New("result record has no result")
		}
		return record.Result.validate()
	case LifecycleRecordStatus:
		if record.Kind != LifecycleServiceTierStatus || record.Status == nil {
			return errors.New("service-tier-status record has the wrong kind or no status")
		}
		return record.Status.validate()
	default:
		return fmt.Errorf("unsupported lifecycle record type %q", record.Type)
	}
}

func (event LifecycleEvent) validate() error {
	if event.Sequence <= 0 || !safeLifecycleMetadata(event.Phase) || !oneOf(event.State, "started", "progress", "retrying", "succeeded", "failed", "cancelled") {
		return errors.New("event sequence, phase, or state is invalid")
	}
	if event.Time == "" {
		return errors.New("event time is required")
	}
	if event.EvidenceID != "" && !safeLifecycleMetadata(event.EvidenceID) {
		return errors.New("event evidence_id is not safe metadata")
	}
	if event.NativeEvidence != nil {
		if err := event.NativeEvidence.validate(); err != nil {
			return err
		}
	}
	if event.Recovery != nil {
		return event.Recovery.validate()
	}
	return nil
}

func (result LifecycleResult) validate() error {
	if !oneOf(result.TerminalState, "succeeded", "failed", "cancelled", "incomplete") || result.FinishedAt == "" {
		return errors.New("result terminal_state or finished_at is invalid")
	}
	if result.TerminalState == "succeeded" && result.ErrorCode != "" {
		return errors.New("a succeeded result cannot carry error_code")
	}
	guidanceCount := len(result.DiagnosticCommands) + len(result.NextCommands) + len(result.ResolvingCommands)
	if result.TerminalState != "succeeded" && (!safeLifecycleMetadata(result.ErrorCode) || guidanceCount == 0) {
		return errors.New("a non-success result requires a stable error_code and command guidance")
	}
	if result.ErrorCode != "" {
		if _, err := NewLifecycleFailure(result.ErrorCode); err != nil {
			return errors.New("result error_code is not in the stable lifecycle registry")
		}
	}
	if err := validateGuidanceCommands(result.DiagnosticCommands, result.NextCommands, result.ResolvingCommands); err != nil {
		return err
	}
	if result.NativeEvidence != nil {
		if err := result.NativeEvidence.validate(); err != nil {
			return err
		}
	}
	if result.Recovery != nil {
		return result.Recovery.validate()
	}
	return nil
}

func (status ServiceTierStatus) validate() error {
	if !oneOf(status.Tier, "Run", "Managed", "External") || status.ObservedAt == "" {
		return errors.New("service tier or observed_at is invalid")
	}
	for _, code := range status.Codes {
		if !safeLifecycleMetadata(code) {
			return errors.New("service tier code is not safe metadata")
		}
	}
	if err := validateGuidanceCommands(status.DiagnosticCommands, status.NextCommands, status.ResolvingCommands); err != nil {
		return err
	}
	if status.NativeEvidence != nil {
		if err := status.NativeEvidence.validate(); err != nil {
			return err
		}
	}
	if status.Recovery != nil {
		return status.Recovery.validate()
	}
	return nil
}

func (identity NativeEvidenceIdentity) validate() error {
	if !safeLifecycleMetadata(identity.Driver) || !safeLifecycleMetadata(identity.Method) {
		return errors.New("native evidence driver and method are required safe metadata")
	}
	for _, value := range []string{identity.RepositoryID, identity.GenerationID, identity.ReplayStart, identity.ReplayEnd, identity.ObservationID} {
		if value != "" && !safeLifecycleMetadata(value) {
			return errors.New("native evidence identity contains unsafe metadata")
		}
	}
	return nil
}

func (recovery RecoveryEnvelope) validate() error {
	if !oneOf(recovery.Kind, "snapshot", "pitr", "cold") {
		return errors.New("recovery kind is invalid")
	}
	if !oneOf(recovery.EncryptionMode, "client-side", "server-side") {
		return errors.New("recovery encryption_mode is invalid")
	}
	if recovery.ExpectedInterruption == "" {
		return errors.New("recovery expected_interruption is required")
	}
	for _, value := range []string{recovery.LatestRecoveryPoint, recovery.WindowStart, recovery.WindowEnd} {
		if value != "" && !safeLifecycleMetadata(value) {
			return errors.New("recovery envelope contains unsafe identity metadata")
		}
	}
	return nil
}

func validateGuidanceCommands(diagnostic, next, resolving []string) error {
	seen := map[string]bool{}
	for role, commands := range map[string][]string{
		"diagnostic": diagnostic, "next": next, "resolving": resolving,
	} {
		for _, command := range commands {
			if !safeGuidanceCommand(command) {
				return errors.New("guidance command is not a safe Onebox command")
			}
			if GuidanceRoleForCommand(command) != role {
				return fmt.Errorf("%s command is classified as %s guidance", command, GuidanceRoleForCommand(command))
			}
			if seen[command] {
				return errors.New("guidance command appears in more than one role")
			}
			seen[command] = true
		}
	}
	return nil
}

func validLifecycleKind(kind LifecycleKind) bool {
	_, ok := lifecycleEventRegistry[kind]
	return ok
}

func safeLifecycleMetadata(value string) bool { return lifecycleMetadata.MatchString(value) }

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func lifecycleJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode lifecycle JSON: %w", err)
	}
	return errors.New("decode lifecycle JSON: multiple JSON values")
}
