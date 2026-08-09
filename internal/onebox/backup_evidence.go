package onebox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/journal"
)

const (
	BackupEvidenceReceiptSchemaVersion    = "onebox.run/migration-backup-evidence/v1alpha1"
	MigrationBackupOverrideSchemaVersion  = "onebox.run/migration-backup-override/v1alpha1"
	BackupRestoreTestPassed               = "passed"
	BackupRestoreTestNotTested            = "not_tested"
	MigrationBackupOverrideSourceLocalCLI = "local_cli"
)

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// MigrationBackupRequirement is plan-bound policy, not operator-supplied
// evidence. Its presence means every pending migration step requires either a
// matching receipt or an explicit audited override.
type MigrationBackupRequirement struct {
	MaximumAge          string                    `json:"maximum_age"`
	RequireRestoreTest  bool                      `json:"require_restore_test"`
	Resources           []MigrationBackupResource `json:"resources"`
	RequiredKeyMaterial []string                  `json:"required_key_material,omitempty"`
}

// MigrationBackupResource is a secret-free identity projected from the exact
// resolved config. Including service, type, persistence mode, and volume names
// prevents evidence for a similarly named resource from crossing plans.
type MigrationBackupResource struct {
	Component   string   `json:"component"`
	Service     string   `json:"service"`
	Type        string   `json:"type"`
	Persistence string   `json:"persistence"`
	Volumes     []string `json:"volumes,omitempty"`
}

func migrationBackupRequirement(cfg *app.Resolved, policy app.Policy, steps []OperationStep) (*MigrationBackupRequirement, error) {
	if !policy.RequireMigrationBackup || !hasMigrationStep(steps) {
		return nil, nil
	}
	resources := make([]MigrationBackupResource, 0, len(cfg.Workloads))
	for name, component := range cfg.Workloads {
		if component.Persistence == nil || component.Persistence.Mode == "ephemeral" {
			continue
		}
		volumes := durableVolumeNames(component)
		sort.Strings(volumes)
		resources = append(resources, MigrationBackupResource{
			Component: name, Service: name, Type: component.Role,
			Persistence: component.Persistence.Mode, Volumes: volumes,
		})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Component < resources[j].Component })
	if len(resources) == 0 {
		return nil, errors.New("migration backup policy is enabled but no durable or external component resource is declared")
	}
	keyMaterial := append([]string(nil), policy.MigrationBackupKeyMaterial...)
	sort.Strings(keyMaterial)
	requirement := &MigrationBackupRequirement{
		MaximumAge:          policy.MigrationBackupMaximumAge,
		RequireRestoreTest:  policy.RequireMigrationRestoreTest,
		Resources:           resources,
		RequiredKeyMaterial: keyMaterial,
	}
	if err := requirement.validate(); err != nil {
		return nil, err
	}
	return requirement, nil
}

func hasMigrationStep(steps []OperationStep) bool {
	for _, step := range steps {
		if step.Kind == StepJob && step.DataEffect == DataEffectMigration {
			return true
		}
	}
	return false
}

func (r MigrationBackupRequirement) validate() error {
	maxAge, err := time.ParseDuration(r.MaximumAge)
	if err != nil || maxAge <= 0 {
		return fmt.Errorf("migration backup maximum_age %q must be a positive duration", r.MaximumAge)
	}
	if len(r.Resources) == 0 {
		return errors.New("migration backup resources must not be empty")
	}
	previous := ""
	for i, resource := range r.Resources {
		if err := resource.validate(); err != nil {
			return fmt.Errorf("migration backup resources[%d]: %w", i, err)
		}
		if previous != "" && resource.Component <= previous {
			return errors.New("migration backup resources must be unique and sorted by component")
		}
		previous = resource.Component
	}
	previous = ""
	for _, material := range r.RequiredKeyMaterial {
		if strings.TrimSpace(material) == "" {
			return errors.New("migration backup required key material must not be empty")
		}
		if previous != "" && material <= previous {
			return errors.New("migration backup required key material must be unique and sorted")
		}
		previous = material
	}
	return nil
}

