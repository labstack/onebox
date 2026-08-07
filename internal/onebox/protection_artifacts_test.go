package onebox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

func TestBindProtectionArtifactsSealsMetadataWithoutContents(t *testing.T) {
	plan := validOperationPlan(t)
	generated := app.ProtectionArtifactSet{Artifacts: []app.GeneratedProtectionArtifact{
		{Class: "inputs", Path: "/var/lib/onebox/apps/example/protection/inputs.json", Mode: 0o600, Digest: "sha256:" + strings.Repeat("a", 64), Content: []byte("secret-value-canary")},
		{Class: "backup-schedule", Path: "/var/lib/onebox/apps/example/protection/backup.json", Mode: 0o644, Digest: "sha256:" + strings.Repeat("b", 64), Content: []byte("database-content-canary")},
	}}
	if err := BindProtectionArtifacts(&plan, generated); err != nil {
		t.Fatal(err)
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-value-canary", "database-content-canary"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("bound plan leaked artifact contents %q: %s", forbidden, encoded)
		}
	}
	if len(plan.Artifacts) != 2 || plan.Artifacts[0].Class != "backup-schedule" || plan.Artifacts[1].Class != "inputs" {
		t.Fatalf("artifact bindings are not deterministically sorted: %#v", plan.Artifacts)
	}
	before := plan.PlanDigest
	plan.Artifacts[0].Digest = "sha256:" + strings.Repeat("c", 64)
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	if plan.PlanDigest == before {
		t.Fatal("artifact digest did not affect the sealed plan identity")
	}
}

func TestOperationPlanRejectsUnsafeArtifactBinding(t *testing.T) {
	plan := validOperationPlan(t)
	plan.Artifacts = []OperationArtifactBinding{{
		Class: "inputs", Path: "relative/inputs.json", Mode: 0o600, Digest: "sha256:" + strings.Repeat("a", 64),
	}}
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "clean absolute") {
		t.Fatalf("unsafe artifact path error = %v", err)
	}
}
