package onebox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/labstack/onebox/internal/engine"
)

const (
	ExecutableDeployPlanSchemaVersion = "onebox.run/executable-deploy-plan/v1alpha1"
	OperationEventSchemaVersion       = "onebox.run/operation-event/v1alpha1"
)

// DeployPlan is the local executable envelope. Operation is the canonical
// adapter-independent graph; Artifact retains the engine's exact drift and
// render binding. Neither contains decrypted secret values.
type DeployPlan struct {
	SchemaVersion string          `json:"schema_version"`
	Operation     OperationPlan   `json:"operation"`
	Artifact      engine.Artifact `json:"artifact"`
	Diff          string          `json:"diff,omitempty"`
	NoOp          bool            `json:"no_op"`
	PlanDigest    string          `json:"plan_digest"`
}

func artifactDigest(artifact engine.Artifact) (string, error) {
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return "", fmt.Errorf("encode engine artifact: %w", err)
	}
	return engine.HashBytes(encoded), nil
}

func (p DeployPlan) validateContent() error {
	if p.SchemaVersion != ExecutableDeployPlanSchemaVersion {
		return fmt.Errorf("schema_version must be %q", ExecutableDeployPlanSchemaVersion)
	}
	if err := p.Operation.Validate(); err != nil {
		return fmt.Errorf("operation: %w", err)
	}
	if err := p.Operation.VerifyDigest(); err != nil {
		return fmt.Errorf("operation: %w", err)
	}
	if p.Operation.Kind != KindDeploy {
		return fmt.Errorf("operation kind must be %q", KindDeploy)
	}
	if p.Artifact.ID == "" || p.Artifact.App == "" || p.Artifact.Env == "" {
		return errors.New("artifact id, app, and env are required")
	}
	if p.Artifact.ID != p.Operation.ReleaseID {
		return errors.New("artifact id does not match operation release_id")
	}
	if p.Artifact.App != p.Operation.Binding.Application {
		return errors.New("artifact app does not match operation binding")
	}
	if p.Artifact.Env != p.Operation.Binding.Environment {
		return errors.New("artifact env does not match operation binding")
	}
	if p.Artifact.ConfigHash != p.Operation.Binding.ConfigDigest {
		return errors.New("artifact config hash does not match operation binding")
	}
	digest, err := artifactDigest(p.Artifact)
	if err != nil {
		return err
	}
	if digest != p.Operation.Binding.StateDigest {
		return errors.New("artifact digest does not match operation state binding")
	}
	if p.Artifact.HostState.CurrentRelease != "" {
		if p.Operation.Binding.LiveComposeDigest == "" || p.Operation.Binding.LivePayloadDigest == "" {
			return errors.New("live compose and payload digests are required for an existing release")
		}
	}
	return nil
}

func (p DeployPlan) ComputeDigest() (string, error) {
	encoded, err := json.Marshal(struct {
		SchemaVersion string          `json:"schema_version"`
		Operation     OperationPlan   `json:"operation"`
		Artifact      engine.Artifact `json:"artifact"`
		Diff          string          `json:"diff,omitempty"`
		NoOp          bool            `json:"no_op"`
	}{
		SchemaVersion: p.SchemaVersion,
		Operation:     p.Operation,
		Artifact:      p.Artifact,
		Diff:          p.Diff,
		NoOp:          p.NoOp,
	})
	if err != nil {
		return "", fmt.Errorf("encode executable deploy plan digest: %w", err)
	}
	return engine.HashBytes(encoded), nil
}

func (p *DeployPlan) Seal() error {
	if p == nil {
		return errors.New("executable deploy plan is nil")
	}
	if err := p.validateContent(); err != nil {
		return err
	}
	digest, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	p.PlanDigest = digest
	return nil
}

func (p DeployPlan) Validate() error {
	if err := p.validateContent(); err != nil {
		return err
	}
	if p.PlanDigest == "" {
		return errors.New("plan_digest is required")
	}
	expected, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if p.PlanDigest != expected {
		return fmt.Errorf("executable deploy plan digest mismatch: got %q, expected %q", p.PlanDigest, expected)
	}
	return nil
}

func (p DeployPlan) Save(path string) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("validate executable deploy plan: %w", err)
	}
	encoded, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode executable deploy plan: %w", err)
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create plan directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".deploy-plan-*")
	if err != nil {
		return fmt.Errorf("create deploy plan: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect deploy plan: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write deploy plan: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close deploy plan: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish deploy plan: %w", err)
	}
	return nil
}

func LoadDeployPlan(path string) (*DeployPlan, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read executable deploy plan: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var plan DeployPlan
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode executable deploy plan: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode executable deploy plan: multiple JSON values")
		}
		return nil, fmt.Errorf("decode executable deploy plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("validate executable deploy plan: %w", err)
	}
	return &plan, nil
}

type OperationEvent struct {
	SchemaVersion string `json:"schema_version"`
	OperationID   string `json:"operation_id"`
	// EvidenceID is the engine's stable release/journal lookup key. It can name
	// intended evidence when an operation fails before its first journal append;
	// consumers must read the journal to determine which evidence was persisted.
	EvidenceID string        `json:"evidence_id,omitempty"`
	Sequence   int           `json:"sequence"`
	Time       string        `json:"time"`
	Kind       OperationKind `json:"kind"`
	Phase      string        `json:"phase"`
	Status     string        `json:"status"`
	Message    string        `json:"message,omitempty"`
}

type EventSink func(OperationEvent)

// ExecutionBinding is the local authority resolved before a destructive
// confirmation. Execute rechecks it so a config edit cannot retarget the
// confirmed operation.
type ExecutionBinding struct {
	Application   string `json:"application"`
	Environment   string `json:"environment"`
	Target        string `json:"target"`
	ConfigDigest  string `json:"config_digest"`
	ComposeDigest string `json:"compose_digest"`
}

// ExecuteRequest is the sole mutation entry point used by local adapters.
// M1 will attach an approval to the same request before exposing it over MCP.
type ExecuteRequest struct {
	Kind            OperationKind
	Plan            *DeployPlan
	Force           bool
	NoRollback      bool
	Redeploy        bool
	RemoveVolumes   bool
	RemoveProxy     bool
	ExpectedBinding *ExecutionBinding
	Events          EventSink
}

type OperationResult struct {
	ID        string        `json:"id"`
	Kind      OperationKind `json:"kind"`
	Status    string        `json:"status"`
	ReleaseID string        `json:"release_id,omitempty"`
	// EvidenceID is the engine's release/journal correlation key, not a claim
	// that a journal append completed. An early failure may leave no evidence.
	EvidenceID string `json:"evidence_id,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	NoOp       bool   `json:"no_op,omitempty"`
}

func parseOperationTime(value, name string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s is not RFC3339: %w", name, err)
	}
	return parsed, nil
}
