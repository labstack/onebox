package onebox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/transport"
)

func backupEvidenceTestPlan(t *testing.T, base time.Time) DeployPlan {
	t.Helper()
	plan := sealedTestDeployPlan(t, base, base.Add(15*time.Minute))
	plan.Operation.Risk = RiskHigh
	plan.Operation.Reversibility = ReversibilityConditional
	plan.Operation.Approval = ApprovalStrong
	plan.Operation.Steps = []OperationStep{
		{ID: "preflight", Kind: StepPreflight, DataEffect: DataEffectNone},
		{ID: "job:migrate", Kind: StepJob, DependsOn: []string{"preflight"}, Component: "migrate", Service: "migrate", DataEffect: DataEffectMigration, ResultPolicy: JobResultProviderOrStrongUnknown, Mutation: true},
	}
	if err := plan.Operation.Seal(); err != nil {
		t.Fatal(err)
	}
	plan.MigrationBackup = &MigrationBackupRequirement{
		MaxAge:             "24h",
		RequireRestoreTest: true,
		Resources: []MigrationBackupResource{{
			Component: "database", Service: "postgres", Type: "postgres",
			Persistence: "durable", Volumes: []string{"db_data"},
		}},
		RequiredKeyMaterial: []string{"application_encryption_key", "runtime_environment"},
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	return plan
}

func backupEvidenceTestReceipt(t *testing.T, plan *DeployPlan, base time.Time) BackupEvidenceReceipt {
	t.Helper()
	digest := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	resourceEvidence := []MigrationBackupResourceEvidence{{
		Resource:  plan.MigrationBackup.Resources[0],
		BackupID:  "backup/postgres/2026-07-13T1700Z",
		CreatedAt: base.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		Integrity: BackupIntegrityEvidence{
			ArtifactDigest: digest("a"), Method: "sha256 manifest verification",
			ValidatedAt: base.Add(-time.Hour).Format(time.RFC3339Nano),
		},
		RestoreTest: BackupRestoreTestEvidence{
			State: BackupRestoreTestPassed, Method: "isolated restore and schema probe",
			TestedAt:         base.Add(-30 * time.Minute).Format(time.RFC3339Nano),
			ValidationDigest: digest("b"),
		},
	}}
	keyMaterial := make([]MigrationBackupKeyMaterialEvidence, 0, len(plan.MigrationBackup.RequiredKeyMaterial))
	for index, name := range plan.MigrationBackup.RequiredKeyMaterial {
		keyMaterial = append(keyMaterial, MigrationBackupKeyMaterialEvidence{
			Name: name, BackupID: "backup/key-material/" + name,
			CreatedAt: base.Add(-2 * time.Hour).Format(time.RFC3339Nano),
			Integrity: BackupIntegrityEvidence{
				ArtifactDigest: digest(string(rune('c' + index))), Method: "sha256 manifest verification",
				ValidatedAt: base.Add(-time.Hour).Format(time.RFC3339Nano),
			},
			Usability: BackupKeyMaterialUsabilityEvidence{
				Method: "isolated decrypt probe", ValidatedAt: base.Add(-45 * time.Minute).Format(time.RFC3339Nano),
				ValidationDigest: digest(string(rune('e' + index))),
			},
		})
	}
	receipt, err := NewBackupEvidenceReceipt(plan, "operator@example.test", base.Add(time.Minute), resourceEvidence, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestBackupEvidenceReceiptIsStrictFreshAndPlanBound(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := backupEvidenceTestPlan(t, base)
	receipt := backupEvidenceTestReceipt(t, &plan, base)
	if err := receipt.ValidateForPlan(&plan, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("valid receipt: %v", err)
	}

	t.Run("tamper detection", func(t *testing.T) {
		tampered := receipt
		tampered.Resources = append([]MigrationBackupResourceEvidence(nil), receipt.Resources...)
		tampered.Resources[0].BackupID = "different-artifact"
		if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("tampered receipt was accepted: %v", err)
		}
	})

	t.Run("resealed target mismatch", func(t *testing.T) {
		mismatch := receipt
		mismatch.Target = "deploy@other.example.test"
		if err := mismatch.Seal(); err != nil {
			t.Fatal(err)
		}
		if err := mismatch.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "target") {
			t.Fatalf("target mismatch was accepted: %v", err)
		}
	})

	t.Run("exact key material set", func(t *testing.T) {
		missing := receipt
		missing.KeyMaterial = append([]MigrationBackupKeyMaterialEvidence(nil), receipt.KeyMaterial[:1]...)
		if err := missing.Seal(); err != nil {
			t.Fatal(err)
		}
		if err := missing.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "key material") {
			t.Fatalf("incomplete key-material evidence was accepted: %v", err)
		}
	})

	t.Run("exact protected resource", func(t *testing.T) {
		mismatch := receipt
		mismatch.Resources = append([]MigrationBackupResourceEvidence(nil), receipt.Resources...)
		mismatch.Resources[0].Resource.Service = "other-postgres"
		if err := mismatch.Seal(); err != nil {
			t.Fatal(err)
		}
		if err := mismatch.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "resources") {
			t.Fatalf("wrong protected resource was accepted: %v", err)
		}
	})

	t.Run("freshness boundary is inclusive", func(t *testing.T) {
		boundary := receipt
		boundary.Resources = append([]MigrationBackupResourceEvidence(nil), receipt.Resources...)
		executionTime := base.Add(2 * time.Minute)
		boundary.Resources[0].CreatedAt = executionTime.Add(-24 * time.Hour).Format(time.RFC3339Nano)
		if err := boundary.Seal(); err != nil {
			t.Fatal(err)
		}
		if err := boundary.ValidateForPlan(&plan, executionTime); err != nil {
			t.Fatalf("backup exactly at max age was rejected: %v", err)
		}
	})

	t.Run("stale backup", func(t *testing.T) {
		stale := receipt
		stale.Resources = append([]MigrationBackupResourceEvidence(nil), receipt.Resources...)
		stale.Resources[0].CreatedAt = base.Add(-25 * time.Hour).Format(time.RFC3339Nano)
		if err := stale.Seal(); err != nil {
			t.Fatal(err)
		}
		if err := stale.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "older than") {
			t.Fatalf("stale backup was accepted: %v", err)
		}
	})

	t.Run("future validation", func(t *testing.T) {
		future := receipt
		future.Resources = append([]MigrationBackupResourceEvidence(nil), receipt.Resources...)
		future.Resources[0].Integrity.ValidatedAt = base.Add(10 * time.Minute).Format(time.RFC3339Nano)
		if err := future.Seal(); err != nil {
			t.Fatal(err)
		}
		if err := future.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "future") {
			t.Fatalf("future validation was accepted: %v", err)
		}
	})

	t.Run("restore test required", func(t *testing.T) {
		untested := receipt
		untested.Resources = append([]MigrationBackupResourceEvidence(nil), receipt.Resources...)
		untested.Resources[0].RestoreTest = BackupRestoreTestEvidence{State: BackupRestoreTestNotTested}
		if err := untested.Seal(); err != nil {
			t.Fatal(err)
		}
		if err := untested.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "passed restore test") {
			t.Fatalf("untested backup was accepted: %v", err)
		}
	})
}