func (r MigrationBackupResource) validate() error {
	for name, value := range map[string]string{
		"component": r.Component, "service": r.Service, "type": r.Type, "persistence": r.Persistence,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if r.Persistence != "durable" && r.Persistence != "external" {
		return fmt.Errorf("persistence must be durable or external, got %q", r.Persistence)
	}
	previous := ""
	for _, volume := range r.Volumes {
		if strings.TrimSpace(volume) == "" {
			return errors.New("volume names must not be empty")
		}
		if previous != "" && volume <= previous {
			return errors.New("volume names must be unique and sorted")
		}
		previous = volume
	}
	return nil
}

type BackupIntegrityEvidence struct {
	ArtifactDigest string `json:"artifact_digest"`
	Method         string `json:"method"`
	ValidatedAt    string `json:"validated_at"`
}

type BackupRestoreTestEvidence struct {
	State            string `json:"state"`
	Method           string `json:"method,omitempty"`
	TestedAt         string `json:"tested_at,omitempty"`
	ValidationDigest string `json:"validation_digest,omitempty"`
}

type BackupKeyMaterialUsabilityEvidence struct {
	Method           string `json:"method"`
	ValidatedAt      string `json:"validated_at"`
	ValidationDigest string `json:"validation_digest"`
}

type MigrationBackupResourceEvidence struct {
	Resource    MigrationBackupResource   `json:"resource"`
	BackupID    string                    `json:"backup_id"`
	CreatedAt   string                    `json:"created_at"`
	Integrity   BackupIntegrityEvidence   `json:"integrity"`
	RestoreTest BackupRestoreTestEvidence `json:"restore_test"`
}

type MigrationBackupKeyMaterialEvidence struct {
	Name      string                             `json:"name"`
	BackupID  string                             `json:"backup_id"`
	CreatedAt string                             `json:"created_at"`
	Integrity BackupIntegrityEvidence            `json:"integrity"`
	Usability BackupKeyMaterialUsabilityEvidence `json:"usability"`
}

// BackupEvidenceReceipt is a tamper-evident attestation about already-created
// backup artifacts. It never runs a backup and never contains key material or
// secret values; it records only artifact identifiers, digests, and validation
// facts bound to one exact executable plan.
type BackupEvidenceReceipt struct {
	SchemaVersion   string                               `json:"schema_version"`
	PlanDigest      string                               `json:"plan_digest"`
	OperationDigest string                               `json:"operation_digest"`
	Application     string                               `json:"application"`
	Environment     string                               `json:"environment"`
	Target          string                               `json:"target"`
	RecordedBy      string                               `json:"recorded_by"`
	RecordedAt      string                               `json:"recorded_at"`
	Resources       []MigrationBackupResourceEvidence    `json:"resources"`
	KeyMaterial     []MigrationBackupKeyMaterialEvidence `json:"key_material,omitempty"`
	EvidenceDigest  string                               `json:"evidence_digest"`
}

func NewBackupEvidenceReceipt(plan *DeployPlan, recordedBy string, recordedAt time.Time, resources []MigrationBackupResourceEvidence, keyMaterial []MigrationBackupKeyMaterialEvidence) (BackupEvidenceReceipt, error) {
	if plan == nil {
		return BackupEvidenceReceipt{}, errors.New("backup evidence requires an executable plan")
	}
	if err := plan.Validate(); err != nil {
		return BackupEvidenceReceipt{}, fmt.Errorf("validate backup evidence plan: %w", err)
	}
	if plan.MigrationBackup == nil {
		return BackupEvidenceReceipt{}, errors.New("executable plan has no migration backup requirement")
	}
	resources = append([]MigrationBackupResourceEvidence(nil), resources...)
	keyMaterial = append([]MigrationBackupKeyMaterialEvidence(nil), keyMaterial...)
	sort.Slice(resources, func(i, j int) bool { return resources[i].Resource.Component < resources[j].Resource.Component })
	sort.Slice(keyMaterial, func(i, j int) bool { return keyMaterial[i].Name < keyMaterial[j].Name })
	binding := plan.Operation.Binding
	receipt := BackupEvidenceReceipt{
		SchemaVersion: BackupEvidenceReceiptSchemaVersion,
		PlanDigest:    plan.PlanDigest, OperationDigest: plan.Operation.PlanDigest,
		Application: binding.Application, Environment: binding.Environment, Target: binding.Target,
		RecordedBy: strings.TrimSpace(recordedBy), RecordedAt: recordedAt.UTC().Format(time.RFC3339Nano),
		Resources: resources, KeyMaterial: keyMaterial,
	}
	if err := receipt.Seal(); err != nil {
		return BackupEvidenceReceipt{}, err
	}
	return receipt, nil
}

func (r BackupEvidenceReceipt) canonicalJSON() ([]byte, error) {
	copy := r
	copy.EvidenceDigest = ""
	return json.Marshal(copy)
}

func (r BackupEvidenceReceipt) ComputeDigest() (string, error) {
	encoded, err := r.canonicalJSON()
	if err != nil {
		return "", fmt.Errorf("encode backup evidence digest: %w", err)
	}
	return engine.HashBytes(encoded), nil
}

func (r BackupEvidenceReceipt) validateContent() error {
	if r.SchemaVersion != BackupEvidenceReceiptSchemaVersion {
		return fmt.Errorf("unsupported backup evidence schema %q; this runner supports %q", r.SchemaVersion, BackupEvidenceReceiptSchemaVersion)
	}
	for name, value := range map[string]string{
		"plan_digest": r.PlanDigest, "operation_digest": r.OperationDigest,
		"application": r.Application, "environment": r.Environment, "target": r.Target,
		"recorded_by": r.RecordedBy, "recorded_at": r.RecordedAt,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if _, err := parseOperationTime(r.RecordedAt, "recorded_at"); err != nil {
		return err
	}
	if err := validateBoundedText("recorded_by", r.RecordedBy, 256); err != nil {
		return err
	}
	if len(r.Resources) == 0 {
		return errors.New("backup evidence resources must not be empty")
	}
	previous := ""
	for i, resource := range r.Resources {
		if err := resource.validate(); err != nil {
			return fmt.Errorf("resources[%d]: %w", i, err)
		}
		if previous != "" && resource.Resource.Component <= previous {
			return errors.New("backup evidence resources must be unique and sorted by component")
		}
		previous = resource.Resource.Component
	}
	previous = ""
	for i, material := range r.KeyMaterial {
		if err := material.validate(); err != nil {
			return fmt.Errorf("key_material[%d]: %w", i, err)
		}
		if previous != "" && material.Name <= previous {
			return errors.New("backup evidence key material must be unique and sorted by name")
		}
		previous = material.Name
	}
	return nil
}

func (r MigrationBackupResourceEvidence) validate() error {
	if err := r.Resource.validate(); err != nil {
		return fmt.Errorf("resource: %w", err)
	}
	if strings.TrimSpace(r.BackupID) == "" {
		return errors.New("backup_id is required")
	}
	if err := validateBoundedText("backup_id", r.BackupID, 512); err != nil {
		return err
	}
	createdAt, err := parseOperationTime(r.CreatedAt, "created_at")
	if err != nil {
		return err
	}
	validatedAt, err := r.Integrity.validate()
	if err != nil {
		return fmt.Errorf("integrity: %w", err)
	}
	if validatedAt.Before(createdAt) {
		return errors.New("integrity validation predates backup creation")
	}
	switch r.RestoreTest.State {
	case BackupRestoreTestPassed:
		if strings.TrimSpace(r.RestoreTest.Method) == "" || strings.TrimSpace(r.RestoreTest.TestedAt) == "" {
			return errors.New("passed restore test requires method and tested_at")
		}
		if err := validateBoundedText("restore_test.method", r.RestoreTest.Method, 256); err != nil {
			return err
		}
		if !sha256Digest.MatchString(r.RestoreTest.ValidationDigest) {
			return errors.New("passed restore test requires a lowercase sha256 validation_digest")
		}
		testedAt, err := parseOperationTime(r.RestoreTest.TestedAt, "restore_test.tested_at")
		if err != nil {
			return err
		}
		if testedAt.Before(createdAt) {
			return errors.New("restore test predates backup creation")
		}
	case BackupRestoreTestNotTested:
		if r.RestoreTest.Method != "" || r.RestoreTest.TestedAt != "" || r.RestoreTest.ValidationDigest != "" {
			return errors.New("not_tested restore state must not include method, tested_at, or validation_digest")
		}
	default:
		return fmt.Errorf("restore_test.state must be %q or %q", BackupRestoreTestPassed, BackupRestoreTestNotTested)
	}
	return nil
}

func (r MigrationBackupKeyMaterialEvidence) validate() error {
	if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.BackupID) == "" {
		return errors.New("name and backup_id are required")
	}
	if err := validateBoundedText("backup_id", r.BackupID, 512); err != nil {
		return err
	}
	createdAt, err := parseOperationTime(r.CreatedAt, "created_at")
	if err != nil {
		return err
	}
	validatedAt, err := r.Integrity.validate()
	if err != nil {
		return fmt.Errorf("integrity: %w", err)
	}
	if validatedAt.Before(createdAt) {
		return errors.New("integrity validation predates key-material backup creation")
	}
	usabilityAt, err := r.Usability.validate()
	if err != nil {
		return fmt.Errorf("usability: %w", err)
	}
	if usabilityAt.Before(createdAt) {
		return errors.New("key-material usability validation predates backup creation")
	}
	return nil
}

func (r BackupIntegrityEvidence) validate() (time.Time, error) {
	if !sha256Digest.MatchString(r.ArtifactDigest) {
		return time.Time{}, errors.New("artifact_digest must be a lowercase sha256 digest")
	}
	if strings.TrimSpace(r.Method) == "" {
		return time.Time{}, errors.New("method is required")
	}
	if err := validateBoundedText("method", r.Method, 256); err != nil {
		return time.Time{}, err
	}
	return parseOperationTime(r.ValidatedAt, "validated_at")
}

func (r BackupKeyMaterialUsabilityEvidence) validate() (time.Time, error) {
	if err := validateBoundedText("method", r.Method, 256); err != nil {
		return time.Time{}, err
	}
	if !sha256Digest.MatchString(r.ValidationDigest) {
		return time.Time{}, errors.New("validation_digest must be a lowercase sha256 digest")
	}
	return parseOperationTime(r.ValidatedAt, "validated_at")
}

func validateBoundedText(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s must be %d bytes or fewer", name, limit)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s must not contain control line breaks or other control characters", name)
	}
	return nil
}

