package onebox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationPlanDigestRoundTripAndTampering(t *testing.T) {
	t.Parallel()
	plan := validOperationPlan(t)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Save(path); err != nil {
		t.Fatal(err)
	}
	if plan.PlanDigest == "" {
		t.Fatal("Save did not seal the plan")
	}
	loaded, err := LoadOperationPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PlanDigest != plan.PlanDigest {
		t.Fatalf("loaded digest = %q, want %q", loaded.PlanDigest, plan.PlanDigest)
	}

	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(encoded), `"server": "deploy@example.test"`, `"server": "attacker@example.test"`, 1)
	if tampered == string(encoded) {
		t.Fatal("test did not alter encoded plan")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOperationPlan(path); err == nil || !strings.Contains(err.Error(), "plan digest mismatch") {
		t.Fatalf("LoadOperationPlan tampering error = %v, want digest mismatch", err)
	}
}

func TestOperationPlanCanonicalJSONExcludesDigest(t *testing.T) {
	t.Parallel()
	plan := validOperationPlan(t)
	plan.PlanDigest = "must-not-be-hashed"
	encoded, err := plan.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "plan_digest") || strings.Contains(string(encoded), plan.PlanDigest) {
		t.Fatalf("canonical JSON contains plan digest: %s", encoded)
	}
	first, err := plan.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	plan.PlanDigest = "a-different-existing-digest"
	second, err := plan.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("digest depends on PlanDigest: %q != %q", first, second)
	}
}

func TestLoadOperationPlanRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	plan := validOperationPlan(t)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Save(path); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded = []byte(strings.Replace(string(encoded), "{", `{"unexpected":true,`, 1))
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOperationPlan(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("LoadOperationPlan unknown-field error = %v", err)
	}
}

func TestOperationPlanValidationRejectsInvalidDependencies(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		steps []OperationStep
		want  string
	}{
		"future": {
			steps: []OperationStep{
				{ID: "first", Kind: StepPreflight, DependsOn: []string{"later"}, DataEffect: DataEffectNone},
				{ID: "later", Kind: StepVerify, DataEffect: DataEffectNone},
			},
			want: "must identify an earlier step",
		},
		"missing": {
			steps: []OperationStep{
				{ID: "first", Kind: StepPreflight, DataEffect: DataEffectNone},
				{ID: "second", Kind: StepVerify, DependsOn: []string{"absent"}, DataEffect: DataEffectNone},
			},
			want: "must identify an earlier step",
		},
		"duplicate ID": {
			steps: []OperationStep{
				{ID: "same", Kind: StepPreflight, DataEffect: DataEffectNone},
				{ID: "same", Kind: StepVerify, DependsOn: []string{"same"}, DataEffect: DataEffectNone},
			},
			want: "duplicate step id",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			plan := validOperationPlan(t)
			plan.Steps = test.steps
			if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestOperationPlanValidationRequiresBinding(t *testing.T) {
	t.Parallel()
	plan := validOperationPlan(t)
	plan.Binding.StateDigest = ""
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "state_digest is required") {
		t.Fatalf("Validate error = %v, want required state digest", err)
	}
}

func TestOperationPlanValidationRejectsInvalidLifetime(t *testing.T) {
	t.Parallel()
	plan := validOperationPlan(t)
	plan.CreatedAt = "not-a-time"
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "created_at must be RFC3339") {
		t.Fatalf("malformed created_at was not rejected: %v", err)
	}
	plan = validOperationPlan(t)
	plan.ExpiresAt = plan.CreatedAt
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "after created_at") {
		t.Fatalf("reversed lifetime was not rejected: %v", err)
	}
}

func validOperationPlan(t *testing.T) OperationPlan {
	t.Helper()
	cfg := operationGraphConfig()
	steps, err := deploymentGraph(cfg, "20260712-120000-abcd")
	if err != nil {
		t.Fatal(err)
	}
	return OperationPlan{
		SchemaVersion: OperationPlanSchemaVersion,
		ID:            "operation-123",
		Kind:          KindDeploy,
		ReleaseID:     "20260712-120000-abcd",
		CreatedAt:     "2026-07-12T12:00:00Z",
		ExpiresAt:     "2026-07-12T12:15:00Z",
		Risk:          RiskModerate,
		Reversibility: ReversibilityConditional,
		Approval:      ApprovalOneTime,
		Binding: OperationBinding{
			Application:   "example",
			Environment:   "production",
			Server:        "deploy@example.test",
			ConfigDigest:  "sha256:config",
			ComposeDigest: "sha256:compose",
			StateDigest:   "sha256:state",
			PayloadDigest: "sha256:payload",
		},
		Steps: steps,
	}
}