func TestBackupEvidenceArtifactsAreStrictAndProtected(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := backupEvidenceTestPlan(t, base)
	receipt := backupEvidenceTestReceipt(t, &plan, base)
	path := filepath.Join(t.TempDir(), "nested", "backup-evidence.json")
	if err := receipt.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("receipt mode = %04o, want 0600", got)
	}
	loaded, err := LoadBackupEvidenceReceipt(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*loaded, receipt) {
		t.Fatalf("loaded receipt differs:\n got: %#v\nwant: %#v", *loaded, receipt)
	}

	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	document["validated"] = true
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	unknownPath := filepath.Join(t.TempDir(), "unknown.json")
	if err := os.WriteFile(unknownPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBackupEvidenceReceipt(unknownPath); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown boolean shortcut was accepted: %v", err)
	}

	oversized := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(oversized, make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBackupEvidenceReceipt(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized receipt was accepted: %v", err)
	}
}

func TestMigrationBackupOverrideRequiresStrongApprovalAndIsAuditable(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := backupEvidenceTestPlan(t, base)
	override, err := NewMigrationBackupOverride(&plan, "operator@example.test", "database provider is degraded; incident INC-42 authorizes fix-forward", base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateMigrationBackupForExecution(&plan, nil, &override, nil, true, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "strong") {
		t.Fatalf("override without strong approval was accepted: %v", err)
	}
	approval, err := NewApprovalGrant(&plan, "approver@example.test", base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := validateMigrationBackupForExecution(&plan, nil, &override, &approval, true, base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if audit.Mode != "override" || audit.OverrideDigest != override.OverrideDigest ||
		audit.OverrideOperator != override.Operator || audit.OverrideReason != override.Reason ||
		audit.OverrideCreatedAt != override.CreatedAt || audit.OverrideSource != override.Source {
		t.Fatalf("override audit evidence incomplete: %#v", audit)
	}

	tampered := override
	tampered.Reason = "different reason"
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered override was accepted: %v", err)
	}
	untrusted := override
	untrusted.Source = "mcp"
	if err := untrusted.Validate(); err == nil || !strings.Contains(err.Error(), "override source") {
		t.Fatalf("untrusted override source was accepted: %v", err)
	}

	receipt := backupEvidenceTestReceipt(t, &plan, base)
	if _, err := validateMigrationBackupForExecution(&plan, &receipt, &override, &approval, true, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("receipt plus override was accepted: %v", err)
	}
}

func TestPlanDerivesMigrationBackupRequirementAndExecuteRejectsMissingEvidenceBeforeConnecting(t *testing.T) {
	configPath := writeServiceProject(t)
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configText := strings.Replace(string(configBytes),
		"      allow_agent_proposals: true\n",
		"      allow_agent_proposals: true\n      require_migration_backup: true\n      migration_backup_max_age: 24h\n      require_migration_restore_test: true\n      migration_backup_key_material: [application_encryption_key]\n", 1)
	configText = strings.Replace(configText,
		"  database:\n",
		"  migrate:\n    type: job\n    service: migrate\n    data_effect: migration\n  database:\n", 1)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(filepath.Dir(configPath), "docker-compose.yaml")
	composeBytes, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	composeText := strings.Replace(string(composeBytes),
		"  postgres:\n",
		"  migrate:\n    image: ghcr.io/example/app:migrate\n  postgres:\n", 1)
	if err := os.WriteFile(composePath, []byte(composeText), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := serviceFake()
	connects := 0
	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	service := New(Options{
		ConfigPath: configPath,
		Now:        func() time.Time { return now },
		Connect: func(context.Context, string) (transport.Transport, error) {
			connects++
			return fake, nil
		},
	})
	plan, err := service.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.MigrationBackup == nil || len(plan.MigrationBackup.Resources) != 1 ||
		plan.MigrationBackup.Resources[0].Component != "database" ||
		!reflect.DeepEqual(plan.MigrationBackup.RequiredKeyMaterial, []string{"application_encryption_key"}) {
		t.Fatalf("plan lost migration backup binding: %#v", plan.MigrationBackup)
	}
	approval, err := NewApprovalGrant(&plan, "approver@example.test", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	connectsAfterPlan := connects
	now = now.Add(2 * time.Minute)
	_, err = service.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})
	if err == nil || !strings.Contains(err.Error(), "fresh backup evidence is required") {
		t.Fatalf("missing backup evidence was accepted: %v", err)
	}
	if connects != connectsAfterPlan {
		t.Fatalf("missing evidence reached target connection: before=%d after=%d", connectsAfterPlan, connects)
	}
}
