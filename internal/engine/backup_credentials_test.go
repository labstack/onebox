package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

type credentialInspectTransport struct {
	*transport.Fake
	mode os.FileMode
	data []byte
}

func (transport *credentialInspectTransport) Upload(_ context.Context, localDir, _ string) error {
	path := filepath.Join(localDir, "credentials.env")
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	transport.mode = info.Mode().Perm()
	transport.data, err = os.ReadFile(path)
	return err
}

func TestInstallBackupCredentialFileUsesPrivateTargetFileWithoutCommandLeak(t *testing.T) {
	fake := &transport.Fake{}
	inspector := &credentialInspectTransport{Fake: fake}
	engine := backupLockTestEngine(fake)
	engine.T = inspector
	engine.fenceVal = "deploy-1 1"
	engine.backupLockVals = map[string]string{"database": "service-lock"}
	engine.backupFenceVals = map[string]string{"database": "backup-1 1"}
	credentialCanary := "credential-canary-value"
	databaseCanary := "database-row-canary"
	plaintext := []byte("BACKUP_ACCESS_KEY_ID=access\nBACKUP_SECRET_ACCESS_KEY=" + credentialCanary + "\nDATABASE_CONTENT=" + databaseCanary + "\n")

	path, err := engine.InstallBackupCredentialFile(
		context.Background(), "database", "offsite",
		[]string{"BACKUP_ACCESS_KEY_ID", "BACKUP_SECRET_ACCESS_KEY"}, plaintext,
	)
	if err != nil {
		t.Fatalf("install backup credentials: %v", err)
	}
	if path != "/var/lib/ob/example/backup/secrets/database-offsite.env" {
		t.Fatalf("credential path = %q", path)
	}
	if inspector.mode != 0o600 {
		t.Fatalf("local staging mode = %#o, want 0600", inspector.mode)
	}
	if string(inspector.data) != string(plaintext) {
		t.Fatal("private upload did not preserve credential bytes")
	}
	for _, command := range fake.Commands {
		if strings.Contains(command, credentialCanary) || strings.Contains(command, databaseCanary) {
			t.Fatalf("credential or database content leaked into host command: %s", command)
		}
	}
}

func TestBackupCredentialErrorsDoNotEchoValues(t *testing.T) {
	fake := &transport.Fake{}
	engine := backupLockTestEngine(fake)
	secret := "credential-canary-value"
	_, err := engine.InstallBackupCredentialFile(
		context.Background(), "database", "offsite", []string{"REQUIRED_ENTRY"},
		[]byte("PRESENT_ENTRY="+secret+"\n"),
	)
	if err == nil {
		t.Fatal("missing credential slot was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("credential error leaked value: %v", err)
	}
}

func TestBackupCredentialGrammarAcceptsLowercaseEnvironmentNames(t *testing.T) {
	fake := &transport.Fake{}
	engine := backupLockTestEngine(fake)
	engine.fenceVal = "deploy-1 1"
	engine.backupLockVals = map[string]string{"database": "service-lock"}
	engine.backupFenceVals = map[string]string{"database": "backup-1 1"}
	if _, err := engine.InstallBackupCredentialFile(
		context.Background(), "database", "offsite", []string{"aws_access_key"}, []byte("aws_access_key=value\n"),
	); err != nil {
		t.Fatalf("lowercase schema-valid credential entry was rejected: %v", err)
	}
}

func TestBackupCredentialInstallFailureCleansRemotePlaintextStaging(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "cp ") && strings.Contains(command, ".credential-staging-") {
			return transport.Result{ExitCode: 1}, true
		}
		return transport.Result{}, false
	}}
	engine := backupLockTestEngine(fake)
	engine.fenceVal = "deploy-1 1"
	engine.backupLockVals = map[string]string{"database": "service-lock"}
	engine.backupFenceVals = map[string]string{"database": "backup-1 1"}
	if _, err := engine.InstallBackupCredentialFile(
		context.Background(), "database", "offsite", []string{"REQUIRED_ENTRY"}, []byte("REQUIRED_ENTRY=value\n"),
	); err == nil {
		t.Fatal("failed target install was accepted")
	}
	foundCleanup := false
	for _, command := range fake.Commands {
		if strings.HasPrefix(command, "rm -rf ") && strings.Contains(command, ".credential-staging-") && strings.Contains(command, ".tmp") {
			foundCleanup = true
		}
	}
	if !foundCleanup {
		t.Fatalf("remote plaintext staging was not cleaned: %#v", fake.Commands)
	}
}

func TestMigrateBackupCredentialFilesCopiesAnUnambiguousLegacyPath(t *testing.T) {
	cfg := testConfig()
	state := app.ServiceRuntimeState{
		BackupState: "enabled",
		LastEffective: &app.BackupEffectiveProjection{
			Policy: app.BackupPolicy{Target: "off-site"},
		},
	}
	var err error
	cfg, err = cfg.WithServiceRuntimeStates(map[string]app.ServiceRuntimeState{"postgres": state})
	if err != nil {
		t.Fatal(err)
	}
	fake := &transport.Fake{}
	engine := New(cfg, nil, fake, Options{Out: io.Discard})
	engine.fenceVal = "deploy-1 1"
	if err := engine.MigrateBackupCredentialFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(fake.Commands, "\n")
	if !strings.Contains(commands, "install -m 600") ||
		!strings.Contains(commands, "postgres-off-site.env") ||
		!strings.Contains(commands, "postgres-off--site.env") {
		t.Fatalf("credential migration did not copy legacy to escaped path:\n%s", commands)
	}
}

func TestMigrateBackupCredentialFilesRefusesAnAmbiguousLegacyPath(t *testing.T) {
	cfg := testConfig()
	service := cfg.Services["postgres"]
	delete(cfg.Services, "postgres")
	cfg.Services["a-b"] = service
	cfg.Services["a"] = service
	stateFor := func(target string) app.ServiceRuntimeState {
		return app.ServiceRuntimeState{
			BackupState: "enabled",
			LastEffective: &app.BackupEffectiveProjection{
				Policy: app.BackupPolicy{Target: target},
			},
		}
	}
	var err error
	cfg, err = cfg.WithServiceRuntimeStates(map[string]app.ServiceRuntimeState{
		"a-b": stateFor("c"),
		"a":   stateFor("b-c"),
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.HasPrefix(command, "if [ -f ") {
			return transport.Result{ExitCode: 1}, true
		}
		return transport.Result{}, false
	}}
	engine := New(cfg, nil, fake, Options{Out: io.Discard})
	err = engine.MigrateBackupCredentialFiles(context.Background())
	if err == nil || !strings.Contains(err.Error(), "belongs to more than one service/target pair") {
		t.Fatalf("ambiguous migration error = %v", err)
	}
}
