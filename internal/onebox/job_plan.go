package onebox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/release"
	"gopkg.in/yaml.v3"
)

const maxExecutableJobPlanBytes = 1 << 20

var pinnedJobImage = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

type JobArtifact struct {
	Application    string          `json:"application"`
	Environment    string          `json:"environment"`
	Server         string          `json:"server"`
	CurrentRelease string          `json:"current_release"`
	RuntimeDigest  string          `json:"runtime_digest"`
	Job            string          `json:"job"`
	Image          string          `json:"image"`
	DataEffect     DataEffectClass `json:"data_effect"`
}

// JobPlan is a sealed, current-release-bound one-shot operation. It contains
// no command body or secret value; execution always uses the referenced
// release's immutable runtime on the target.
type JobPlan struct {
	SchemaVersion   string                      `json:"schema_version"`
	Runner          buildinfo.Runner            `json:"runner"`
	Operation       OperationPlan               `json:"operation"`
	Artifact        JobArtifact                 `json:"artifact"`
	MigrationBackup *MigrationBackupRequirement `json:"migration_backup,omitempty"`
	PlanDigest      string                      `json:"plan_digest"`
}

func (p JobPlan) ExecutableOperation() OperationPlan { return p.Operation }
func (p JobPlan) ExecutablePlanDigest() string       { return p.PlanDigest }
func (p JobPlan) ExecutableMigrationBackup() *MigrationBackupRequirement {
	return p.MigrationBackup
}

func jobArtifactDigest(artifact JobArtifact) (string, error) {
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return "", fmt.Errorf("encode job artifact: %w", err)
	}
	return engine.HashBytes(encoded), nil
}

func (p JobPlan) validateContent() error {
	if p.SchemaVersion != ExecutableJobPlanSchemaVersion {
		return fmt.Errorf("schema_version must be %q", ExecutableJobPlanSchemaVersion)
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
	if p.Operation.Kind != KindJobRun {
		return fmt.Errorf("operation kind must be %q", KindJobRun)
	}
	if p.Artifact.Application != p.Operation.Binding.Application ||
		p.Artifact.Environment != p.Operation.Binding.Environment ||
		p.Artifact.Server != p.Operation.Binding.Server {
		return errors.New("job artifact authority does not match operation binding")
	}
	if p.Artifact.CurrentRelease == "" || p.Artifact.CurrentRelease != p.Operation.ReleaseID {
		return errors.New("job artifact current release does not match operation release_id")
	}
	if p.Artifact.RuntimeDigest == "" || p.Artifact.RuntimeDigest != p.Operation.Binding.ComposeDigest {
		return errors.New("job artifact runtime digest does not match operation binding")
	}
	if p.Artifact.Job == "" || !pinnedJobImage.MatchString(p.Artifact.Image) {
		return errors.New("job artifact requires a job identifier and digest-pinned image")
	}
	if len(p.Operation.Steps) != 1 {
		return errors.New("job operation must contain exactly one step")
	}
	step := p.Operation.Steps[0]
	if step.Kind != StepJob || step.Component != p.Artifact.Job || step.Service != p.Artifact.Job || step.DataEffect != p.Artifact.DataEffect {
		return errors.New("job operation step does not match its artifact")
	}
	digest, err := jobArtifactDigest(p.Artifact)
	if err != nil {
		return err
	}
	if digest != p.Operation.Binding.StateDigest {
		return errors.New("job artifact digest does not match operation state binding")
	}
	if p.MigrationBackup != nil {
		if step.DataEffect != DataEffectMigration {
			return errors.New("migration backup requirement is present for a non-migration job")
		}
		if err := p.MigrationBackup.validate(); err != nil {
			return fmt.Errorf("migration backup requirement: %w", err)
		}
	}
	return nil
}

func (p JobPlan) ComputeDigest() (string, error) {
	copy := p
	copy.PlanDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("encode executable job plan digest: %w", err)
	}
	return engine.HashBytes(encoded), nil
}

func (p *JobPlan) Seal() error {
	if p == nil {
		return errors.New("executable job plan is nil")
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

func (p JobPlan) Validate() error {
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
		return fmt.Errorf("executable job plan digest mismatch: got %q, expected %q", p.PlanDigest, expected)
	}
	return nil
}

func (p JobPlan) Save(path string) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("validate executable job plan: %w", err)
	}
	encoded, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".job-plan-*")
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

func LoadJobPlan(path string) (*JobPlan, error) {
	encoded, err := readBoundedArtifact(path, "executable job plan", maxExecutableJobPlanBytes)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var plan JobPlan
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode executable job plan: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode executable job plan: multiple JSON values")
		}
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("validate executable job plan: %w", err)
	}
	return &plan, nil
}

// LoadExecutablePlan dispatches by the closed schema identity so approval and
// evidence adapters accept deploy and job plans without guessing from fields.
func LoadExecutablePlan(path string) (ExecutablePlan, error) {
	encoded, err := readBoundedArtifact(path, "executable plan", maxExecutableDeployPlanBytes)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return nil, fmt.Errorf("decode executable plan identity: %w", err)
	}
	switch envelope.SchemaVersion {
	case ExecutableDeployPlanSchemaVersion:
		return LoadDeployPlan(path)
	case ExecutableJobPlanSchemaVersion:
		return LoadJobPlan(path)
	default:
		return nil, fmt.Errorf("unsupported executable plan schema %q", envelope.SchemaVersion)
	}
}

type PlanJobRequest struct {
	Job string
}

