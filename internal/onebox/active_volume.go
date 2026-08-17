package onebox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const ActiveVolumeSchemaVersion = "onebox.run/active-volume/v1alpha1"

var (
	ErrActiveVolumeStateMissing = errors.New("active-volume state is missing")
	ErrActiveVolumeStaleEpoch   = errors.New("active-volume state has a stale epoch")
	activeDockerVolume          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
)

type ActiveVolumeSelection struct {
	DockerVolume string `json:"docker_volume"`
	OperationID  string `json:"operation_id"`
	Epoch        int    `json:"epoch"`
}

// ActiveVolumeRecord is the sealed source of truth for the physical Docker
// volume behind one logical service volume. OperationID+Epoch are its fence;
// discovery of an unrelated volume can never advance this record.
type ActiveVolumeRecord struct {
	SchemaVersion      string                 `json:"schema_version"`
	Application        string                 `json:"application"`
	Environment        string                 `json:"environment"`
	Service            string                 `json:"service"`
	LogicalName        string                 `json:"logical_name"`
	SelectedVolume     string                 `json:"selected_docker_volume"`
	SelectionOperation string                 `json:"selection_operation"`
	PreviousSelection  *ActiveVolumeSelection `json:"previous_selection,omitempty"`
	Epoch              int                    `json:"epoch"`
	RecordDigest       string                 `json:"record_digest"`
}

func NewActiveVolumeRecord(application, environment, service, logicalName, selectedVolume, operationID string, epoch int, previous *ActiveVolumeSelection) (ActiveVolumeRecord, error) {
	record := ActiveVolumeRecord{
		SchemaVersion: ActiveVolumeSchemaVersion, Application: application, Environment: environment,
		Service: service, LogicalName: logicalName, SelectedVolume: selectedVolume,
		SelectionOperation: operationID, PreviousSelection: cloneActiveVolumeSelection(previous), Epoch: epoch,
	}
	if err := record.Seal(); err != nil {
		return ActiveVolumeRecord{}, err
	}
	return record, nil
}

func (record ActiveVolumeRecord) canonicalJSON() ([]byte, error) {
	copy := record
	copy.RecordDigest = ""
	return json.Marshal(copy)
}

func (record ActiveVolumeRecord) ComputeDigest() (string, error) {
	encoded, err := record.canonicalJSON()
	if err != nil {
		return "", fmt.Errorf("encode active-volume digest: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (record ActiveVolumeRecord) validateContent() error {
	if record.SchemaVersion != ActiveVolumeSchemaVersion {
		return fmt.Errorf("unsupported active-volume schema %q", record.SchemaVersion)
	}
	for name, value := range map[string]string{
		"application": record.Application, "environment": record.Environment, "service": record.Service,
		"logical_name": record.LogicalName, "selection_operation": record.SelectionOperation,
	} {
		if !safeLifecycleMetadata(value) {
			return fmt.Errorf("active-volume %s is invalid", name)
		}
	}
	if !activeDockerVolume.MatchString(record.SelectedVolume) {
		return errors.New("active-volume selected Docker volume is invalid")
	}
	if record.Epoch <= 0 {
		return errors.New("active-volume epoch must be positive")
	}
	if record.PreviousSelection != nil {
		previous := record.PreviousSelection
		if !activeDockerVolume.MatchString(previous.DockerVolume) || !safeLifecycleMetadata(previous.OperationID) || previous.Epoch <= 0 {
			return errors.New("active-volume previous selection is invalid")
		}
		if previous.Epoch >= record.Epoch {
			return errors.New("active-volume previous selection must have an older epoch")
		}
		if previous.DockerVolume == record.SelectedVolume {
			return errors.New("active-volume previous and selected Docker volumes must differ")
		}
	}
	return nil
}

func (record *ActiveVolumeRecord) Seal() error {
	if record == nil {
		return errors.New("active-volume record is nil")
	}
	if err := record.validateContent(); err != nil {
		return err
	}
	digest, err := record.ComputeDigest()
	if err != nil {
		return err
	}
	record.RecordDigest = digest
	return nil
}

func (record ActiveVolumeRecord) Validate() error {
	if err := record.validateContent(); err != nil {
		return err
	}
	if !lifecycleGraphDigest.MatchString(record.RecordDigest) {
		return errors.New("active-volume record digest is missing or invalid")
	}
	expected, err := record.ComputeDigest()
	if err != nil {
		return err
	}
	if record.RecordDigest != expected {
		return errors.New("active-volume record digest mismatch")
	}
	return nil
}

func (record ActiveVolumeRecord) ValidateEpoch(minimum int) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if record.Epoch < minimum {
		return fmt.Errorf("%w: got %d, require at least %d", ErrActiveVolumeStaleEpoch, record.Epoch, minimum)
	}
	return nil
}

func EncodeActiveVolumeRecord(record ActiveVolumeRecord) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func DecodeActiveVolumeRecord(encoded []byte) (ActiveVolumeRecord, error) {
	if len(bytes.TrimSpace(encoded)) == 0 {
		return ActiveVolumeRecord{}, ErrActiveVolumeStateMissing
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var record ActiveVolumeRecord
	if err := decoder.Decode(&record); err != nil {
		return ActiveVolumeRecord{}, fmt.Errorf("decode active-volume record: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ActiveVolumeRecord{}, errors.New("decode active-volume record: multiple JSON values")
		}
		return ActiveVolumeRecord{}, fmt.Errorf("decode active-volume record: %w", err)
	}
	if err := record.Validate(); err != nil {
		return ActiveVolumeRecord{}, fmt.Errorf("validate active-volume record: %w", err)
	}
	return record, nil
}

func cloneActiveVolumeSelection(selection *ActiveVolumeSelection) *ActiveVolumeSelection {
	if selection == nil {
		return nil
	}
	copy := *selection
	return &copy
}
