package onebox

import (
	"errors"
	"fmt"
	"regexp"
)

const (
	LifecycleCLIRunnerSchema       = "onebox.run/lifecycle-cli-runner/v1alpha1"
	LifecycleScheduledRunnerSchema = "onebox.run/lifecycle-scheduled-runner/v1alpha1"
	LifecycleArchiveRunnerSchema   = "onebox.run/lifecycle-archive-runner/v1alpha1"
	RestrictedArchiveSchemaVersion = "onebox.run/restricted-archive-envelope/v1alpha1"
)

// LifecycleOperationSchema is the single dispatch record shared by CLI and
// scheduled adapters. Runners are admitted by their exact schema, never by a
// broad "local" or "trusted" class.
type LifecycleOperationSchema struct {
	Kind          OperationKind
	EventKind     LifecycleKind
	Risk          RiskClass
	Reversibility ReversibilityClass
	Approval      ApprovalClass
	RunnerSchemas []string
}

var lifecycleOperationRegistry = map[OperationKind]LifecycleOperationSchema{
	KindServiceImagePatch: lifecycleSchema(KindServiceImagePatch, LifecycleServiceImagePatch, RiskHigh, ReversibilityConditional, ApprovalStrong),
	KindProtectionEnable:  lifecycleSchema(KindProtectionEnable, LifecycleProtectionEnable, RiskHigh, ReversibilityConditional, ApprovalStrong),
	KindProtectionDisable: lifecycleSchema(KindProtectionDisable, LifecycleProtectionDisable, RiskHigh, ReversibilityConditional, ApprovalStrong),
	KindBackupCreate:      scheduledLifecycleSchema(KindBackupCreate, LifecycleBackupCreate, RiskModerate, ReversibilityConditional, ApprovalStanding),
	KindBackupPrune:       scheduledLifecycleSchema(KindBackupPrune, LifecycleBackupPrune, RiskHigh, ReversibilityIrreversible, ApprovalStanding),
	KindReplayArchive:     archiveLifecycleSchema(KindReplayArchive, LifecycleReplayArchive, RiskLow, ReversibilityReversible, ApprovalStanding),
	KindRestoreTest:       scheduledLifecycleSchema(KindRestoreTest, LifecycleRestoreTest, RiskModerate, ReversibilityConditional, ApprovalStanding),
	KindRestorePrepare:    lifecycleSchema(KindRestorePrepare, LifecycleRestorePrepare, RiskHigh, ReversibilityConditional, ApprovalStrong),
	KindRestoreCutover:    lifecycleSchema(KindRestoreCutover, LifecycleRestoreCutover, RiskCritical, ReversibilityConditional, ApprovalStrong),
	KindRestoreAbort:      lifecycleSchema(KindRestoreAbort, LifecycleRestoreAbort, RiskHigh, ReversibilityConditional, ApprovalStrong),
	KindHygieneRun:        scheduledLifecycleSchema(KindHygieneRun, LifecycleHygieneRun, RiskModerate, ReversibilityConditional, ApprovalStanding),
	KindAssuranceCheck:    scheduledLifecycleSchema(KindAssuranceCheck, LifecycleAssuranceCheck, RiskLow, ReversibilityReversible, ApprovalNone),
}

var lifecycleEventRegistry = buildLifecycleEventRegistry()

func lifecycleSchema(kind OperationKind, eventKind LifecycleKind, risk RiskClass, reversibility ReversibilityClass, approval ApprovalClass) LifecycleOperationSchema {
	return LifecycleOperationSchema{
		Kind: kind, EventKind: eventKind, Risk: risk, Reversibility: reversibility, Approval: approval,
		RunnerSchemas: []string{LifecycleCLIRunnerSchema},
	}
}

func scheduledLifecycleSchema(kind OperationKind, eventKind LifecycleKind, risk RiskClass, reversibility ReversibilityClass, approval ApprovalClass) LifecycleOperationSchema {
	schema := lifecycleSchema(kind, eventKind, risk, reversibility, approval)
	schema.RunnerSchemas = append(schema.RunnerSchemas, LifecycleScheduledRunnerSchema)
	return schema
}

func archiveLifecycleSchema(kind OperationKind, eventKind LifecycleKind, risk RiskClass, reversibility ReversibilityClass, approval ApprovalClass) LifecycleOperationSchema {
	schema := scheduledLifecycleSchema(kind, eventKind, risk, reversibility, approval)
	schema.RunnerSchemas = append(schema.RunnerSchemas, LifecycleArchiveRunnerSchema)
	return schema
}

func buildLifecycleEventRegistry() map[LifecycleKind]OperationKind {
	registry := make(map[LifecycleKind]OperationKind, len(lifecycleOperationRegistry)+1)
	for kind, schema := range lifecycleOperationRegistry {
		registry[schema.EventKind] = kind
	}
	registry[LifecycleServiceTierStatus] = ""
	return registry
}

// LifecycleOperationSchemaFor performs exact runner-schema dispatch and
// returns a copy so callers cannot mutate the canonical registry.
func LifecycleOperationSchemaFor(kind OperationKind, runnerSchema string) (LifecycleOperationSchema, error) {
	schema, ok := lifecycleOperationRegistry[kind]
	if !ok {
		return LifecycleOperationSchema{}, fmt.Errorf("unsupported lifecycle operation %q", kind)
	}
	if !stringIn(schema.RunnerSchemas, runnerSchema) {
		return LifecycleOperationSchema{}, fmt.Errorf("unsupported runner schema %q for lifecycle operation %q", runnerSchema, kind)
	}
	schema.RunnerSchemas = append([]string(nil), schema.RunnerSchemas...)
	return schema, nil
}

