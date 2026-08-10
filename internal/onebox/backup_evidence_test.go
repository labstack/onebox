package onebox

import (
	"context"
	"encoding/json"
	"fmt"
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
		MaximumAge:         "24h",
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

func backupReportForTest(t *testing.T, plan *DeployPlan, base time.Time) BackupReport {
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
	report, err := NewBackupReport(plan, "operator@example.test", base.Add(time.Minute), resourceEvidence, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func fillBackupReportTemplate(template BackupReport, observations BackupReport) BackupReport {
	filled := template
	filled.ReportedBy = observations.ReportedBy
	filled.ReportedAt = observations.ReportedAt
	filled.Resources = append([]MigrationBackupResourceEvidence(nil), template.Resources...)
	for index := range filled.Resources {
		observed := observations.Resources[index]
		filled.Resources[index].BackupID = observed.BackupID
		filled.Resources[index].CreatedAt = observed.CreatedAt
		filled.Resources[index].Integrity = observed.Integrity
		if filled.Resources[index].RestoreTest.State == BackupRestoreTestPassed {
			filled.Resources[index].RestoreTest.Method = observed.RestoreTest.Method
			filled.Resources[index].RestoreTest.TestedAt = observed.RestoreTest.TestedAt
			filled.Resources[index].RestoreTest.ValidationDigest = observed.RestoreTest.ValidationDigest
		}
	}
	filled.KeyMaterial = append([]MigrationBackupKeyMaterialEvidence(nil), template.KeyMaterial...)
	for index := range filled.KeyMaterial {
		observed := observations.KeyMaterial[index]
		filled.KeyMaterial[index].BackupID = observed.BackupID
		filled.KeyMaterial[index].CreatedAt = observed.CreatedAt
		filled.KeyMaterial[index].Integrity = observed.Integrity
		filled.KeyMaterial[index].Usability = observed.Usability
	}
	return filled
}

func TestBackupReportTemplateRoundTripPreservesPlanSkeleton(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	for _, requireRestoreTest := range []bool{true, false} {
		t.Run(fmt.Sprintf("restore_test_%t", requireRestoreTest), func(t *testing.T) {
			plan := backupEvidenceTestPlan(t, base)
			plan.MigrationBackup.RequireRestoreTest = requireRestoreTest
			if err := plan.Seal(); err != nil {
				t.Fatal(err)
			}
			template, err := NewBackupReportTemplate(&plan)
			if err != nil {
				t.Fatal(err)
			}
			if len(template.Resources) != len(plan.MigrationBackup.Resources) || len(template.KeyMaterial) != len(plan.MigrationBackup.RequiredKeyMaterial) {
				t.Fatalf("template shape = resources:%d keys:%d", len(template.Resources), len(template.KeyMaterial))
			}
			if !reflect.DeepEqual(template.Resources[0].Resource, plan.MigrationBackup.Resources[0]) {
				t.Fatalf("template changed protected resource: %#v", template.Resources[0].Resource)
			}
			wantRestoreState := BackupRestoreTestNotTested
			if requireRestoreTest {
				wantRestoreState = BackupRestoreTestPassed
			}
			if template.Resources[0].RestoreTest.State != wantRestoreState {
				t.Fatalf("template restore state = %q, want %q", template.Resources[0].RestoreTest.State, wantRestoreState)
			}
			for index, name := range plan.MigrationBackup.RequiredKeyMaterial {
				if template.KeyMaterial[index].Name != name {
					t.Fatalf("template key %d = %q, want %q", index, template.KeyMaterial[index].Name, name)
				}
			}
			filled := fillBackupReportTemplate(template, backupReportForTest(t, &plan, base))
			if err := filled.ValidateForPlan(&plan, base.Add(2*time.Minute)); err != nil {
				t.Fatalf("filled plan-produced template is not executable: %v", err)
			}
		})
	}
}

func TestBackupReportIsStrictFreshAndPlanBound(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := backupEvidenceTestPlan(t, base)
	report := backupReportForTest(t, &plan, base)
	if err := report.ValidateForPlan(&plan, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("valid report: %v", err)
	}
	confirmation, err := NewApprovalGrant(&plan, &report, "approver@example.test", base.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := validateMigrationBackupForExecution(&plan, &report, nil, &confirmation, true, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("execute with bound report: %v", err)
	}
	if audit.Mode != "receipt" || !sha256Digest.MatchString(audit.ReceiptDigest) || audit.RecordedAt != base.Add(2*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("internal attempt receipt = %#v", audit)
	}

	t.Run("content changes digest", func(t *testing.T) {
		originalDigest, err := report.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		tampered := report
		tampered.Resources = append([]MigrationBackupResourceEvidence(nil), report.Resources...)
		tampered.Resources[0].BackupID = "different-artifact"
		tamperedDigest, err := tampered.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		if tamperedDigest == originalDigest {
			t.Fatal("changed report retained the same digest")
		}
		if _, err := validateMigrationBackupForExecution(&plan, &tampered, nil, &confirmation, true, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "confirm the current plan and report again") {
			t.Fatalf("changed report was accepted with the old confirmation: %v", err)
		}
	})

	t.Run("target mismatch", func(t *testing.T) {
		mismatch := report
		mismatch.Server = "deploy@other.example.test"
		if err := mismatch.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "target") {
			t.Fatalf("target mismatch was accepted: %v", err)
		}
	})

	t.Run("exact key material set", func(t *testing.T) {
		missing := report
		missing.KeyMaterial = append([]MigrationBackupKeyMaterialEvidence(nil), report.KeyMaterial[:1]...)
		if err := missing.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "key material") {
			t.Fatalf("incomplete key-material evidence was accepted: %v", err)
		}
	})

	t.Run("exact protected resource", func(t *testing.T) {
		mismatch := report
		mismatch.Resources = append([]MigrationBackupResourceEvidence(nil), report.Resources...)
		mismatch.Resources[0].Resource.Service = "other-postgres"
		if err := mismatch.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "resources") {
			t.Fatalf("wrong protected resource was accepted: %v", err)
		}
	})

	t.Run("freshness boundary is inclusive", func(t *testing.T) {
		boundary := report
		boundary.Resources = append([]MigrationBackupResourceEvidence(nil), report.Resources...)
		executionTime := base.Add(2 * time.Minute)
		boundary.Resources[0].CreatedAt = executionTime.Add(-24 * time.Hour).Format(time.RFC3339Nano)
		if err := boundary.ValidateForPlan(&plan, executionTime); err != nil {
			t.Fatalf("backup exactly at max age was rejected: %v", err)
		}
	})

	t.Run("stale backup", func(t *testing.T) {
		stale := report
		stale.Resources = append([]MigrationBackupResourceEvidence(nil), report.Resources...)
		stale.Resources[0].CreatedAt = base.Add(-25 * time.Hour).Format(time.RFC3339Nano)
		if err := stale.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "older than") {
			t.Fatalf("stale backup was accepted: %v", err)
		}
	})

	t.Run("future validation", func(t *testing.T) {
		future := report
		future.Resources = append([]MigrationBackupResourceEvidence(nil), report.Resources...)
		future.Resources[0].Integrity.ValidatedAt = base.Add(10 * time.Minute).Format(time.RFC3339Nano)
		if err := future.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "future") {
			t.Fatalf("future validation was accepted: %v", err)
		}
	})

	t.Run("restore test required", func(t *testing.T) {
		untested := report
		untested.Resources = append([]MigrationBackupResourceEvidence(nil), report.Resources...)
		untested.Resources[0].RestoreTest = BackupRestoreTestEvidence{State: BackupRestoreTestNotTested}
		if err := untested.ValidateForPlan(&plan, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "passed restore test") {
			t.Fatalf("untested backup was accepted: %v", err)
		}
	})
}