func (r *BackupEvidenceReceipt) Seal() error {
	if r == nil {
		return errors.New("backup evidence receipt is nil")
	}
	if err := r.validateContent(); err != nil {
		return err
	}
	digest, err := r.ComputeDigest()
	if err != nil {
		return err
	}
	r.EvidenceDigest = digest
	return nil
}

func (r BackupEvidenceReceipt) Validate() error {
	if err := r.validateContent(); err != nil {
		return err
	}
	if r.EvidenceDigest == "" {
		return errors.New("evidence_digest is required")
	}
	expected, err := r.ComputeDigest()
	if err != nil {
		return err
	}
	if r.EvidenceDigest != expected {
		return fmt.Errorf("backup evidence digest mismatch: got %q, expected %q", r.EvidenceDigest, expected)
	}
	return nil
}

func (r BackupEvidenceReceipt) ValidateForPlan(plan *DeployPlan, now time.Time) error {
	if plan == nil {
		return errors.New("backup evidence has no executable plan")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("validate backup evidence plan: %w", err)
	}
	requirement := plan.MigrationBackup
	if requirement == nil {
		return errors.New("executable plan has no migration backup requirement")
	}
	binding := plan.Operation.Binding
	checks := []struct{ name, got, want string }{
		{"plan digest", r.PlanDigest, plan.PlanDigest},
		{"operation digest", r.OperationDigest, plan.Operation.PlanDigest},
		{"application", r.Application, binding.Application},
		{"environment", r.Environment, binding.Environment},
		{"target", r.Target, binding.Target},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("backup evidence %s does not match the executable plan", check.name)
		}
	}
	resourceIdentities := make([]MigrationBackupResource, len(r.Resources))
	for i := range r.Resources {
		resourceIdentities[i] = r.Resources[i].Resource
	}
	if !reflect.DeepEqual(resourceIdentities, requirement.Resources) {
		return errors.New("backup evidence resources do not match the executable plan")
	}
	keyNames := make([]string, len(r.KeyMaterial))
	for i := range r.KeyMaterial {
		keyNames[i] = r.KeyMaterial[i].Name
	}
	if !reflect.DeepEqual(keyNames, requirement.RequiredKeyMaterial) {
		return errors.New("backup evidence key material does not match the executable plan")
	}
	maxAge, _ := time.ParseDuration(requirement.MaximumAge)
	now = now.UTC()
	recordedAt, _ := parseOperationTime(r.RecordedAt, "recorded_at")
	planCreatedAt, _ := parseOperationTime(plan.Operation.CreatedAt, "plan created_at")
	planExpiresAt, _ := parseOperationTime(plan.Operation.ExpiresAt, "plan expires_at")
	if recordedAt.Before(planCreatedAt) {
		return errors.New("backup evidence receipt predates the executable plan")
	}
	if recordedAt.After(planExpiresAt) {
		return errors.New("backup evidence receipt was recorded after the executable plan expired")
	}
	if recordedAt.After(now.Add(time.Minute)) {
		return errors.New("backup evidence receipt was recorded in the future — check the runner clock")
	}
	for _, resource := range r.Resources {
		if err := validateFreshEvidenceTimes(resource.CreatedAt, resource.Integrity.ValidatedAt, resource.RestoreTest.TestedAt, recordedAt, now, maxAge); err != nil {
			return fmt.Errorf("backup evidence for resource %q: %w", resource.Resource.Component, err)
		}
		if requirement.RequireRestoreTest && resource.RestoreTest.State != BackupRestoreTestPassed {
			return fmt.Errorf("backup evidence for resource %q requires a passed restore test", resource.Resource.Component)
		}
	}
	for _, material := range r.KeyMaterial {
		if err := validateFreshEvidenceTimes(material.CreatedAt, material.Integrity.ValidatedAt, material.Usability.ValidatedAt, recordedAt, now, maxAge); err != nil {
			return fmt.Errorf("backup evidence for key material %q: %w", material.Name, err)
		}
	}
	return nil
}

