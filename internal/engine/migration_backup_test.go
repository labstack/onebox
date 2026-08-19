package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/transport"
)

func migrationBackupEngineConfig() *app.Resolved {
	cfg := testConfig()
	migrate := cfg.Workloads["migrate"]
	migrate.DataEffect = "migration"
	cfg.Workloads["migrate"] = migrate
	// The backup requirement needs at least one durable resource to name.
	worker := cfg.Workloads["worker"]
	worker.Persistence = &app.Persistence{Mode: "durable"}
	worker.Volumes = []app.Volume{{Name: "data", Path: "/data", Mode: "rw"}}
	cfg.Workloads["worker"] = worker
	environment := cfg.Environments["production"]
	environment.Policy.Migrations.RequireBackup = true
	environment.Policy.Migrations.BackupMaximumAge = "24h"
	cfg.Environments["production"] = environment
	return cfg
}

func migrationBackupEngineOptions(now time.Time, evidence *journal.MigrationBackupEvidence) Options {
	return Options{
		Out: &bytes.Buffer{}, Sleep: noSleep, Now: func() time.Time { return now },
		Environment: "production", MigrationBackup: evidence,
		ApprovalDigest: "sha256:approved", ApprovalClass: "strong", AllowUnknownMigration: true,
	}
}

func migrationBackupHappyFake() *transport.Fake {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "head -c") && strings.Contains(command, ".job-migrate-result") {
			return transport.Result{Stdout: "changed=false\n"}, true
		}
		return base(command)
	}
	return f
}

func TestMigrationBackupPolicyStopsBeforeMigrationWithoutEvidence(t *testing.T) {
	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	f := migrationBackupHappyFake()
	e := New(migrationBackupEngineConfig(), testProject(t), f, migrationBackupEngineOptions(now, nil))
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "migration backup report is required") {
		t.Fatalf("missing evidence did not stop deploy: %v", err)
	}
	if sequence := strings.Join(f.Commands, "\n"); strings.Contains(sequence, "OB_RESULT_FILE") {
		t.Fatalf("migration ran before evidence was accepted:\n%s", sequence)
	}
}

func TestMigrationBackupReceiptIsJournaledBeforeMigration(t *testing.T) {
	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	evidence := &journal.MigrationBackupEvidence{
		Mode: "receipt", ReceiptDigest: "sha256:" + strings.Repeat("a", 64),
		ProtectedResources: []string{"database/postgres"},
		ValidUntil:         now.Add(10 * time.Minute).Format(time.RFC3339Nano),
		RecordedBy:         "operator@example.test", RecordedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	f := migrationBackupHappyFake()
	e := New(migrationBackupEngineConfig(), testProject(t), f, migrationBackupEngineOptions(now, evidence))
	if err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir()); err != nil {
		t.Fatalf("deploy with receipt: %v", err)
	}
	sequence := strings.Join(f.Commands, "\n")
	evidenceIndex := strings.Index(sequence, `"sub_step":"`+journal.MigrationBackupSubStep+`"`)
	migrationIndex := strings.Index(sequence, "OB_RESULT_FILE")
	if evidenceIndex < 0 || migrationIndex < 0 || evidenceIndex > migrationIndex {
		t.Fatalf("receipt evidence was not journaled before migration:\n%s", sequence)
	}
	for _, want := range []string{`"mode":"receipt"`, `"receipt_digest":"` + evidence.ReceiptDigest + `"`, `"protected_resources":["database/postgres"]`} {
		if !strings.Contains(sequence, want) {
			t.Fatalf("journal missing %s:\n%s", want, sequence)
		}
	}
}

func TestMigrationBackupOverrideAuditIsJournaled(t *testing.T) {
	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	evidence := &journal.MigrationBackupEvidence{
		Mode: "override", OverrideDigest: "sha256:" + strings.Repeat("b", 64),
		ProtectedResources: []string{"database/postgres"},
		ValidUntil:         now.Add(10 * time.Minute).Format(time.RFC3339Nano),
		OverrideOperator:   "operator@example.test", OverrideReason: "incident INC-42 requires fix-forward",
		OverrideCreatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), OverrideSource: "local_cli",
	}
	f := migrationBackupHappyFake()
	e := New(migrationBackupEngineConfig(), testProject(t), f, migrationBackupEngineOptions(now, evidence))
	if err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir()); err != nil {
		t.Fatalf("deploy with override: %v", err)
	}
	sequence := strings.Join(f.Commands, "\n")
	for _, want := range []string{
		`"sub_step":"` + journal.MigrationBackupSubStep + `"`,
		`"mode":"override"`, `"override_digest":"` + evidence.OverrideDigest + `"`,
		`"override_operator":"` + evidence.OverrideOperator + `"`,
		`"override_reason":"` + evidence.OverrideReason + `"`,
		`"override_source":"` + evidence.OverrideSource + `"`,
	} {
		if !strings.Contains(sequence, want) {
			t.Fatalf("override audit missing %s:\n%s", want, sequence)
		}
	}
}

func TestExpiredMigrationBackupEvidenceStopsBeforeMigration(t *testing.T) {
	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	evidence := &journal.MigrationBackupEvidence{
		Mode: "receipt", ReceiptDigest: "sha256:" + strings.Repeat("a", 64),
		ProtectedResources: []string{"database/postgres"},
		ValidUntil:         now.Add(-time.Second).Format(time.RFC3339Nano),
		RecordedBy:         "operator@example.test", RecordedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	f := happyFake()
	e := New(migrationBackupEngineConfig(), testProject(t), f, migrationBackupEngineOptions(now, evidence))
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired evidence did not stop deploy: %v", err)
	}
	if sequence := strings.Join(f.Commands, "\n"); strings.Contains(sequence, "OB_RESULT_FILE") {
		t.Fatalf("migration ran with expired evidence:\n%s", sequence)
	}
}