func (s *Service) PlanJob(ctx context.Context, request PlanJobRequest) (JobPlan, error) {
	jobID := strings.TrimSpace(request.Job)
	if jobID == "" {
		return JobPlan{}, errors.New("job identifier is required")
	}
	now := s.now().UTC()
	lp, err := s.loadProject(ctx, false)
	if err != nil {
		return JobPlan{}, fmt.Errorf("load project: %w", err)
	}
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return JobPlan{}, err
	}
	environmentConfig, err := lp.resolved.Environment(s.environment)
	if err != nil {
		return JobPlan{}, err
	}
	if err := enforceRunnerPolicy(environmentConfig.Policy, s.runner, ExecutableJobPlanSchemaVersion); err != nil {
		return JobPlan{}, err
	}
	if err := lp.resolved.Spec.RunPreflight(filepath.Dir(lp.configPath)); err != nil {
		return JobPlan{}, err
	}
	job, ok := lp.resolved.Workloads[jobID]
	if !ok || !job.IsJob() {
		return JobPlan{}, fmt.Errorf("unknown job %q", jobID)
	}
	if job.When != "manual" {
		return JobPlan{}, fmt.Errorf("job %q is %s; one-shot invocation is reserved for when: manual jobs", jobID, job.When)
	}
	e, cleanup, target, err := s.engine(ctx, lp, s.environment)
	if err != nil {
		return JobPlan{}, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()
	hostState, err := e.Refresh(ctx)
	if err != nil {
		return JobPlan{}, fmt.Errorf("refresh: %w", err)
	}
	if hostState.CurrentRelease == "" {
		return JobPlan{}, errors.New("job requires a current serving release; run `ob deploy`")
	}
	runtime, runtimeDigest, image, err := readJobRuntime(ctx, e, hostState.CurrentRelease, jobID)
	_ = runtime
	if err != nil {
		return JobPlan{}, err
	}
	if !pinnedJobImage.MatchString(image) {
		return JobPlan{}, fmt.Errorf("job %q image is not digest-pinned in current release %s; run `ob deploy` to create an immutable runtime", jobID, hostState.CurrentRelease)
	}
	effect := DataEffectClass(job.DataEffect)
	step := OperationStep{ID: "job:" + jobID, Kind: StepJob, Component: jobID, Service: jobID, DataEffect: effect, Mutation: true}
	if effect == DataEffectMigration {
		step.ResultPolicy = JobResultProviderOrStrongUnknown
	}
	steps := []OperationStep{step}
	migrationBackup, err := migrationBackupRequirement(lp.resolved, environmentConfig.Policy, steps)
	if err != nil {
		return JobPlan{}, err
	}
	risk, reversibility, approval := RiskModerate, ReversibilityReversible, ApprovalNone
	if environmentConfig.Policy.RequireApproval {
		approval = ApprovalOneTime
	}
	if effect == DataEffectMigration || effect == DataEffectUnknown {
		risk, reversibility, approval = RiskHigh, ReversibilityConditional, ApprovalStrong
	}
	artifact := JobArtifact{
		Application: lp.resolved.Name, Environment: s.environment, Server: target,
		CurrentRelease: hostState.CurrentRelease, RuntimeDigest: runtimeDigest,
		Job: jobID, Image: image, DataEffect: effect,
	}
	stateDigest, err := jobArtifactDigest(artifact)
	if err != nil {
		return JobPlan{}, err
	}
	operationID := s.newOperationID(now, gitShortSHA(ctx, filepath.Dir(lp.configPath)), KindJobRun)
	operation := OperationPlan{
		SchemaVersion: OperationPlanSchemaVersion, ID: operationID, Kind: KindJobRun,
		ReleaseID: hostState.CurrentRelease,
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(executablePlanTTL).Format(time.RFC3339Nano),
		Risk: risk, Reversibility: reversibility, Approval: approval,
		Binding: OperationBinding{
			Application: lp.resolved.Name, Environment: s.environment, Server: target,
			ConfigDigest: engine.HashBytes(lp.configBytes), ComposeDigest: runtimeDigest, StateDigest: stateDigest,
		},
		Steps: steps,
	}
	if err := operation.Seal(); err != nil {
		return JobPlan{}, fmt.Errorf("seal job operation: %w", err)
	}
	plan := JobPlan{
		SchemaVersion: ExecutableJobPlanSchemaVersion, Runner: s.runner,
		Operation: operation, Artifact: artifact, MigrationBackup: migrationBackup,
	}
	if err := plan.Seal(); err != nil {
		return JobPlan{}, fmt.Errorf("seal executable job plan: %w", err)
	}
	return plan, nil
}

func readJobRuntime(ctx context.Context, e *engine.Engine, releaseID, jobID string) ([]byte, string, string, error) {
	path := release.PathsFor(e.Names()).Releases + "/" + releaseID + "/compose.yaml"
	res, err := e.T.Run(ctx, "cat "+quote(path)+" 2>/dev/null")
	if err != nil {
		return nil, "", "", fmt.Errorf("read current runtime: %w", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return nil, "", "", fmt.Errorf("current release %q runtime is unavailable", releaseID)
	}
	var document struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(res.Stdout), &document); err != nil {
		return nil, "", "", fmt.Errorf("parse current runtime: %w", err)
	}
	service, ok := document.Services[jobID]
	if !ok {
		return nil, "", "", fmt.Errorf("job %q is absent from current release %s; run `ob deploy`", jobID, releaseID)
	}
	return []byte(res.Stdout), engine.HashBytes([]byte(res.Stdout)), strings.TrimSpace(service.Image), nil
}

var _ ExecutablePlan = (*JobPlan)(nil)
var _ ExecutablePlan = (*DeployPlan)(nil)