func validateFreshEvidenceTimes(createdValue, validatedValue, testedValue string, recordedAt, now time.Time, maxAge time.Duration) error {
	createdAt, _ := parseOperationTime(createdValue, "created_at")
	validatedAt, _ := parseOperationTime(validatedValue, "validated_at")
	if createdAt.After(now.Add(time.Minute)) || validatedAt.After(now.Add(time.Minute)) {
		return errors.New("backup or integrity validation is dated in the future")
	}
	if now.Sub(createdAt) > maxAge {
		return fmt.Errorf("backup is older than the policy max age %s", maxAge)
	}
	latest := validatedAt
	if testedValue != "" {
		testedAt, _ := parseOperationTime(testedValue, "tested_at")
		if testedAt.After(now.Add(time.Minute)) {
			return errors.New("restore test is dated in the future")
		}
		if testedAt.After(latest) {
			latest = testedAt
		}
	}
	if latest.After(recordedAt) {
		return errors.New("receipt was recorded before its validation evidence completed")
	}
	return nil
}

func (r BackupEvidenceReceipt) validUntil(plan *DeployPlan) time.Time {
	requirement := plan.MigrationBackup
	maxAge, _ := time.ParseDuration(requirement.MaximumAge)
	expiresAt, _ := parseOperationTime(plan.Operation.ExpiresAt, "plan expires_at")
	validUntil := expiresAt
	for _, resource := range r.Resources {
		createdAt, _ := parseOperationTime(resource.CreatedAt, "created_at")
		if candidate := createdAt.Add(maxAge); candidate.Before(validUntil) {
			validUntil = candidate
		}
	}
	for _, material := range r.KeyMaterial {
		createdAt, _ := parseOperationTime(material.CreatedAt, "created_at")
		if candidate := createdAt.Add(maxAge); candidate.Before(validUntil) {
			validUntil = candidate
		}
	}
	return validUntil.UTC()
}