func TestBackupReportArtifactsAreStrictAndTemplateIsProtected(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	plan := backupEvidenceTestPlan(t, base)
	report := backupReportForTest(t, &plan, base)
	template, err := NewBackupReportTemplate(&plan)
	if err != nil {
		t.Fatal(err)
	}
	filled := fillBackupReportTemplate(template, report)
	if err := filled.ValidateForPlan(&plan, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("filled plan-produced template is not executable: %v", err)
	}
	templatePath := filepath.Join(t.TempDir(), "nested", "backup-report-template.json")
	if err := template.SaveTemplate(templatePath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("template mode = %04o, want 0600", got)
	}
	path := filepath.Join(t.TempDir(), "backup-report.json")
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBackupReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*loaded, report) {
		t.Fatalf("loaded report differs:\n got: %#v\nwant: %#v", *loaded, report)
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
	if _, err := LoadBackupReport(unknownPath); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown boolean shortcut was accepted: %v", err)
	}

	oversized := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(oversized, make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBackupReport(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized report was accepted: %v", err)
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
	approval, err := NewApprovalGrant(&plan, nil, "approver@example.test", base.Add(time.Minute))
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

	report := backupReportForTest(t, &plan, base)
	if _, err := validateMigrationBackupForExecution(&plan, &report, &override, &approval, true, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("report plus override was accepted: %v", err)
	}
}

func TestPlanDerivesMigrationBackupRequirementAndExecuteRejectsMissingReportBeforeConnecting(t *testing.T) {
	configPath := writeServiceProject(t)
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configText := strings.Replace(string(configBytes),
		"      allow_agent_proposals: true\n",
		"      allow_agent_proposals: true\n      require_migration_backup: true\n      migration_backup_maximum_age: 24h\n      require_migration_restore_test: true\n      migration_backup_key_material: [application_encryption_key]\n", 1)
	configText = strings.Replace(configText,
		"  database:\n",
		"  migrate:\n    role: job\n    image: ghcr.io/example/app:migrate\n    when: pre_release\n    data_effect: migration\n  database:\n", 1)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
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
	approval, err := NewApprovalGrant(&plan, nil, "approver@example.test", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	connectsAfterPlan := connects
	now = now.Add(2 * time.Minute)
	_, err = service.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})
	if err == nil || !strings.Contains(err.Error(), "fresh backup report is required") {
		t.Fatalf("missing backup report was accepted: %v", err)
	}
	if connects != connectsAfterPlan {
		t.Fatalf("missing evidence reached target connection: before=%d after=%d", connectsAfterPlan, connects)
	}
}
