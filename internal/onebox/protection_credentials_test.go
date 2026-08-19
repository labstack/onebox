package onebox

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
)

func TestProtectionSecretSlotsContainReferencesOnly(t *testing.T) {
	cfg := &app.Resolved{
		Spec: &app.Spec{
			Name:     "example",
			BasePath: "/var/lib/ob",
			BackupTargets: map[string]app.BackupTarget{
				"offsite": {
					Credentials: app.CredentialReference{
						File: "secrets/backup.env", Provider: "sops",
						AccessKeyEntry: "BACKUP_ACCESS_KEY_ID", SecretKeyEntry: "BACKUP_SECRET_ACCESS_KEY",
					},
				},
			},
			Services: map[string]app.Service{
				"database": {Driver: "postgres", Version: 17, Protection: &app.ProtectionPolicy{Target: "offsite"}},
			},
		},
		Env: "production",
	}
	slots, err := ProtectionSecretSlots(cfg, "database")
	if err != nil {
		t.Fatalf("resolve protection slots: %v", err)
	}
	wantEntries := []string{"BACKUP_ACCESS_KEY_ID", "BACKUP_SECRET_ACCESS_KEY", "OB_REPOSITORY_PASSPHRASE", "POSTGRES_PASSWORD"}
	if len(slots) != len(wantEntries) {
		t.Fatalf("slots = %#v", slots)
	}
	for index, want := range wantEntries {
		if slots[index].Entry != want || slots[index].File != "/var/lib/ob/example/protection/secrets/database-offsite.env" {
			t.Errorf("slot %d = %#v, want entry %q in target-side file", index, slots[index], want)
		}
	}

	steps, err := LifecycleOperationGraph(KindBackupCreate, LifecycleCLIRunnerSchema, "database")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	plan := OperationPlan{
		SchemaVersion: OperationPlanSchemaVersion, ID: "backup-1", Kind: KindBackupCreate,
		CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		Risk: RiskModerate, Reversibility: ReversibilityConditional, Approval: ApprovalStanding,
		Binding: OperationBinding{Application: "example", Environment: "production", Server: "host", ConfigDigest: "config", ComposeDigest: "compose", StateDigest: "state"},
		Steps:   steps, SecretSlots: slots,
	}
	if err := plan.Seal(); err != nil {
		t.Fatalf("seal protection plan: %v", err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credential-canary", "database-row-canary", "secret_value", "value"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("plan contains forbidden credential/content field %q: %s", forbidden, encoded)
		}
	}
}

func TestOperationPlanRejectsInlineOrRelativeSecretSlots(t *testing.T) {
	plan := OperationPlan{SecretSlots: []SecretSlotReference{{Slot: "credential:key", Entry: "ACCESS_KEY", File: "relative.env"}}}
	plan.SchemaVersion = OperationPlanSchemaVersion
	plan.ID, plan.Kind = "backup-1", KindBackupCreate
	plan.CreatedAt, plan.ExpiresAt = "2026-08-07T12:00:00Z", "2026-08-07T13:00:00Z"
	plan.Risk, plan.Reversibility, plan.Approval = RiskModerate, ReversibilityConditional, ApprovalStanding
	plan.Binding = OperationBinding{Application: "example", Environment: "production", Server: "host", ConfigDigest: "config", ComposeDigest: "compose", StateDigest: "state"}
	plan.Steps = []OperationStep{{ID: "preflight", Kind: StepPreflight, DataEffect: DataEffectNone}}
	if err := plan.Validate(); err == nil {
		t.Fatal("relative secret slot path was accepted")
	}
}
