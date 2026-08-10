package onebox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func approvalForTestPlan(t *testing.T, plan *DeployPlan) ApprovalGrant {
	t.Helper()
	createdAt, err := time.Parse(time.RFC3339Nano, plan.Operation.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := NewApprovalGrant(plan, nil, "operator@example", createdAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return approval
}

func TestApprovalGrantBindsEveryExecutionAuthority(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := sealedTestDeployPlan(t, base, base.Add(15*time.Minute))
	approval := approvalForTestPlan(t, &plan)
	if err := approval.ValidateForPlan(&plan, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*DeployPlan)
		want   string
	}{
		{"target", func(p *DeployPlan) { p.Operation.Binding.Server = "deploy@other.invalid" }, "target"},
		{"environment", func(p *DeployPlan) { p.Operation.Binding.Environment = "staging" }, "environment"},
		{"observed state", func(p *DeployPlan) { p.Operation.Binding.StateDigest = "sha256:other" }, "observed state"},
		{"risk", func(p *DeployPlan) { p.Operation.Risk = RiskHigh }, "risk"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := plan
			test.mutate(&changed)
			// Re-seal both envelopes to model a separately valid plan rather than
			// relying on the plan's own tamper detection.
			if err := changed.Operation.Seal(); err != nil {
				t.Fatal(err)
			}
			if err := changed.Seal(); err != nil {
				// State/application mutations can intentionally violate artifact
				// redundancy first; the grant still must never validate them.
				if err := approval.ValidateForPlan(&changed, base.Add(2*time.Minute)); err == nil {
					t.Fatalf("approval accepted changed %s", test.name)
				}
				return
			}
			if err := approval.ValidateForPlan(&changed, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("changed %s was not rejected clearly: %v", test.name, err)
			}
		})
	}
}

func TestApprovalGrantSaveLoadIsStrictAndProtected(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := sealedTestDeployPlan(t, base, base.Add(15*time.Minute))
	approval := approvalForTestPlan(t, &plan)
	path := filepath.Join(t.TempDir(), "nested", "approval.json")
	if err := approval.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("approval mode = %04o, want 0600", got)
	}
	loaded, err := LoadApprovalGrant(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ApprovalDigest != approval.ApprovalDigest || loaded.PlanDigest != plan.PlanDigest {
		t.Fatalf("loaded approval lost identity: %#v", loaded)
	}

	encoded, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	document["unrecognized_authority"] = true
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovalGrant(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown approval authority was not rejected: %v", err)
	}
}

func TestLoadApprovalGrantRejectsOversizedArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized-approval.json")
	if err := os.WriteFile(path, make([]byte, maxApprovalGrantBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovalGrant(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized approval error = %v", err)
	}
}

func TestExecuteRequiresPlanBoundApprovalBeforeConnecting(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	fake.Commands, fake.Uploads, fake.Inputs = nil, nil, nil
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "approval is required") {
		t.Fatalf("deploy without approval was not rejected: %v", err)
	}
	if len(fake.Commands) != 0 || len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("missing approval reached target: commands=%v uploads=%v inputs=%v", fake.Commands, fake.Uploads, fake.Inputs)
	}

	approval := approvalForTestPlan(t, &plan)
	if _, err := svc.Execute(context.Background(), ExecuteRequest{
		Kind: KindDeploy, Plan: &plan, Approval: &approval,
	}); err != nil && strings.Contains(err.Error(), "approval") {
		t.Fatalf("bound approval was rejected at the authority boundary: %v", err)
	}
	if len(fake.Commands) == 0 {
		t.Fatal("bound approval did not permit target execution")
	}
}

func TestApprovalCannotOutliveOrPostdatePlan(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := sealedTestDeployPlan(t, base, base.Add(15*time.Minute))
	approval := approvalForTestPlan(t, &plan)

	approval.ExpiresAt = base.Add(16 * time.Minute).Format(time.RFC3339Nano)
	if err := approval.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := approval.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "outlives") {
		t.Fatalf("approval outliving plan was not rejected: %v", err)
	}

	approval = approvalForTestPlan(t, &plan)
	if err := approval.ValidateForPlan(&plan, base.Add(20*time.Minute)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired approval was not rejected: %v", err)
	}
}

func TestApprovalRejectsUntrustedSourceAndUnsafeOperatorText(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := sealedTestDeployPlan(t, base, base.Add(15*time.Minute))

	approval := approvalForTestPlan(t, &plan)
	approval.Source = "mcp"
	if err := approval.Validate(); err == nil || !strings.Contains(err.Error(), "approval source") {
		t.Fatalf("untrusted source error = %v", err)
	}

	approval = approvalForTestPlan(t, &plan)
	approval.ApprovedBy = "operator@example\nsecret"
	if err := approval.Validate(); err == nil || !strings.Contains(err.Error(), "control line breaks") {
		t.Fatalf("unsafe operator error = %v", err)
	}

	approval = approvalForTestPlan(t, &plan)
	approval.ApprovedBy = "operator@example\x1b[31m"
	if err := approval.Validate(); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("terminal-control operator error = %v", err)
	}
}