// MigrationBackupOverride is a separate, plan-bound break-glass artifact. A
// request must explicitly carry it; operator, reason, time, and digest are
// retained in execution and journal evidence.
type MigrationBackupOverride struct {
	SchemaVersion       string                    `json:"schema_version"`
	PlanDigest          string                    `json:"plan_digest"`
	OperationDigest     string                    `json:"operation_digest"`
	Application         string                    `json:"application"`
	Environment         string                    `json:"environment"`
	Target              string                    `json:"target"`
	Resources           []MigrationBackupResource `json:"resources"`
	RequiredKeyMaterial []string                  `json:"required_key_material,omitempty"`
	Operator            string                    `json:"operator"`
	Reason              string                    `json:"reason"`
	CreatedAt           string                    `json:"created_at"`
	Source              string                    `json:"source"`
	OverrideDigest      string                    `json:"override_digest"`
}

func NewMigrationBackupOverride(plan *DeployPlan, operator, reason string, now time.Time) (MigrationBackupOverride, error) {
	if plan == nil {
		return MigrationBackupOverride{}, errors.New("migration backup override requires an executable plan")
	}
	if err := plan.Validate(); err != nil {
		return MigrationBackupOverride{}, fmt.Errorf("validate migration backup override plan: %w", err)
	}
	if plan.MigrationBackup == nil {
		return MigrationBackupOverride{}, errors.New("executable plan has no migration backup requirement")
	}
	binding := plan.Operation.Binding
	override := MigrationBackupOverride{
		SchemaVersion: MigrationBackupOverrideSchemaVersion,
		PlanDigest:    plan.PlanDigest, OperationDigest: plan.Operation.PlanDigest,
		Application: binding.Application, Environment: binding.Environment, Target: binding.Target,
		Resources:           append([]MigrationBackupResource(nil), plan.MigrationBackup.Resources...),
		RequiredKeyMaterial: append([]string(nil), plan.MigrationBackup.RequiredKeyMaterial...),
		Operator:            strings.TrimSpace(operator), Reason: strings.TrimSpace(reason),
		CreatedAt: now.UTC().Format(time.RFC3339Nano), Source: MigrationBackupOverrideSourceLocalCLI,
	}
	if err := override.Seal(); err != nil {
		return MigrationBackupOverride{}, err
	}
	return override, nil
}

