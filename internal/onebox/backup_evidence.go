package onebox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/journal"
)

const (
	BackupReportSchemaVersion             = "onebox.run/backup-report/v1alpha1"
	backupReceiptSchemaVersion            = "onebox.run/internal-backup-receipt/v1alpha1"
	MigrationBackupOverrideSchemaVersion  = "onebox.run/migration-backup-override/v1alpha1"
	BackupRestoreTestPassed               = "passed"
	BackupRestoreTestNotTested            = "not_tested"
	MigrationBackupOverrideSourceLocalCLI = "local_cli"
)

// ErrBackupReportNotRequired reports that a plan declares no migration backup
// requirement. It is a sentinel rather than an anonymous error because a caller
// cannot know in advance whether a plan will need a report: `ob plan
// --backup-report-out` is asked for defensively, and "this deploy touches no
// data" must be distinguishable from "the template could not be produced".
var ErrBackupReportNotRequired = errors.New("executable plan has no migration backup requirement")

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// MigrationBackupRequirement is plan-bound policy, not operator-supplied
// protection input. Its presence means every pending migration step requires
// either a matching report or an explicit audited override.
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
	if !policy.Migrations.RequireBackup || !hasMigrationStep(steps) {
		return nil, nil
	}
	resources := make([]MigrationBackupResource, 0, len(cfg.Workloads)+len(cfg.Services))
	for name, workload := range cfg.Workloads {
		// Declared: anything but ephemeral counts, which keeps `external` a
		// resource the operator is asked to cover. Undeclared: a managed volume
		// is durable, which is the inference doctor and this gate now share.
		mode := "durable"
		if workload.Persistence != nil {
			mode = workload.Persistence.Mode
			if mode == "ephemeral" {
				continue
			}
		} else if !workload.HoldsDurableData() {
			continue
		}
		volumes := durableVolumeNames(workload)
		sort.Strings(volumes)
		resources = append(resources, MigrationBackupResource{
			Component: name, Service: name, Type: workload.Role,
			Persistence: mode, Volumes: volumes,
		})
	}
	// A managed service is the usual place the data actually lives, and onebox
	// runs it and names its volume. Counting only workloads left the standard
	// shape — replicated stateless application plus a managed database — unable
	// to satisfy the gate at all: the requirement wanted a durable workload,
	// stateful_replicas refuses a durable workload with replicas, and the
	// service counted for nothing.
	for _, name := range cfg.ServiceNames() {
		service := cfg.Services[name]
		mode := "durable"
		if service.Persistence != nil {
			mode = service.Persistence.Mode
		}
		if mode == "ephemeral" {
			continue
		}
		volumes := append([]string(nil), service.Volumes...)
		sort.Strings(volumes)
		resources = append(resources, MigrationBackupResource{
			Component: name, Service: name, Type: "service",
			Persistence: mode, Volumes: volumes,
		})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Component < resources[j].Component })
	if len(resources) == 0 {
		return nil, errors.New("migration backup policy is enabled but nothing holds data to back up: no workload has a managed volume or declares durable or external persistence, and no supporting service is declared")
	}
	keyMaterial := append([]string(nil), policy.Migrations.BackupKeyMaterial...)
	sort.Strings(keyMaterial)
	requirement := &MigrationBackupRequirement{
		MaximumAge:          policy.Migrations.BackupMaximumAge,
		RequireRestoreTest:  policy.Migrations.RequireRestoreTest,
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
	maxAge, err := app.PositiveDuration(r.MaximumAge)
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

// BackupReport is an operator/tool report about already-created backup
// artifacts. It is execution input, not proof that Onebox created or
// independently verified a backup. The report never contains backup bytes,
// key material, secret values, commands, or provider credentials.
type BackupReport struct {
	SchemaVersion   string                               `json:"schema_version"`
	PlanDigest      string                               `json:"plan_digest"`
	OperationDigest string                               `json:"operation_digest"`
	Application     string                               `json:"application"`
	Environment     string                               `json:"environment"`
	Server          string                               `json:"server"`
	ReportedBy      string                               `json:"reported_by"`
	ReportedAt      string                               `json:"reported_at"`
	Resources       []MigrationBackupResourceEvidence    `json:"resources"`
	KeyMaterial     []MigrationBackupKeyMaterialEvidence `json:"key_material,omitempty"`
}

func NewBackupReport(plan ExecutablePlan, reportedBy string, reportedAt time.Time, resources []MigrationBackupResourceEvidence, keyMaterial []MigrationBackupKeyMaterialEvidence) (BackupReport, error) {
	view, err := inspectExecutablePlan(plan)
	if err != nil {
		return BackupReport{}, fmt.Errorf("validate backup report plan: %w", err)
	}
	if view.migrationBackup == nil {
		return BackupReport{}, ErrBackupReportNotRequired
	}
	resources = append([]MigrationBackupResourceEvidence(nil), resources...)
	keyMaterial = append([]MigrationBackupKeyMaterialEvidence(nil), keyMaterial...)
	sort.Slice(resources, func(i, j int) bool { return resources[i].Resource.Component < resources[j].Resource.Component })
	sort.Slice(keyMaterial, func(i, j int) bool { return keyMaterial[i].Name < keyMaterial[j].Name })
	binding := view.operation.Binding
	report := BackupReport{
		SchemaVersion: BackupReportSchemaVersion,
		PlanDigest:    view.digest, OperationDigest: view.operation.PlanDigest,
		Application: binding.Application, Environment: binding.Environment, Server: binding.Server,
		ReportedBy: strings.TrimSpace(reportedBy), ReportedAt: reportedAt.UTC().Format(time.RFC3339Nano),
		Resources: resources, KeyMaterial: keyMaterial,
	}
	if err := report.Validate(); err != nil {
		return BackupReport{}, err
	}
	return report, nil
}

// NewBackupReportTemplate projects the exact requirement from a sealed plan.
// Placeholder values intentionally make the template invalid until the
// reporting tool replaces them with its own observations.
func NewBackupReportTemplate(plan ExecutablePlan) (BackupReport, error) {
	view, err := inspectExecutablePlan(plan)
	if err != nil {
		return BackupReport{}, fmt.Errorf("validate backup report plan: %w", err)
	}
	if view.migrationBackup == nil {
		return BackupReport{}, ErrBackupReportNotRequired
	}
	binding := view.operation.Binding
	report := BackupReport{
		SchemaVersion: BackupReportSchemaVersion,
		PlanDigest:    view.digest, OperationDigest: view.operation.PlanDigest,
		Application: binding.Application, Environment: binding.Environment, Server: binding.Server,
		ReportedBy: "REPLACE-with-reporting-operator-or-tool", ReportedAt: "REPLACE-with-RFC3339-time",
	}
	for _, resource := range view.migrationBackup.Resources {
		restore := BackupRestoreTestEvidence{State: BackupRestoreTestNotTested}
		if view.migrationBackup.RequireRestoreTest {
			restore = BackupRestoreTestEvidence{
				State: BackupRestoreTestPassed, Method: "REPLACE-how-restore-was-tested",
				TestedAt: "REPLACE-with-RFC3339-time", ValidationDigest: "sha256:REPLACE",
			}
		}
		report.Resources = append(report.Resources, MigrationBackupResourceEvidence{
			Resource: resource, BackupID: "REPLACE-with-backup-id", CreatedAt: "REPLACE-with-RFC3339-time",
			Integrity:   BackupIntegrityEvidence{ArtifactDigest: "sha256:REPLACE", Method: "sha256", ValidatedAt: "REPLACE-with-RFC3339-time"},
			RestoreTest: restore,
		})
	}
	for _, name := range view.migrationBackup.RequiredKeyMaterial {
		report.KeyMaterial = append(report.KeyMaterial, MigrationBackupKeyMaterialEvidence{
			Name: name, BackupID: "REPLACE-with-backup-id", CreatedAt: "REPLACE-with-RFC3339-time",
			Integrity: BackupIntegrityEvidence{ArtifactDigest: "sha256:REPLACE", Method: "sha256", ValidatedAt: "REPLACE-with-RFC3339-time"},
			Usability: BackupKeyMaterialUsabilityEvidence{Method: "REPLACE-how-key-usability-was-checked", ValidatedAt: "REPLACE-with-RFC3339-time", ValidationDigest: "sha256:REPLACE"},
		})
	}
	return report, nil
}

func (r BackupReport) SaveTemplate(path string) error {
	if r.SchemaVersion != BackupReportSchemaVersion || !sha256Digest.MatchString(r.PlanDigest) {
		return errors.New("backup report template must be created from a valid executable plan")
	}
	if err := saveBackupArtifact(path, ".backup-report-*", r); err != nil {
		return fmt.Errorf("save backup report template: %w", err)
	}
	return nil
}

func (r BackupReport) canonicalJSON() ([]byte, error) {
	return json.Marshal(r)
}

func (r BackupReport) ComputeDigest() (string, error) {
	encoded, err := r.canonicalJSON()
	if err != nil {
		return "", fmt.Errorf("encode backup report digest: %w", err)
	}
	return engine.HashBytes(encoded), nil
}

func (r BackupReport) Validate() error {
	if r.SchemaVersion != BackupReportSchemaVersion {
		return fmt.Errorf("unsupported backup report schema %q; this runner supports %q", r.SchemaVersion, BackupReportSchemaVersion)
	}
	for name, value := range map[string]string{
		"plan_digest": r.PlanDigest, "operation_digest": r.OperationDigest,
		"application": r.Application, "environment": r.Environment, "target": r.Server,
		"reported_by": r.ReportedBy, "reported_at": r.ReportedAt,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if _, err := parseOperationTime(r.ReportedAt, "reported_at"); err != nil {
		return err
	}
	if err := validateBoundedText("reported_by", r.ReportedBy, 256); err != nil {
		return err
	}
	if len(r.Resources) == 0 {
		return errors.New("backup report resources must not be empty")
	}
	previous := ""
	for i, resource := range r.Resources {
		if err := resource.validate(); err != nil {
			return fmt.Errorf("resources[%d]: %w", i, err)
		}
		if previous != "" && resource.Resource.Component <= previous {
			return errors.New("backup report resources must be unique and sorted by component")
		}
		previous = resource.Resource.Component
	}
	previous = ""
	for i, material := range r.KeyMaterial {
		if err := material.validate(); err != nil {
			return fmt.Errorf("key_material[%d]: %w", i, err)
		}
		if previous != "" && material.Name <= previous {
			return errors.New("backup report key material must be unique and sorted by name")
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

// keyMaterialSatisfies normalizes nil against empty. Ordering remains bound to
// the requirement because a report is an exact projection of its plan.
//
// reflect.DeepEqual treats an empty slice and a nil slice as different. A
// policy requiring no key material leaves the requirement nil while a receipt
// with none carries a zero-length slice, so the default configuration refused
// every earlier locally wrapped report and the gate could not be
// satisfied at all.
func keyMaterialSatisfies(supplied []MigrationBackupKeyMaterialEvidence, required []string) bool {
	names := make([]string, 0, len(supplied))
	for i := range supplied {
		names = append(names, supplied[i].Name)
	}
	return slices.Equal(names, required)
}

func (r BackupReport) ValidateForPlan(plan ExecutablePlan, now time.Time) error {
	if err := r.Validate(); err != nil {
		return err
	}
	view, err := inspectExecutablePlan(plan)
	if err != nil {
		return fmt.Errorf("validate backup report plan: %w", err)
	}
	requirement := view.migrationBackup
	if requirement == nil {
		return ErrBackupReportNotRequired
	}
	binding := view.operation.Binding
	checks := []struct{ name, got, want string }{
		{"plan digest", r.PlanDigest, view.digest},
		{"operation digest", r.OperationDigest, view.operation.PlanDigest},
		{"application", r.Application, binding.Application},
		{"environment", r.Environment, binding.Environment},
		{"target", r.Server, binding.Server},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("backup report %s does not match the executable plan", check.name)
		}
	}
	resourceIdentities := make([]MigrationBackupResource, len(r.Resources))
	for i := range r.Resources {
		resourceIdentities[i] = r.Resources[i].Resource
	}
	if !reflect.DeepEqual(resourceIdentities, requirement.Resources) {
		return errors.New("backup report resources do not match the executable plan")
	}
	if !keyMaterialSatisfies(r.KeyMaterial, requirement.RequiredKeyMaterial) {
		return errors.New("backup report key material does not match the executable plan")
	}
	maxAge, _ := app.PositiveDuration(requirement.MaximumAge)
	now = now.UTC()
	reportedAt, _ := parseOperationTime(r.ReportedAt, "reported_at")
	planCreatedAt, _ := parseOperationTime(view.operation.CreatedAt, "plan created_at")
	planExpiresAt, _ := parseOperationTime(view.operation.ExpiresAt, "plan expires_at")
	if reportedAt.Before(planCreatedAt) {
		return errors.New("backup report predates the executable plan")
	}
	if reportedAt.After(planExpiresAt) {
		return errors.New("backup report was recorded after the executable plan expired")
	}
	if reportedAt.After(now.Add(time.Minute)) {
		return errors.New("backup report was recorded in the future — check the runner clock")
	}
	for _, resource := range r.Resources {
		if err := validateFreshEvidenceTimes(resource.CreatedAt, resource.Integrity.ValidatedAt, resource.RestoreTest.TestedAt, reportedAt, now, maxAge); err != nil {
			return fmt.Errorf("backup report for resource %q: %w", resource.Resource.Component, err)
		}
		if requirement.RequireRestoreTest && resource.RestoreTest.State != BackupRestoreTestPassed {
			return fmt.Errorf("backup report for resource %q requires a passed restore test", resource.Resource.Component)
		}
	}
	for _, material := range r.KeyMaterial {
		if err := validateFreshEvidenceTimes(material.CreatedAt, material.Integrity.ValidatedAt, material.Usability.ValidatedAt, reportedAt, now, maxAge); err != nil {
			return fmt.Errorf("backup report for key material %q: %w", material.Name, err)
		}
	}
	return nil
}

func validateFreshEvidenceTimes(createdValue, validatedValue, testedValue string, reportedAt, now time.Time, maxAge time.Duration) error {
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
	if latest.After(reportedAt) {
		return errors.New("report was recorded before its validation checks completed")
	}
	return nil
}

func (r BackupReport) validUntil(plan ExecutablePlan) time.Time {
	view, _ := inspectExecutablePlan(plan)
	requirement := view.migrationBackup
	maxAge, _ := app.PositiveDuration(requirement.MaximumAge)
	expiresAt, _ := parseOperationTime(view.operation.ExpiresAt, "plan expires_at")
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

// backupReceipt is created only at the execution boundary. It binds the exact
// report accepted for one attempt without creating another public artifact or
// asking the report author to manufacture a checksum field.
type backupReceipt struct {
	SchemaVersion   string `json:"schema_version"`
	PlanDigest      string `json:"plan_digest"`
	OperationDigest string `json:"operation_digest"`
	ReportDigest    string `json:"report_digest"`
	RecordedAt      string `json:"recorded_at"`
	ValidUntil      string `json:"valid_until"`
	ReceiptDigest   string `json:"receipt_digest"`
}

func newBackupReceipt(plan ExecutablePlan, report BackupReport, now time.Time) (backupReceipt, error) {
	if err := report.ValidateForPlan(plan, now); err != nil {
		return backupReceipt{}, err
	}
	view, err := inspectExecutablePlan(plan)
	if err != nil {
		return backupReceipt{}, err
	}
	reportDigest, err := report.ComputeDigest()
	if err != nil {
		return backupReceipt{}, err
	}
	receipt := backupReceipt{
		SchemaVersion: backupReceiptSchemaVersion, PlanDigest: view.digest,
		OperationDigest: view.operation.PlanDigest, ReportDigest: reportDigest,
		RecordedAt: now.UTC().Format(time.RFC3339Nano), ValidUntil: report.validUntil(plan).Format(time.RFC3339Nano),
	}
	encoded, err := receipt.canonicalJSON()
	if err != nil {
		return backupReceipt{}, err
	}
	receipt.ReceiptDigest = engine.HashBytes(encoded)
	return receipt, nil
}

func (r backupReceipt) canonicalJSON() ([]byte, error) {
	copy := r
	copy.ReceiptDigest = ""
	return json.Marshal(copy)
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
	Server              string                    `json:"server"`
	Resources           []MigrationBackupResource `json:"resources"`
	RequiredKeyMaterial []string                  `json:"required_key_material,omitempty"`
	Operator            string                    `json:"operator"`
	Reason              string                    `json:"reason"`
	CreatedAt           string                    `json:"created_at"`
	Source              string                    `json:"source"`
	OverrideDigest      string                    `json:"override_digest"`
}

func NewMigrationBackupOverride(plan ExecutablePlan, operator, reason string, now time.Time) (MigrationBackupOverride, error) {
	view, err := inspectExecutablePlan(plan)
	if err != nil {
		return MigrationBackupOverride{}, fmt.Errorf("validate migration backup override plan: %w", err)
	}
	if view.migrationBackup == nil {
		return MigrationBackupOverride{}, ErrBackupReportNotRequired
	}
	binding := view.operation.Binding
	override := MigrationBackupOverride{
		SchemaVersion: MigrationBackupOverrideSchemaVersion,
		PlanDigest:    view.digest, OperationDigest: view.operation.PlanDigest,
		Application: binding.Application, Environment: binding.Environment, Server: binding.Server,
		Resources:           append([]MigrationBackupResource(nil), view.migrationBackup.Resources...),
		RequiredKeyMaterial: append([]string(nil), view.migrationBackup.RequiredKeyMaterial...),
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
		"application": o.Application, "environment": o.Environment, "server": o.Server,
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

func (o MigrationBackupOverride) ValidateForPlan(plan ExecutablePlan, now time.Time) error {
	if err := o.Validate(); err != nil {
		return err
	}
	view, err := inspectExecutablePlan(plan)
	if err != nil {
		return fmt.Errorf("validate migration backup override plan: %w", err)
	}
	if view.migrationBackup == nil {
		return ErrBackupReportNotRequired
	}
	binding := view.operation.Binding
	checks := []struct{ name, got, want string }{
		{"plan digest", o.PlanDigest, view.digest},
		{"operation digest", o.OperationDigest, view.operation.PlanDigest},
		{"application", o.Application, binding.Application},
		{"environment", o.Environment, binding.Environment},
		{"server", o.Server, binding.Server},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("migration backup override %s does not match the executable plan", check.name)
		}
	}
	if !reflect.DeepEqual(o.Resources, view.migrationBackup.Resources) || !reflect.DeepEqual(o.RequiredKeyMaterial, view.migrationBackup.RequiredKeyMaterial) {
		return errors.New("migration backup override protected resources do not match the executable plan")
	}
	createdAt, _ := parseOperationTime(o.CreatedAt, "created_at")
	planCreatedAt, _ := parseOperationTime(view.operation.CreatedAt, "plan created_at")
	planExpiresAt, _ := parseOperationTime(view.operation.ExpiresAt, "plan expires_at")
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
	plan ExecutablePlan,
	report *BackupReport,
	override *MigrationBackupOverride,
	approval *ApprovalGrant,
	required bool,
	now time.Time,
) (*journal.MigrationBackupEvidence, error) {
	view, err := inspectExecutablePlan(plan)
	if err != nil {
		return nil, fmt.Errorf("validate migration backup plan: %w", err)
	}
	if report != nil && override != nil {
		return nil, errors.New("supply either a backup report or a migration backup override, not both")
	}
	if view.migrationBackup == nil {
		if report != nil || override != nil {
			return nil, errors.New("a backup report or override was supplied for a plan with no migration backup requirement")
		}
		return nil, nil
	}
	if report == nil && override == nil {
		if required {
			return nil, errors.New("a fresh backup report is required before migration; supply --backup-report or an explicitly approved override")
		}
		return nil, nil
	}
	resources := make([]string, len(view.migrationBackup.Resources))
	for i, resource := range view.migrationBackup.Resources {
		resources[i] = resource.Component + "/" + resource.Service
	}
	if report != nil {
		reportDigest, err := report.ComputeDigest()
		if err != nil {
			return nil, err
		}
		if approval == nil {
			return nil, errors.New("backup report requires a local confirmation bound to the same report")
		}
		if approval.BackupReportDigest != reportDigest {
			return nil, errors.New("backup report does not match the report bound into the local confirmation; confirm the current plan and report again")
		}
		receipt, err := newBackupReceipt(plan, *report, now)
		if err != nil {
			return nil, err
		}
		return &journal.MigrationBackupEvidence{
			Mode: "receipt", ReceiptDigest: receipt.ReceiptDigest,
			ProtectedResources: resources, ValidUntil: receipt.ValidUntil,
			RecordedBy: report.ReportedBy, RecordedAt: receipt.RecordedAt,
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
	expiresAt, _ := parseOperationTime(view.operation.ExpiresAt, "plan expires_at")
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
	return writeDurableArtifact(path, prefix, encoded)
}

func LoadBackupReport(path string) (*BackupReport, error) {
	var report BackupReport
	if err := loadBackupArtifact(path, &report); err != nil {
		return nil, fmt.Errorf("load backup report: %w", err)
	}
	if err := report.Validate(); err != nil {
		return nil, fmt.Errorf("validate backup report: %w", err)
	}
	return &report, nil
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
