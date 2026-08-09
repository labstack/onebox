package onebox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/engine"
)

const (
	ApprovalGrantSchemaVersion = "onebox.run/approval-grant/v1alpha1"
	ApprovalSourceLocalCLI     = "local_cli"
)

// ApprovalGrant is a short-lived, tamper-evident acknowledgement of one exact
// executable plan. It is deliberately redundant: the plan digest is the
// cryptographic binding, while the projected fields make the authority being
// granted reviewable in isolation and auditable without reopening the plan.
//
// A local grant proves an explicit CLI approval ceremony, not an external
// identity-provider signature. Future approval providers can issue the same
// bound contract with an authenticated source and signature.
type ApprovalGrant struct {
	SchemaVersion   string        `json:"schema_version"`
	PlanDigest      string        `json:"plan_digest"`
	OperationDigest string        `json:"operation_digest"`
	Application     string        `json:"application"`
	Environment     string        `json:"environment"`
	Server          string        `json:"server"`
	ConfigDigest    string        `json:"config_digest"`
	ComposeDigest   string        `json:"compose_digest"`
	StateDigest     string        `json:"state_digest"`
	PayloadDigest   string        `json:"payload_digest"`
	LiveStateDigest string        `json:"live_state_digest,omitempty"`
	Risk            RiskClass     `json:"risk"`
	Approval        ApprovalClass `json:"approval"`
	ApprovedBy      string        `json:"approved_by"`
	ApprovedAt      string        `json:"approved_at"`
	ExpiresAt       string        `json:"expires_at"`
	Source          string        `json:"source"`
	ApprovalDigest  string        `json:"approval_digest"`
}

// NewApprovalGrant seals a local approval whose lifetime never exceeds the
// plan's own expiry. The caller is responsible for running the human approval
// ceremony before calling this constructor.
func NewApprovalGrant(plan *DeployPlan, approvedBy string, now time.Time) (ApprovalGrant, error) {
	if plan == nil {
		return ApprovalGrant{}, errors.New("approval requires an executable plan")
	}
	if err := plan.Validate(); err != nil {
		return ApprovalGrant{}, fmt.Errorf("validate approval plan: %w", err)
	}
	if strings.TrimSpace(approvedBy) == "" {
		return ApprovalGrant{}, errors.New("approved_by is required")
	}
	expiresAt, err := parseOperationTime(plan.Operation.ExpiresAt, "plan expires_at")
	if err != nil {
		return ApprovalGrant{}, err
	}
	now = now.UTC()
	if !now.Before(expiresAt) {
		return ApprovalGrant{}, fmt.Errorf("deployment plan expired at %s — re-plan", expiresAt.Format(time.RFC3339))
	}
	binding := plan.Operation.Binding
	grant := ApprovalGrant{
		SchemaVersion:   ApprovalGrantSchemaVersion,
		PlanDigest:      plan.PlanDigest,
		OperationDigest: plan.Operation.PlanDigest,
		Application:     binding.Application,
		Environment:     binding.Environment,
		Server:          binding.Server,
		ConfigDigest:    binding.ConfigDigest,
		ComposeDigest:   binding.ComposeDigest,
		StateDigest:     binding.StateDigest,
		PayloadDigest:   binding.PayloadDigest,
		LiveStateDigest: approvalLiveStateDigest(binding),
		Risk:            plan.Operation.Risk,
		Approval:        plan.Operation.Approval,
		ApprovedBy:      strings.TrimSpace(approvedBy),
		ApprovedAt:      now.Format(time.RFC3339Nano),
		ExpiresAt:       expiresAt.UTC().Format(time.RFC3339Nano),
		Source:          ApprovalSourceLocalCLI,
	}
	if err := grant.Seal(); err != nil {
		return ApprovalGrant{}, err
	}
	return grant, nil
}