func (o MigrationBackupOverride) canonicalJSON() ([]byte, error) {
	copy := o
	copy.OverrideDigest = ""
	return json.Marshal(copy)
}

func (o MigrationBackupOverride) ComputeDigest() (string, error) {
	encoded, err := o.canonicalJSON()
	if err != nil {
		return "", fmt.Errorf("encode migration backup override digest: %w", err)
	}
	return engine.HashBytes(encoded), nil
}

func (o MigrationBackupOverride) validateContent() error {
	if o.SchemaVersion != MigrationBackupOverrideSchemaVersion {
		return fmt.Errorf("unsupported migration backup override schema %q; this runner supports %q", o.SchemaVersion, MigrationBackupOverrideSchemaVersion)
	}
	for name, value := range map[string]string{
		"plan_digest": o.PlanDigest, "operation_digest": o.OperationDigest,
		"application": o.Application, "environment": o.Environment, "target": o.Target,
		"operator": o.Operator, "reason": o.Reason, "created_at": o.CreatedAt, "source": o.Source,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if len(o.Reason) > 512 {
		return errors.New("migration backup override reason must be 512 characters or fewer")
	}
	if err := validateBoundedText("operator", o.Operator, 256); err != nil {
		return err
	}
	if err := validateBoundedText("reason", o.Reason, 512); err != nil {
		return err
	}
	if err := validateBoundedText("source", o.Source, 64); err != nil {
		return err
	}
	if o.Source != MigrationBackupOverrideSourceLocalCLI {
		return fmt.Errorf("unsupported migration backup override source %q", o.Source)
	}
	if _, err := parseOperationTime(o.CreatedAt, "created_at"); err != nil {
		return err
	}
	requirement := MigrationBackupRequirement{MaximumAge: time.Second.String(), Resources: o.Resources, RequiredKeyMaterial: o.RequiredKeyMaterial}
	return requirement.validate()
}

func (o *MigrationBackupOverride) Seal() error {
	if o == nil {
		return errors.New("migration backup override is nil")
	}
	if err := o.validateContent(); err != nil {
		return err
	}
	digest, err := o.ComputeDigest()
	if err != nil {
		return err
	}
	o.OverrideDigest = digest
	return nil
}

func (o MigrationBackupOverride) Validate() error {
	if err := o.validateContent(); err != nil {
		return err
	}
	if o.OverrideDigest == "" {
		return errors.New("override_digest is required")
	}
	expected, err := o.ComputeDigest()
	if err != nil {
		return err
	}
	if o.OverrideDigest != expected {
		return fmt.Errorf("migration backup override digest mismatch: got %q, expected %q", o.OverrideDigest, expected)
	}
	return nil
}

func (o MigrationBackupOverride) ValidateForPlan(plan *DeployPlan, now time.Time) error {
	if plan == nil {
		return errors.New("migration backup override has no executable plan")
	}
	if err := o.Validate(); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("validate migration backup override plan: %w", err)
	}
	if plan.MigrationBackup == nil {
		return errors.New("executable plan has no migration backup requirement")
	}
	binding := plan.Operation.Binding
	checks := []struct{ name, got, want string }{
		{"plan digest", o.PlanDigest, plan.PlanDigest},
		{"operation digest", o.OperationDigest, plan.Operation.PlanDigest},
		{"application", o.Application, binding.Application},
		{"environment", o.Environment, binding.Environment},
		{"target", o.Target, binding.Target},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("migration backup override %s does not match the executable plan", check.name)
		}
	}
	if !reflect.DeepEqual(o.Resources, plan.MigrationBackup.Resources) || !reflect.DeepEqual(o.RequiredKeyMaterial, plan.MigrationBackup.RequiredKeyMaterial) {
		return errors.New("migration backup override protected resources do not match the executable plan")
	}
	createdAt, _ := parseOperationTime(o.CreatedAt, "created_at")
	planCreatedAt, _ := parseOperationTime(plan.Operation.CreatedAt, "plan created_at")
	planExpiresAt, _ := parseOperationTime(plan.Operation.ExpiresAt, "plan expires_at")
	if createdAt.Before(planCreatedAt) {
		return errors.New("migration backup override predates the executable plan")
	}
	if createdAt.After(planExpiresAt) {
		return errors.New("migration backup override was created after the executable plan expired")
	}
	if createdAt.After(now.UTC().Add(time.Minute)) {
		return errors.New("migration backup override was created in the future — check the runner clock")
	}
	return nil
}

