package onebox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/journal"
)

const (
	ExecutableDeployPlanSchemaVersion = "onebox.run/executable-deploy-plan/v1alpha2"
	ExecutableJobPlanSchemaVersion    = "onebox.run/executable-job-plan/v1alpha1"
	OperationEventSchemaVersion       = "onebox.run/operation-event/v1alpha1"
	maxExecutableDeployPlanBytes      = 16 << 20
	maxApprovalGrantBytes             = 1 << 20
)

func readBoundedArtifact(path, kind string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kind, err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kind, err)
	}
	if len(encoded) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", kind, limit)
	}
	return encoded, nil
}

func SupportedExecutableDeployPlanSchemas() []string {
	return []string{ExecutableDeployPlanSchemaVersion, ExecutableJobPlanSchemaVersion}
}

func CurrentRunnerProvenance() buildinfo.Runner {
	return buildinfo.CurrentRunner(SupportedExecutableDeployPlanSchemas()...)
}

// DeployPlan is the local executable envelope. Operation is the canonical
// adapter-independent graph; Artifact retains the engine's exact drift and
// render binding. Neither contains decrypted secret values.
type DeployPlan struct {
	SchemaVersion   string                      `json:"schema_version"`
	Runner          buildinfo.Runner            `json:"runner"`
	Operation       OperationPlan               `json:"operation"`
	Artifact        engine.Artifact             `json:"artifact"`
	Diff            string                      `json:"diff,omitempty"`
	NoOp            bool                        `json:"no_op"`
	MigrationBackup *MigrationBackupRequirement `json:"migration_backup,omitempty"`
	PlanDigest      string                      `json:"plan_digest"`
}

func (p DeployPlan) ExecutableOperation() OperationPlan { return p.Operation }
func (p DeployPlan) ExecutablePlanDigest() string       { return p.PlanDigest }
func (p DeployPlan) ExecutableMigrationBackup() *MigrationBackupRequirement {
	return p.MigrationBackup
}
func (*DeployPlan) executablePlan()           {}
func (p *DeployPlan) executablePlanNil() bool { return p == nil }

// ExecutablePlan is the common approval/evidence authority shared by deploy
// and one-shot job plans. Its unexported methods keep the authority closed to
// Onebox plan types, so an adapter cannot substitute its own validation.
type ExecutablePlan interface {
	executablePlan()
	executablePlanNil() bool
	Validate() error
	ExecutableOperation() OperationPlan
	ExecutablePlanDigest() string
	ExecutableMigrationBackup() *MigrationBackupRequirement
}

type executablePlanView struct {
	operation       OperationPlan
	digest          string
	migrationBackup *MigrationBackupRequirement
}

func inspectExecutablePlan(plan ExecutablePlan) (executablePlanView, error) {
	if plan == nil || plan.executablePlanNil() {
		return executablePlanView{}, errors.New("executable plan is nil")
	}
	if err := plan.Validate(); err != nil {
		return executablePlanView{}, err
	}
	return executablePlanView{
		operation:       plan.ExecutableOperation(),
		digest:          plan.ExecutablePlanDigest(),
		migrationBackup: plan.ExecutableMigrationBackup(),
	}, nil
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
		return unsupportedDeployPlanSchemaError(p.SchemaVersion)
	}
	if err := validateRunnerProvenance(p.Runner, p.SchemaVersion); err != nil {
		return fmt.Errorf("runner: %w", err)
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
	if p.MigrationBackup != nil {
		if !hasMigrationStep(p.Operation.Steps) {
			return errors.New("migration backup requirement is present without a migration step")
		}
		if err := p.MigrationBackup.validate(); err != nil {
			return fmt.Errorf("migration backup requirement: %w", err)
		}
	}
	return nil
}

func (p DeployPlan) ComputeDigest() (string, error) {
	planCopy := p
	planCopy.PlanDigest = ""
	encoded, err := json.Marshal(planCopy)
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
	if err := writeDurableArtifact(path, ".deploy-plan-*", encoded); err != nil {
		return fmt.Errorf("publish deploy plan: %w", err)
	}
	return nil
}

func LoadDeployPlan(path string) (*DeployPlan, error) {
	encoded, err := readBoundedArtifact(path, "executable deploy plan", maxExecutableDeployPlanBytes)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return nil, fmt.Errorf("decode executable deploy plan: %w", err)
	}
	if envelope.SchemaVersion != ExecutableDeployPlanSchemaVersion {
		return nil, unsupportedDeployPlanSchemaError(envelope.SchemaVersion)
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
	EvidenceID string           `json:"evidence_id,omitempty"`
	Sequence   int              `json:"sequence"`
	Time       string           `json:"time"`
	Kind       OperationKind    `json:"kind"`
	Phase      string           `json:"phase"`
	Status     OperationStatus  `json:"status"`
	Message    string           `json:"message,omitempty"`
	Runner     buildinfo.Runner `json:"runner"`
}

type EventSink func(OperationEvent)