func approvalLiveStateDigest(binding OperationBinding) string {
	if binding.LiveComposeDigest == "" && binding.LivePayloadDigest == "" {
		return ""
	}
	encoded, _ := json.Marshal(struct {
		Compose string `json:"compose_digest"`
		Payload string `json:"payload_digest"`
	}{binding.LiveComposeDigest, binding.LivePayloadDigest})
	return engine.HashBytes(encoded)
}

func (a ApprovalGrant) canonicalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SchemaVersion   string        `json:"schema_version"`
		PlanDigest      string        `json:"plan_digest"`
		OperationDigest string        `json:"operation_digest"`
		Application     string        `json:"application"`
		Environment     string        `json:"environment"`
		Server          string        `json:"server"`
		ConfigDigest    string        `json:"config_digest"`
		ComposeDigest   string        `json:"compose_digest"`
		StateDigest     string        `json:"state_digest"`
		PayloadDigest   string        `json:"payload_digest"`
		LiveStateDigest string        `json:"live_state_digest,omitempty"`
		Risk            RiskClass     `json:"risk"`
		Approval        ApprovalClass `json:"approval"`
		ApprovedBy      string        `json:"approved_by"`
		ApprovedAt      string        `json:"approved_at"`
		ExpiresAt       string        `json:"expires_at"`
		Source          string        `json:"source"`
	}{
		a.SchemaVersion, a.PlanDigest, a.OperationDigest,
		a.Application, a.Environment, a.Server,
		a.ConfigDigest, a.ComposeDigest, a.StateDigest, a.PayloadDigest,
		a.LiveStateDigest, a.Risk, a.Approval, a.ApprovedBy,
		a.ApprovedAt, a.ExpiresAt, a.Source,
	})
}

func (a ApprovalGrant) ComputeDigest() (string, error) {
	encoded, err := a.canonicalJSON()
	if err != nil {
		return "", fmt.Errorf("encode approval grant digest: %w", err)
	}
	return engine.HashBytes(encoded), nil
}

