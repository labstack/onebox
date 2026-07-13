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
	"strings"
	"time"
)

// OperationPlanSchemaVersion identifies the stable, executable operation-plan
// envelope. Plans are rejected across schema versions instead of being
// interpreted with possibly different execution semantics.
const OperationPlanSchemaVersion = "onebox.run/operation-plan/v1alpha1"

type OperationKind string

const (
	KindDeploy       OperationKind = "deploy"
	KindResume       OperationKind = "resume"
	KindAbort        OperationKind = "abort"
	KindRollback     OperationKind = "rollback"
	KindBootstrap    OperationKind = "bootstrap"
	KindServiceApply OperationKind = "service_apply"
	KindProxyApply   OperationKind = "proxy_apply"
	KindSecretsPush  OperationKind = "secrets_push"
	KindDestroy      OperationKind = "destroy"
)

type RiskClass string

const (
	RiskLow      RiskClass = "low"
	RiskModerate RiskClass = "moderate"
	RiskHigh     RiskClass = "high"
	RiskCritical RiskClass = "critical"
)

type ReversibilityClass string

const (
	ReversibilityReversible   ReversibilityClass = "reversible"
	ReversibilityConditional  ReversibilityClass = "conditional"
	ReversibilityIrreversible ReversibilityClass = "irreversible"
)

type ApprovalClass string

const (
	ApprovalNone       ApprovalClass = "none"
	ApprovalStanding   ApprovalClass = "standing"
	ApprovalOneTime    ApprovalClass = "one_time"
	ApprovalStrong     ApprovalClass = "strong"
	ApprovalBreakGlass ApprovalClass = "break_glass"
)

type OperationStepKind string

const (
	StepPreflight       OperationStepKind = "preflight"
	StepTransfer        OperationStepKind = "transfer"
	StepJob             OperationStepKind = "job"
	StepHook            OperationStepKind = "hook"
	StepWorkloadRelease OperationStepKind = "workload_release"
	StepVerify          OperationStepKind = "verify"
	StepActivate        OperationStepKind = "activate"
)

type DataEffectClass string

const (
	DataEffectNone      DataEffectClass = "none"
	DataEffectMigration DataEffectClass = "migration"
	DataEffectUnknown   DataEffectClass = "unknown"
)

// OperationBinding identifies the exact authority and state against which a
// plan was calculated. PayloadDigest may be empty for operations that do not
// transfer a release, but the remaining fields are required for every plan.
type OperationBinding struct {
	Application   string `json:"application"`
	Environment   string `json:"environment"`
	Target        string `json:"target"`
	ConfigDigest  string `json:"config_digest"`
	ComposeDigest string `json:"compose_digest"`
	StateDigest   string `json:"state_digest"`
	PayloadDigest string `json:"payload_digest,omitempty"`
	// Live digests bind the baseline used for diff and no-op decisions without
	// persisting live Compose or secret payload bytes.
	LiveComposeDigest string `json:"live_compose_digest,omitempty"`
	LivePayloadDigest string `json:"live_payload_digest,omitempty"`
}

// OperationStep is executable structure rather than rendered shell text.
// Lifecycle hooks identify only the configured seam; their bodies are never
// copied into a plan.
type OperationStep struct {
	ID         string            `json:"id"`
	Kind       OperationStepKind `json:"kind"`
	DependsOn  []string          `json:"depends_on,omitempty"`
	Component  string            `json:"component,omitempty"`
	Service    string            `json:"service,omitempty"`
	DataEffect DataEffectClass   `json:"data_effect,omitempty"`
	Strategy   string            `json:"strategy,omitempty"`
	Mutation   bool              `json:"mutation"`
}

// OperationPlan is the canonical executable representation shared by every
// adapter. Human-readable commands and approval cards are renderings of it.
type OperationPlan struct {
	SchemaVersion string             `json:"schema_version"`
	ID            string             `json:"id"`
	Kind          OperationKind      `json:"kind"`
	ReleaseID     string             `json:"release_id,omitempty"`
	CreatedAt     string             `json:"created_at"`
	ExpiresAt     string             `json:"expires_at"`
	Risk          RiskClass          `json:"risk"`
	Reversibility ReversibilityClass `json:"reversibility"`
	Approval      ApprovalClass      `json:"approval"`
	Binding       OperationBinding   `json:"binding"`
	Steps         []OperationStep    `json:"steps"`
	PlanDigest    string             `json:"plan_digest"`
}