// ExecutionBinding is the local authority resolved before a destructive
// confirmation. Execute rechecks it so a config edit cannot retarget the
// confirmed operation.
type ExecutionBinding struct {
	Application   string `json:"application"`
	Environment   string `json:"environment"`
	Server        string `json:"server"`
	ConfigDigest  string `json:"config_digest"`
	ComposeDigest string `json:"compose_digest"`
}

// ExecuteRequest is the sole mutation entry point used by local adapters.
// Approval is validated again at this boundary; adapter-side checks are never
// treated as execution authority.
type ExecuteRequest struct {
	Kind                    OperationKind
	Plan                    *DeployPlan
	JobPlan                 *JobPlan
	Approval                *ApprovalGrant
	BackupReport            *BackupReport
	MigrationBackupOverride *MigrationBackupOverride
	BreakLock               bool
	AllowDestructiveMounts  bool
	// Service is the backup operations' one argument. It is an input to a
	// mutation rather than a plan, because a backup stages nothing into a
	// release and has nothing to roll back.
	Service string
	// RecoveryTarget is the RFC 3339 point in time a recovery aims at. Empty
	// means the newest recoverable point.
	RecoveryTarget     string
	BreakMigrationGate bool
	NoRollback         bool
	Redeploy           bool
	RemoveVolumes      bool
	RemoveProxy        bool
	ExpectedBinding    *ExecutionBinding
	Events             EventSink
}

// Validate rejects ambiguous plans, mismatched operation kinds, and safety or
// authority fields that the selected operation does not consume.
func (request ExecuteRequest) Validate() error {
	if request.Plan != nil && request.JobPlan != nil {
		return errors.New("execution request cannot contain both deploy and job plans")
	}
	switch request.Kind {
	case KindDeploy:
		if request.JobPlan != nil {
			return errors.New("deploy execution cannot contain a job plan")
		}
	case KindJobRun:
		if request.Plan != nil {
			return errors.New("job execution cannot contain a deploy plan")
		}
	default:
		if request.Plan != nil || request.JobPlan != nil {
			return errors.New("only deploy and job execution requests may contain plans")
		}
	}
	if request.AllowDestructiveMounts && request.Kind != KindServiceApply {
		return errors.New("allow_destructive_mounts is valid only for service apply")
	}
	if request.BreakMigrationGate && request.Kind != KindAbort {
		return errors.New("break_migration_gate is valid only for abort")
	}
	if (request.NoRollback || request.Redeploy) && request.Kind != KindDeploy {
		return errors.New("no_rollback and redeploy are valid only for deploy")
	}
	if (request.RemoveVolumes || request.RemoveProxy) && request.Kind != KindDestroy {
		return errors.New("remove_volumes and remove_proxy are valid only for destroy")
	}
	if (request.Approval != nil || request.BackupReport != nil || request.MigrationBackupOverride != nil) && request.Kind != KindDeploy && request.Kind != KindJobRun {
		return errors.New("approval and migration backup authorization are valid only for deploy and job run")
	}
	return nil
}

type OperationResult struct {
	ID        string          `json:"id"`
	Kind      OperationKind   `json:"kind"`
	Status    OperationStatus `json:"status"`
	ReleaseID string          `json:"release_id,omitempty"`
	// EvidenceID is the engine's release/journal correlation key, not a claim
	// that a journal append completed. An early failure may leave no evidence.
	EvidenceID                    string                     `json:"evidence_id,omitempty"`
	StartedAt                     string                     `json:"started_at"`
	FinishedAt                    string                     `json:"finished_at"`
	NoOp                          bool                       `json:"no_op,omitempty"`
	ApprovalDigest                string                     `json:"approval_digest,omitempty"`
	BackupReportDigest            string                     `json:"backup_report_digest,omitempty"`
	MigrationBackupOverrideDigest string                     `json:"migration_backup_override_digest,omitempty"`
	Runner                        buildinfo.Runner           `json:"runner"`
	JobResult                     *journal.JobResultEvidence `json:"job_result,omitempty"`
}

func validateRunnerProvenance(runner buildinfo.Runner, planSchema string) error {
	if strings.TrimSpace(runner.Version) == "" {
		return errors.New("version is required")
	}
	for _, supported := range runner.SupportedExecutablePlanSchemas {
		if supported == planSchema {
			return nil
		}
	}
	return fmt.Errorf("runner does not declare support for executable plan schema %q", planSchema)
}

func unsupportedDeployPlanSchemaError(schema string) error {
	if strings.TrimSpace(schema) == "" {
		return fmt.Errorf(
			"executable deploy plan has no schema_version; this ob %s runner only executes %q — upgrade `ob` if needed, then regenerate the plan with the current `ob plan`",
			buildinfo.Read().Version,
			ExecutableDeployPlanSchemaVersion,
		)
	}
	return fmt.Errorf(
		"unsupported executable deploy plan schema %q; this ob %s runner supports %q — upgrade `ob` if needed, then regenerate the plan with the current `ob plan`",
		schema,
		buildinfo.Read().Version,
		ExecutableDeployPlanSchemaVersion,
	)
}

func parseOperationTime(value, name string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s is not RFC3339: %w", name, err)
	}
	return parsed, nil
}
