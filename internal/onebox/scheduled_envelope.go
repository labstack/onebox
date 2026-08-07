package onebox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ScheduledEnvelopeSchemaVersion   = "onebox.run/scheduled-operation-envelope/v1alpha1"
	CurrentScheduledEnvelopeProtocol = 1
	CurrentScheduledRunnerProtocol   = 1
	CurrentScheduledCLIProtocol      = 1
)

type ProtocolRange struct {
	Minimum int `json:"minimum"`
	Maximum int `json:"maximum"`
}

type ScheduledRunnerCompatibility struct {
	RunnerProtocol    int           `json:"runner_protocol"`
	CLIProtocols      ProtocolRange `json:"cli_protocols"`
	EnvelopeProtocols ProtocolRange `json:"envelope_protocols"`
}

type ScheduledCLICompatibility struct {
	CLIProtocol       int           `json:"cli_protocol"`
	RunnerProtocols   ProtocolRange `json:"runner_protocols"`
	EnvelopeProtocols ProtocolRange `json:"envelope_protocols"`
}

type ScheduledTimingPolicy struct {
	ScheduledFor  string `json:"scheduled_for"`
	NotBefore     string `json:"not_before"`
	ExpiresAt     string `json:"expires_at"`
	MaxRuntime    string `json:"max_runtime"`
	RetryIdentity string `json:"retry_identity"`
}

type ScheduledStateBinding struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Epoch  int    `json:"epoch"`
}

type ScheduledRunnerArtifactReference struct {
	Path         string `json:"path"`
	Digest       string `json:"digest"`
	SBOMDigest   string `json:"sbom_digest"`
	ProvenanceID string `json:"provenance_id"`
}

type ScheduledOperationEnvelope struct {
	SchemaVersion    string                           `json:"schema_version"`
	EnvelopeProtocol int                              `json:"envelope_protocol"`
	CLIProtocol      int                              `json:"cli_protocol"`
	RunnerProtocols  ProtocolRange                    `json:"runner_protocols"`
	OperationID      string                           `json:"operation_id"`
	Application      string                           `json:"application"`
	Environment      string                           `json:"environment"`
	Service          string                           `json:"service"`
	Operation        OperationKind                    `json:"operation"`
	Runner           ScheduledRunnerArtifactReference `json:"runner"`
	Timing           ScheduledTimingPolicy            `json:"timing"`
	Artifacts        []OperationArtifactBinding       `json:"artifacts"`
	State            ScheduledStateBinding            `json:"state"`
	SecretFiles      []SecretSlotReference            `json:"secret_files,omitempty"`
	EnvelopeDigest   string                           `json:"envelope_digest"`
}

type ScheduledEnvelopeInput struct {
	CLIProtocol     int
	RunnerProtocols ProtocolRange
	OperationID     string
	Application     string
	Environment     string
	Service         string
	Operation       OperationKind
	Runner          ScheduledRunnerArtifactReference
	Timing          ScheduledTimingPolicy
	Artifacts       []OperationArtifactBinding
	State           ScheduledStateBinding
	SecretFiles     []SecretSlotReference
}

func CurrentScheduledRunnerCompatibility() ScheduledRunnerCompatibility {
	return ScheduledRunnerCompatibility{
		RunnerProtocol:    CurrentScheduledRunnerProtocol,
		CLIProtocols:      ProtocolRange{Minimum: CurrentScheduledCLIProtocol, Maximum: CurrentScheduledCLIProtocol},
		EnvelopeProtocols: ProtocolRange{Minimum: CurrentScheduledEnvelopeProtocol, Maximum: CurrentScheduledEnvelopeProtocol},
	}
}

func CurrentScheduledCLICompatibility() ScheduledCLICompatibility {
	return ScheduledCLICompatibility{
		CLIProtocol:       CurrentScheduledCLIProtocol,
		RunnerProtocols:   ProtocolRange{Minimum: CurrentScheduledRunnerProtocol, Maximum: CurrentScheduledRunnerProtocol},
		EnvelopeProtocols: ProtocolRange{Minimum: CurrentScheduledEnvelopeProtocol, Maximum: CurrentScheduledEnvelopeProtocol},
	}
}

func NewScheduledOperationEnvelope(input ScheduledEnvelopeInput) (ScheduledOperationEnvelope, error) {
	artifacts := append([]OperationArtifactBinding(nil), input.Artifacts...)
	secretFiles := append([]SecretSlotReference(nil), input.SecretFiles...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Class < artifacts[j].Class })
	sort.Slice(secretFiles, func(i, j int) bool { return secretFiles[i].Slot < secretFiles[j].Slot })
	envelope := ScheduledOperationEnvelope{
		SchemaVersion: ScheduledEnvelopeSchemaVersion, EnvelopeProtocol: CurrentScheduledEnvelopeProtocol,
		CLIProtocol: input.CLIProtocol, RunnerProtocols: input.RunnerProtocols,
		OperationID: input.OperationID, Application: input.Application, Environment: input.Environment,
		Service: input.Service, Operation: input.Operation, Runner: input.Runner, Timing: input.Timing,
		Artifacts: artifacts, State: input.State, SecretFiles: secretFiles,
	}
	if err := envelope.Seal(); err != nil {
		return ScheduledOperationEnvelope{}, err
	}
	return envelope, nil
}