// CanonicalJSON returns the deterministic digest input. PlanDigest is always
// excluded, preventing a self-referential identity.
func (p OperationPlan) CanonicalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SchemaVersion string             `json:"schema_version"`
		ID            string             `json:"id"`
		Kind          OperationKind      `json:"kind"`
		ReleaseID     string             `json:"release_id,omitempty"`
		CreatedAt     string             `json:"created_at"`
		ExpiresAt     string             `json:"expires_at"`
		Risk          RiskClass          `json:"risk"`
		Reversibility ReversibilityClass `json:"reversibility"`
		Approval      ApprovalClass      `json:"approval"`
		Binding       OperationBinding   `json:"binding"`
		Steps         []OperationStep    `json:"steps"`
	}{
		SchemaVersion: p.SchemaVersion,
		ID:            p.ID,
		Kind:          p.Kind,
		ReleaseID:     p.ReleaseID,
		CreatedAt:     p.CreatedAt,
		ExpiresAt:     p.ExpiresAt,
		Risk:          p.Risk,
		Reversibility: p.Reversibility,
		Approval:      p.Approval,
		Binding:       p.Binding,
		Steps:         p.Steps,
	})
}

func (p OperationPlan) ComputeDigest() (string, error) {
	canonical, err := p.CanonicalJSON()
	if err != nil {
		return "", fmt.Errorf("encode operation plan: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Seal validates a plan and binds its current contents to PlanDigest.
func (p *OperationPlan) Seal() error {
	if p == nil {
		return errors.New("operation plan is nil")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	digest, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	p.PlanDigest = digest
	return nil
}

func (p OperationPlan) VerifyDigest() error {
	if p.PlanDigest == "" {
		return errors.New("plan_digest is required")
	}
	expected, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if p.PlanDigest != expected {
		return fmt.Errorf("plan digest mismatch: got %q, expected %q", p.PlanDigest, expected)
	}
	return nil
}

func (p OperationPlan) Validate() error {
	if p.SchemaVersion != OperationPlanSchemaVersion {
		return fmt.Errorf("schema_version must be %q", OperationPlanSchemaVersion)
	}
	if strings.TrimSpace(p.ID) == "" {
		return errors.New("id is required")
	}
	if !validOperationKind(p.Kind) {
		return fmt.Errorf("unknown operation kind %q", p.Kind)
	}
	if (p.Kind == KindDeploy || p.Kind == KindResume) && strings.TrimSpace(p.ReleaseID) == "" {
		return fmt.Errorf("release_id is required for %s", p.Kind)
	}
	if strings.TrimSpace(p.CreatedAt) == "" {
		return errors.New("created_at is required")
	}
	if strings.TrimSpace(p.ExpiresAt) == "" {
		return errors.New("expires_at is required")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, p.CreatedAt)
	if err != nil {
		return fmt.Errorf("created_at must be RFC3339: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, p.ExpiresAt)
	if err != nil {
		return fmt.Errorf("expires_at must be RFC3339: %w", err)
	}
	if !expiresAt.After(createdAt) {
		return errors.New("expires_at must be after created_at")
	}
	if !validRiskClass(p.Risk) {
		return fmt.Errorf("unknown risk class %q", p.Risk)
	}
	if !validReversibilityClass(p.Reversibility) {
		return fmt.Errorf("unknown reversibility class %q", p.Reversibility)
	}
	if !validApprovalClass(p.Approval) {
		return fmt.Errorf("unknown approval class %q", p.Approval)
	}
	if err := p.Binding.validate(); err != nil {
		return fmt.Errorf("binding: %w", err)
	}
	if len(p.Steps) == 0 {
		return errors.New("steps must not be empty")
	}
	seen := make(map[string]struct{}, len(p.Steps))
	for i, step := range p.Steps {
		if strings.TrimSpace(step.ID) == "" {
			return fmt.Errorf("steps[%d].id is required", i)
		}
		if _, exists := seen[step.ID]; exists {
			return fmt.Errorf("duplicate step id %q", step.ID)
		}
		if !validStepKind(step.Kind) {
			return fmt.Errorf("step %q has unknown kind %q", step.ID, step.Kind)
		}
		if !validDataEffect(step.DataEffect) {
			return fmt.Errorf("step %q has unknown data effect %q", step.ID, step.DataEffect)
		}
		if (step.Kind == StepJob || step.Kind == StepHook || step.Kind == StepWorkloadRelease) && strings.TrimSpace(step.Component) == "" {
			return fmt.Errorf("step %q component is required", step.ID)
		}
		if (step.Kind == StepJob || step.Kind == StepWorkloadRelease) && strings.TrimSpace(step.Service) == "" {
			return fmt.Errorf("step %q service is required", step.ID)
		}
		if step.Kind == StepWorkloadRelease && step.Strategy != "rolling" && step.Strategy != "recreate" {
			return fmt.Errorf("step %q strategy must be rolling or recreate", step.ID)
		}
		for _, dependency := range step.DependsOn {
			if dependency == step.ID {
				return fmt.Errorf("step %q depends on itself", step.ID)
			}
			if _, exists := seen[dependency]; !exists {
				return fmt.Errorf("step %q dependency %q must identify an earlier step", step.ID, dependency)
			}
		}
		seen[step.ID] = struct{}{}
	}
	return nil
}

func (b OperationBinding) validate() error {
	required := []struct {
		name  string
		value string
	}{
		{"application", b.Application},
		{"environment", b.Environment},
		{"target", b.Target},
		{"config_digest", b.ConfigDigest},
		{"compose_digest", b.ComposeDigest},
		{"state_digest", b.StateDigest},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	return nil
}

// Save seals and atomically writes a plan. The receiver is updated with the
// digest that was persisted.
func (p *OperationPlan) Save(path string) error {
	return SaveOperationPlan(path, p)
}

func SaveOperationPlan(path string, p *OperationPlan) error {
	if err := p.Seal(); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode operation plan: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create operation plan directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".operation-plan-*")
	if err != nil {
		return fmt.Errorf("create operation plan: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect operation plan: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write operation plan: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close operation plan: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish operation plan: %w", err)
	}
	return nil
}

func LoadOperationPlan(path string) (OperationPlan, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return OperationPlan{}, fmt.Errorf("read operation plan: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var plan OperationPlan
	if err := decoder.Decode(&plan); err != nil {
		return OperationPlan{}, fmt.Errorf("decode operation plan: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return OperationPlan{}, err
	}
	if err := plan.Validate(); err != nil {
		return OperationPlan{}, fmt.Errorf("validate operation plan: %w", err)
	}
	if err := plan.VerifyDigest(); err != nil {
		return OperationPlan{}, err
	}
	return plan, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode operation plan: %w", err)
	}
	return errors.New("decode operation plan: multiple JSON values")
}

func validOperationKind(kind OperationKind) bool {
	switch kind {
	case KindDeploy, KindResume, KindAbort, KindRollback, KindBootstrap,
		KindServiceApply, KindProxyApply, KindSecretsPush, KindDestroy:
		return true
	default:
		return false
	}
}

func validRiskClass(class RiskClass) bool {
	switch class {
	case RiskLow, RiskModerate, RiskHigh, RiskCritical:
		return true
	default:
		return false
	}
}

func validReversibilityClass(class ReversibilityClass) bool {
	switch class {
	case ReversibilityReversible, ReversibilityConditional, ReversibilityIrreversible:
		return true
	default:
		return false
	}
}

func validApprovalClass(class ApprovalClass) bool {
	switch class {
	case ApprovalNone, ApprovalStanding, ApprovalOneTime, ApprovalStrong, ApprovalBreakGlass:
		return true
	default:
		return false
	}
}

func validStepKind(kind OperationStepKind) bool {
	switch kind {
	case StepPreflight, StepTransfer, StepJob, StepHook, StepWorkloadRelease, StepVerify, StepActivate:
		return true
	default:
		return false
	}
}

func validDataEffect(effect DataEffectClass) bool {
	switch effect {
	case DataEffectNone, DataEffectMigration, DataEffectUnknown:
		return true
	default:
		return false
	}
}
