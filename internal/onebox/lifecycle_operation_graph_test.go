package onebox

import (
	"strings"
	"testing"
	"time"
)

var allLifecycleOperationKinds = []OperationKind{
	KindServiceImagePatch,
	KindProtectionEnable,
	KindProtectionDisable,
	KindBackupCreate,
	KindBackupPrune,
	KindReplayArchive,
	KindRestoreTest,
	KindRestorePrepare,
	KindRestoreCutover,
	KindRestoreAbort,
	KindHygieneRun,
	KindAssuranceCheck,
}

func TestLifecycleOperationSchemaDispatchesEveryCanonicalKind(t *testing.T) {
	if err := validateLifecycleOperationRegistry(); err != nil {
		t.Fatalf("validate lifecycle operation registry: %v", err)
	}

	createdAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	for _, kind := range allLifecycleOperationKinds {
		t.Run(string(kind), func(t *testing.T) {
			schema, err := LifecycleOperationSchemaFor(kind, LifecycleCLIRunnerSchema)
			if err != nil {
				t.Fatalf("dispatch CLI schema: %v", err)
			}
			steps, err := LifecycleOperationGraph(kind, LifecycleCLIRunnerSchema, "database")
			if err != nil {
				t.Fatalf("build operation graph: %v", err)
			}
			plan := OperationPlan{
				SchemaVersion: OperationPlanSchemaVersion,
				ID:            "operation-1",
				Kind:          kind,
				CreatedAt:     createdAt.Format(time.RFC3339),
				ExpiresAt:     createdAt.Add(time.Hour).Format(time.RFC3339),
				Risk:          schema.Risk,
				Reversibility: schema.Reversibility,
				Approval:      schema.Approval,
				Binding: OperationBinding{
					Application: "example", Environment: "production", Target: "host",
					ConfigDigest: "config", ComposeDigest: "compose", StateDigest: "state",
				},
				Steps: steps,
			}
			if err := plan.Seal(); err != nil {
				t.Fatalf("seal dispatched plan: %v", err)
			}

			record := LifecycleRecord{
				SchemaVersion: LifecycleRecordSchemaVersion,
				Type:          LifecycleRecordEvent,
				OperationID:   plan.ID,
				Kind:          schema.EventKind,
				Service:       "database",
				Event: &LifecycleEvent{
					Sequence: 1, Time: createdAt.Format(time.RFC3339), Phase: "preflight", State: "started",
				},
			}
			if kind == KindServiceImagePatch {
				record.PatchScope = "protected"
			}
			if err := record.Validate(); err != nil {
				t.Fatalf("validate dispatched structured event: %v", err)
			}
		})
	}
}

func TestScheduledRunnerHasClosedLifecycleAllowlist(t *testing.T) {
	allowed := map[OperationKind]bool{
		KindBackupCreate: true, KindBackupPrune: true, KindReplayArchive: true,
		KindRestoreTest: true, KindHygieneRun: true,
		KindAssuranceCheck: true,
	}
	for _, kind := range allLifecycleOperationKinds {
		_, err := LifecycleOperationSchemaFor(kind, LifecycleScheduledRunnerSchema)
		if allowed[kind] && err != nil {
			t.Errorf("scheduled runner rejected %q: %v", kind, err)
		}
		if !allowed[kind] && err == nil {
			t.Errorf("scheduled runner accepted operator-only operation %q", kind)
		}
	}
	for _, kind := range []OperationKind{KindServiceImagePatch, KindProtectionEnable, KindProtectionDisable} {
		if _, err := LifecycleOperationGraph(kind, LifecycleScheduledRunnerSchema, "database"); err == nil {
			t.Errorf("scheduled runner can execute forbidden operation %q", kind)
		}
	}
}

func TestRestrictedArchiveEnvelopeCanOnlyAppendReplayEvidence(t *testing.T) {
	envelope := RestrictedArchiveEnvelope{
		SchemaVersion: RestrictedArchiveSchemaVersion,
		RunnerSchema:  LifecycleArchiveRunnerSchema,
		OperationID:   "archive-1",
		Kind:          KindReplayArchive,
		Service:       "database",
		StateDigest:   "sha256:" + strings.Repeat("a", 64),
		HelperDigest:  "sha256:" + strings.Repeat("b", 64),
	}
	steps, err := envelope.OperationGraph()
	if err != nil {
		t.Fatalf("build restricted archive graph: %v", err)
	}
	if got, want := len(steps), 3; got != want {
		t.Fatalf("restricted archive graph has %d steps, want %d", got, want)
	}
	if steps[1].Kind != StepArchiveAppend {
		t.Fatalf("restricted archive mutation = %q, want %q", steps[1].Kind, StepArchiveAppend)
	}
	for _, step := range steps {
		if step.Kind == StepLifecycleAction {
			t.Fatal("restricted archive envelope can invoke an arbitrary lifecycle action")
		}
	}

	envelope.Kind = KindBackupCreate
	if err := envelope.Validate(); err == nil {
		t.Fatal("restricted archive envelope accepted backup_create")
	}
}

func TestLifecycleOperationRejectsUnsupportedRunnerSchema(t *testing.T) {
	if _, err := LifecycleOperationSchemaFor(KindBackupCreate, "onebox.run/unknown-runner/v1"); err == nil {
		t.Fatal("unknown lifecycle runner schema was accepted")
	}
	if _, err := LifecycleOperationSchemaFor(KindDeploy, LifecycleCLIRunnerSchema); err == nil {
		t.Fatal("non-lifecycle operation dispatched through lifecycle registry")
	}
}