func (envelope *ScheduledOperationEnvelope) Seal() error {
	if envelope == nil {
		return errors.New("scheduled operation envelope is nil")
	}
	if err := envelope.validateContent(); err != nil {
		return err
	}
	digest, err := envelope.computeDigest()
	if err != nil {
		return err
	}
	envelope.EnvelopeDigest = digest
	return nil
}

func (envelope ScheduledOperationEnvelope) Validate() error {
	if err := envelope.validateContent(); err != nil {
		return err
	}
	if !lifecycleGraphDigest.MatchString(envelope.EnvelopeDigest) {
		return errors.New("scheduled envelope digest is missing or invalid")
	}
	expected, err := envelope.computeDigest()
	if err != nil {
		return err
	}
	if envelope.EnvelopeDigest != expected {
		return errors.New("scheduled envelope digest mismatch")
	}
	return nil
}

func (envelope ScheduledOperationEnvelope) ValidateForRunner(runner ScheduledRunnerCompatibility, observedStateDigest string, now time.Time) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	if err := runner.validate(); err != nil {
		return err
	}
	if !envelope.RunnerProtocols.Contains(runner.RunnerProtocol) || !runner.EnvelopeProtocols.Contains(envelope.EnvelopeProtocol) || !runner.CLIProtocols.Contains(envelope.CLIProtocol) {
		return errors.New("scheduled_runner_incompatible: apply a CLI and scheduled runner with mutually supported protocols")
	}
	if observedStateDigest != envelope.State.Digest {
		return errors.New("scheduled_envelope_stale: observed lifecycle state changed; apply the current protection plan")
	}
	notBefore, _ := time.Parse(time.RFC3339Nano, envelope.Timing.NotBefore)
	expiresAt, _ := time.Parse(time.RFC3339Nano, envelope.Timing.ExpiresAt)
	now = now.UTC()
	if now.Before(notBefore) || !now.Before(expiresAt) {
		return errors.New("scheduled_envelope_stale: execution is outside the sealed timing window; apply the current protection plan")
	}
	return nil
}

func ValidateScheduledRunnerForCLI(envelope ScheduledOperationEnvelope, runner ScheduledRunnerCompatibility, cli ScheduledCLICompatibility) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	if err := runner.validate(); err != nil {
		return err
	}
	if err := cli.validate(); err != nil {
		return err
	}
	if envelope.CLIProtocol != cli.CLIProtocol || !cli.RunnerProtocols.Contains(runner.RunnerProtocol) || !cli.EnvelopeProtocols.Contains(envelope.EnvelopeProtocol) ||
		!runner.CLIProtocols.Contains(cli.CLIProtocol) || !runner.EnvelopeProtocols.Contains(envelope.EnvelopeProtocol) || !envelope.RunnerProtocols.Contains(runner.RunnerProtocol) {
		return errors.New("scheduled_runner_incompatible: upgrade or apply Onebox so CLI, runner, and envelope protocol ranges overlap")
	}
	return nil
}

func (envelope ScheduledOperationEnvelope) validateContent() error {
	if envelope.SchemaVersion != ScheduledEnvelopeSchemaVersion || envelope.EnvelopeProtocol <= 0 || envelope.CLIProtocol <= 0 {
		return errors.New("scheduled envelope schema or protocol is invalid")
	}
	if err := envelope.RunnerProtocols.validate("runner protocol"); err != nil {
		return err
	}
	for _, value := range []string{envelope.OperationID, envelope.Application, envelope.Environment, envelope.Service} {
		if !safeLifecycleMetadata(value) {
			return errors.New("scheduled envelope identity must be safe metadata")
		}
	}
	if _, err := LifecycleOperationSchemaFor(envelope.Operation, LifecycleScheduledRunnerSchema); err != nil {
		return err
	}
	if !filepath.IsAbs(envelope.Runner.Path) || filepath.Clean(envelope.Runner.Path) != envelope.Runner.Path ||
		!lifecycleGraphDigest.MatchString(envelope.Runner.Digest) || !lifecycleGraphDigest.MatchString(envelope.Runner.SBOMDigest) ||
		!safeLifecycleMetadata(envelope.Runner.ProvenanceID) {
		return errors.New("scheduled envelope runner artifact is invalid")
	}
	if err := envelope.Timing.validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(envelope.State.Path) || filepath.Clean(envelope.State.Path) != envelope.State.Path ||
		!lifecycleGraphDigest.MatchString(envelope.State.Digest) || envelope.State.Epoch <= 0 {
		return errors.New("scheduled envelope state binding is invalid")
	}
	previous := ""
	for index, artifact := range envelope.Artifacts {
		if !safeLifecycleMetadata(artifact.Class) || !filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path ||
			(artifact.Mode != 0o600 && artifact.Mode != 0o644) || !lifecycleGraphDigest.MatchString(artifact.Digest) {
			return fmt.Errorf("scheduled envelope artifact %d is invalid", index)
		}
		if previous != "" && artifact.Class <= previous {
			return errors.New("scheduled envelope artifacts must be unique and sorted by class")
		}
		previous = artifact.Class
	}
	previous = ""
	for index, secret := range envelope.SecretFiles {
		if !safeLifecycleMetadata(secret.Slot) || !safeLifecycleMetadata(secret.Entry) || !filepath.IsAbs(secret.File) || filepath.Clean(secret.File) != secret.File {
			return fmt.Errorf("scheduled envelope secret_files[%d] is invalid", index)
		}
		if previous != "" && secret.Slot <= previous {
			return errors.New("scheduled envelope secret files must be unique and sorted by slot")
		}
		previous = secret.Slot
	}
	return nil
}