func validateMigrationBackupForExecution(
	plan *DeployPlan,
	receipt *BackupEvidenceReceipt,
	override *MigrationBackupOverride,
	approval *ApprovalGrant,
	required bool,
	now time.Time,
) (*journal.MigrationBackupEvidence, error) {
	if receipt != nil && override != nil {
		return nil, errors.New("supply either backup evidence or a migration backup override, not both")
	}
	if plan.MigrationBackup == nil {
		if receipt != nil || override != nil {
			return nil, errors.New("backup evidence or override was supplied for a plan with no migration backup requirement")
		}
		return nil, nil
	}
	if receipt == nil && override == nil {
		if required {
			return nil, errors.New("fresh backup evidence is required before migration; supply a plan-bound receipt or an explicitly approved override")
		}
		return nil, nil
	}
	resources := make([]string, len(plan.MigrationBackup.Resources))
	for i, resource := range plan.MigrationBackup.Resources {
		resources[i] = resource.Component + "/" + resource.Service
	}
	if receipt != nil {
		if err := receipt.ValidateForPlan(plan, now); err != nil {
			return nil, err
		}
		return &journal.MigrationBackupEvidence{
			Mode: "receipt", ReceiptDigest: receipt.EvidenceDigest,
			ProtectedResources: resources, ValidUntil: receipt.validUntil(plan).Format(time.RFC3339Nano),
			RecordedBy: receipt.RecordedBy, RecordedAt: receipt.RecordedAt,
		}, nil
	}
	if approval == nil || approval.Approval != ApprovalStrong && approval.Approval != ApprovalBreakGlass {
		return nil, errors.New("migration backup override requires a validated strong or break-glass approval for the exact plan")
	}
	if err := approval.ValidateForPlan(plan, now); err != nil {
		return nil, fmt.Errorf("validate migration backup override approval: %w", err)
	}
	if err := override.ValidateForPlan(plan, now); err != nil {
		return nil, err
	}
	expiresAt, _ := parseOperationTime(plan.Operation.ExpiresAt, "plan expires_at")
	return &journal.MigrationBackupEvidence{
		Mode: "override", OverrideDigest: override.OverrideDigest,
		ProtectedResources: resources, ValidUntil: expiresAt.UTC().Format(time.RFC3339Nano),
		OverrideOperator: override.Operator, OverrideReason: override.Reason,
		OverrideCreatedAt: override.CreatedAt, OverrideSource: override.Source,
	}, nil
}