func (a ApprovalGrant) validateContent() error {
	if a.SchemaVersion != ApprovalGrantSchemaVersion {
		return fmt.Errorf("unsupported approval schema %q; this runner supports %q", a.SchemaVersion, ApprovalGrantSchemaVersion)
	}
	required := []struct {
		name  string
		value string
	}{
		{"plan_digest", a.PlanDigest},
		{"operation_digest", a.OperationDigest},
		{"application", a.Application},
		{"environment", a.Environment},
		{"target", a.Server},
		{"config_digest", a.ConfigDigest},
		{"compose_digest", a.ComposeDigest},
		{"state_digest", a.StateDigest},
		{"payload_digest", a.PayloadDigest},
		{"approved_by", a.ApprovedBy},
		{"approved_at", a.ApprovedAt},
		{"expires_at", a.ExpiresAt},
		{"source", a.Source},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	if err := validateBoundedText("approved_by", a.ApprovedBy, 256); err != nil {
		return err
	}
	if a.Source != ApprovalSourceLocalCLI {
		return fmt.Errorf("unsupported approval source %q", a.Source)
	}
	if !validRiskClass(a.Risk) {
		return fmt.Errorf("unknown risk class %q", a.Risk)
	}
	if !validApprovalClass(a.Approval) {
		return fmt.Errorf("unknown approval class %q", a.Approval)
	}
	approvedAt, err := parseOperationTime(a.ApprovedAt, "approved_at")
	if err != nil {
		return err
	}
	expiresAt, err := parseOperationTime(a.ExpiresAt, "expires_at")
	if err != nil {
		return err
	}
	if !expiresAt.After(approvedAt) {
		return errors.New("approval expires_at must be after approved_at")
	}
	return nil
}

func (a *ApprovalGrant) Seal() error {
	if a == nil {
		return errors.New("approval grant is nil")
	}
	if err := a.validateContent(); err != nil {
		return err
	}
	digest, err := a.ComputeDigest()
	if err != nil {
		return err
	}
	a.ApprovalDigest = digest
	return nil
}

func (a ApprovalGrant) Validate() error {
	if err := a.validateContent(); err != nil {
		return err
	}
	if a.ApprovalDigest == "" {
		return errors.New("approval_digest is required")
	}
	expected, err := a.ComputeDigest()
	if err != nil {
		return err
	}
	if a.ApprovalDigest != expected {
		return fmt.Errorf("approval grant digest mismatch: got %q, expected %q", a.ApprovalDigest, expected)
	}
	return nil
}

// ValidateForPlan rechecks every projected authority field. It runs inside the
// canonical execution boundary, so adapters cannot weaken the binding by
// validating a subset before calling Execute.
func (a ApprovalGrant) ValidateForPlan(plan *DeployPlan, now time.Time) error {
	if plan == nil {
		return errors.New("approval has no executable plan")
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("validate approved plan: %w", err)
	}
	binding := plan.Operation.Binding
	want := ApprovalGrant{
		PlanDigest:      plan.PlanDigest,
		OperationDigest: plan.Operation.PlanDigest,
		Application:     binding.Application,
		Environment:     binding.Environment,
		Server:          binding.Server,
		ConfigDigest:    binding.ConfigDigest,
		ComposeDigest:   binding.ComposeDigest,
		StateDigest:     binding.StateDigest,
		PayloadDigest:   binding.PayloadDigest,
		LiveStateDigest: approvalLiveStateDigest(binding),
		Risk:            plan.Operation.Risk,
		Approval:        plan.Operation.Approval,
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"application", a.Application, want.Application},
		{"environment", a.Environment, want.Environment},
		{"target", a.Server, want.Server},
		{"risk", a.Risk, want.Risk},
		{"approval class", a.Approval, want.Approval},
		{"config digest", a.ConfigDigest, want.ConfigDigest},
		{"compose digest", a.ComposeDigest, want.ComposeDigest},
		{"observed state digest", a.StateDigest, want.StateDigest},
		{"payload digest", a.PayloadDigest, want.PayloadDigest},
		{"live state digest", a.LiveStateDigest, want.LiveStateDigest},
		{"operation digest", a.OperationDigest, want.OperationDigest},
		{"plan digest", a.PlanDigest, want.PlanDigest},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("approval %s does not match the executable plan", check.name)
		}
	}
	approvedAt, _ := parseOperationTime(a.ApprovedAt, "approved_at")
	expiresAt, _ := parseOperationTime(a.ExpiresAt, "expires_at")
	planCreatedAt, _ := parseOperationTime(plan.Operation.CreatedAt, "plan created_at")
	planExpiresAt, _ := parseOperationTime(plan.Operation.ExpiresAt, "plan expires_at")
	if approvedAt.Before(planCreatedAt) {
		return errors.New("approval predates the executable plan")
	}
	if expiresAt.After(planExpiresAt) {
		return errors.New("approval outlives the executable plan")
	}
	now = now.UTC()
	if expiresAt.Before(now) {
		return fmt.Errorf("approval expired at %s — approve the current plan again", expiresAt.Format(time.RFC3339))
	}
	if approvedAt.After(now.Add(time.Minute)) {
		return errors.New("approval was created in the future — check the runner clock")
	}
	return nil
}

func (a ApprovalGrant) Save(path string) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("validate approval grant: %w", err)
	}
	encoded, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("encode approval grant: %w", err)
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create approval directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".approval-grant-*")
	if err != nil {
		return fmt.Errorf("create approval grant: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect approval grant: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write approval grant: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close approval grant: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish approval grant: %w", err)
	}
	return nil
}

func LoadApprovalGrant(path string) (*ApprovalGrant, error) {
	encoded, err := readBoundedArtifact(path, "approval grant", maxApprovalGrantBytes)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var approval ApprovalGrant
	if err := decoder.Decode(&approval); err != nil {
		return nil, fmt.Errorf("decode approval grant: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode approval grant: multiple JSON values")
		}
		return nil, fmt.Errorf("decode approval grant: %w", err)
	}
	if err := approval.Validate(); err != nil {
		return nil, fmt.Errorf("validate approval grant: %w", err)
	}
	return &approval, nil
}