func (timing ScheduledTimingPolicy) validate() error {
	if !safeLifecycleMetadata(timing.RetryIdentity) {
		return errors.New("scheduled timing retry identity is invalid")
	}
	scheduledFor, err := time.Parse(time.RFC3339Nano, timing.ScheduledFor)
	if err != nil {
		return errors.New("scheduled_for must be RFC3339")
	}
	notBefore, err := time.Parse(time.RFC3339Nano, timing.NotBefore)
	if err != nil {
		return errors.New("not_before must be RFC3339")
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, timing.ExpiresAt)
	if err != nil {
		return errors.New("expires_at must be RFC3339")
	}
	if scheduledFor.Before(notBefore) || !scheduledFor.Before(expiresAt) {
		return errors.New("scheduled timing window does not contain scheduled_for")
	}
	maxRuntime, err := time.ParseDuration(timing.MaxRuntime)
	if err != nil || maxRuntime <= 0 || maxRuntime > expiresAt.Sub(notBefore) {
		return errors.New("scheduled max_runtime must fit inside the timing window")
	}
	return nil
}

func (protocols ProtocolRange) Contains(protocol int) bool {
	return protocol >= protocols.Minimum && protocol <= protocols.Maximum
}

func (protocols ProtocolRange) validate(name string) error {
	if protocols.Minimum <= 0 || protocols.Maximum < protocols.Minimum {
		return fmt.Errorf("%s range is invalid", name)
	}
	return nil
}

func (runner ScheduledRunnerCompatibility) validate() error {
	if runner.RunnerProtocol <= 0 {
		return errors.New("scheduled runner protocol is invalid")
	}
	if err := runner.CLIProtocols.validate("runner CLI protocol"); err != nil {
		return err
	}
	return runner.EnvelopeProtocols.validate("runner envelope protocol")
}

func (cli ScheduledCLICompatibility) validate() error {
	if cli.CLIProtocol <= 0 {
		return errors.New("scheduled CLI protocol is invalid")
	}
	if err := cli.RunnerProtocols.validate("CLI runner protocol"); err != nil {
		return err
	}
	return cli.EnvelopeProtocols.validate("CLI envelope protocol")
}

func (envelope ScheduledOperationEnvelope) computeDigest() (string, error) {
	copy := envelope
	copy.EnvelopeDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func EncodeScheduledOperationEnvelope(envelope ScheduledOperationEnvelope) ([]byte, error) {
	if err := envelope.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func DecodeScheduledOperationEnvelope(encoded []byte) (ScheduledOperationEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var envelope ScheduledOperationEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return ScheduledOperationEnvelope{}, fmt.Errorf("decode scheduled envelope: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ScheduledOperationEnvelope{}, errors.New("decode scheduled envelope: multiple JSON values")
		}
		return ScheduledOperationEnvelope{}, err
	}
	if err := envelope.Validate(); err != nil {
		return ScheduledOperationEnvelope{}, err
	}
	return envelope, nil
}

func SaveScheduledOperationEnvelope(path string, envelope ScheduledOperationEnvelope) error {
	encoded, err := EncodeScheduledOperationEnvelope(envelope)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".scheduled-envelope-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func LoadScheduledOperationEnvelope(path string) (ScheduledOperationEnvelope, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return ScheduledOperationEnvelope{}, err
	}
	return DecodeScheduledOperationEnvelope(encoded)
}

func ReadScheduledStateDigest(path string) (string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var state struct {
		StateDigest string `json:"state_digest"`
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(&state); err != nil {
		return "", err
	}
	if !lifecycleGraphDigest.MatchString(state.StateDigest) {
		return "", errors.New("scheduled state file has no valid state_digest")
	}
	return state.StateDigest, nil
}

func ScheduledEnvelopeContainsSecretValue(envelope ScheduledOperationEnvelope, canaries ...string) bool {
	encoded, _ := json.Marshal(envelope)
	for _, canary := range canaries {
		if strings.Contains(string(encoded), canary) {
			return true
		}
	}
	return false
}