func saveBackupArtifact(path, prefix string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
	return os.Rename(tmpName, path)
}

func (r BackupEvidenceReceipt) Save(path string) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("validate backup evidence receipt: %w", err)
	}
	if err := saveBackupArtifact(path, ".backup-evidence-*", r); err != nil {
		return fmt.Errorf("save backup evidence receipt: %w", err)
	}
	return nil
}

func LoadBackupEvidenceReceipt(path string) (*BackupEvidenceReceipt, error) {
	var receipt BackupEvidenceReceipt
	if err := loadBackupArtifact(path, &receipt); err != nil {
		return nil, fmt.Errorf("load backup evidence receipt: %w", err)
	}
	if err := receipt.Validate(); err != nil {
		return nil, fmt.Errorf("validate backup evidence receipt: %w", err)
	}
	return &receipt, nil
}

func (o MigrationBackupOverride) Save(path string) error {
	if err := o.Validate(); err != nil {
		return fmt.Errorf("validate migration backup override: %w", err)
	}
	if err := saveBackupArtifact(path, ".migration-backup-override-*", o); err != nil {
		return fmt.Errorf("save migration backup override: %w", err)
	}
	return nil
}

func LoadMigrationBackupOverride(path string) (*MigrationBackupOverride, error) {
	var override MigrationBackupOverride
	if err := loadBackupArtifact(path, &override); err != nil {
		return nil, fmt.Errorf("load migration backup override: %w", err)
	}
	if err := override.Validate(); err != nil {
		return nil, fmt.Errorf("validate migration backup override: %w", err)
	}
	return &override, nil
}

func loadBackupArtifact(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	const maxArtifactBytes = 1 << 20
	encoded, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		return err
	}
	if len(encoded) > maxArtifactBytes {
		return fmt.Errorf("artifact exceeds %d bytes", maxArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