// LifecycleOperationGraph returns the deterministic canonical steps for one
// lifecycle operation. The archive-hook runner receives a deliberately
// smaller graph that can only append replay evidence.
func LifecycleOperationGraph(kind OperationKind, runnerSchema, service string) ([]OperationStep, error) {
	if !safeLifecycleMetadata(service) {
		return nil, errors.New("service is required and must be secret-free metadata")
	}
	if _, err := LifecycleOperationSchemaFor(kind, runnerSchema); err != nil {
		return nil, err
	}
	if runnerSchema == LifecycleArchiveRunnerSchema {
		return chainedLifecycleSteps(
			OperationStep{ID: "archive-preflight", Kind: StepPreflight, Service: service, DataEffect: DataEffectNone},
			OperationStep{ID: "archive-append", Kind: StepArchiveAppend, Service: service, DataEffect: DataEffectNone, Mutation: true},
			OperationStep{ID: "archive-record", Kind: StepLifecycleRecord, Service: service, DataEffect: DataEffectNone, Mutation: true},
		), nil
	}
	return chainedLifecycleSteps(
		OperationStep{ID: "protection-lock", Kind: StepProtectionLock, Service: service, DataEffect: DataEffectNone},
		OperationStep{ID: "preflight", Kind: StepPreflight, Service: service, DataEffect: DataEffectNone},
		OperationStep{ID: "execute", Kind: StepLifecycleAction, Service: service, DataEffect: DataEffectUnknown, Mutation: true},
		OperationStep{ID: "verify", Kind: StepVerify, Service: service, DataEffect: DataEffectNone},
		OperationStep{ID: "record", Kind: StepLifecycleRecord, Service: service, DataEffect: DataEffectNone, Mutation: true},
	), nil
}

func chainedLifecycleSteps(steps ...OperationStep) []OperationStep {
	for index := range steps {
		if index > 0 {
			steps[index].DependsOn = []string{steps[index-1].ID}
		}
	}
	return steps
}

// RestrictedArchiveEnvelope is the only database-hook operation envelope. It
// cannot name a generic operation or runner and binds all writes to one service
// and one already-sealed lifecycle state.
type RestrictedArchiveEnvelope struct {
	SchemaVersion string        `json:"schema_version"`
	RunnerSchema  string        `json:"runner_schema"`
	OperationID   string        `json:"operation_id"`
	Kind          OperationKind `json:"kind"`
	Service       string        `json:"service"`
	StateDigest   string        `json:"state_digest"`
	HelperDigest  string        `json:"helper_digest"`
}

func (envelope RestrictedArchiveEnvelope) Validate() error {
	if envelope.SchemaVersion != RestrictedArchiveSchemaVersion {
		return fmt.Errorf("unsupported restricted archive envelope schema %q", envelope.SchemaVersion)
	}
	if envelope.RunnerSchema != LifecycleArchiveRunnerSchema {
		return fmt.Errorf("restricted archive envelope requires runner schema %q", LifecycleArchiveRunnerSchema)
	}
	if envelope.Kind != KindReplayArchive {
		return fmt.Errorf("restricted archive envelope cannot invoke operation %q", envelope.Kind)
	}
	if !safeLifecycleMetadata(envelope.OperationID) || !safeLifecycleMetadata(envelope.Service) {
		return errors.New("restricted archive operation and service identity must be safe metadata")
	}
	if !lifecycleGraphDigest.MatchString(envelope.StateDigest) || !lifecycleGraphDigest.MatchString(envelope.HelperDigest) {
		return errors.New("restricted archive state and helper digests must be pinned")
	}
	_, err := LifecycleOperationSchemaFor(envelope.Kind, envelope.RunnerSchema)
	return err
}

func (envelope RestrictedArchiveEnvelope) OperationGraph() ([]OperationStep, error) {
	if err := envelope.Validate(); err != nil {
		return nil, err
	}
	return LifecycleOperationGraph(envelope.Kind, envelope.RunnerSchema, envelope.Service)
}

func validateLifecycleOperationRegistry() error {
	if len(lifecycleOperationRegistry) != 12 {
		return fmt.Errorf("lifecycle operation registry has %d schemas, want 12", len(lifecycleOperationRegistry))
	}
	if len(lifecycleEventRegistry) != len(lifecycleOperationRegistry)+1 {
		return errors.New("structured lifecycle event registry is incomplete or ambiguous")
	}
	for kind, schema := range lifecycleOperationRegistry {
		if kind != schema.Kind || OperationKind(schema.EventKind) != kind {
			return fmt.Errorf("lifecycle operation %q has mismatched event kind %q", kind, schema.EventKind)
		}
		if !validOperationKind(kind) || !validRiskClass(schema.Risk) || !validReversibilityClass(schema.Reversibility) || !validApprovalClass(schema.Approval) {
			return fmt.Errorf("lifecycle operation %q has invalid plan metadata", kind)
		}
		if len(schema.RunnerSchemas) == 0 || schema.RunnerSchemas[0] != LifecycleCLIRunnerSchema {
			return fmt.Errorf("lifecycle operation %q is unavailable to the canonical CLI", kind)
		}
	}
	return nil
}

func stringIn(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

var lifecycleGraphDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
