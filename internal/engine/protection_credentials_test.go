package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestInstallProtectionCredentialFileUsesPrivateTargetFileWithoutCommandLeak(t *testing.T) {
	fake := &transport.Fake{}
	inspector := &credentialInspectTransport{Fake: fake}
	engine := protectionLockTestEngine(fake)
	engine.T = inspector
	engine.fenceVal = "deploy-1 1"
	engine.protectionLockVals = map[string]string{"database": "service-lock"}
	engine.protectionFenceVals = map[string]string{"database": "backup-1 1"}
	credentialCanary := "credential-canary-value"
	databaseCanary := "database-row-canary"
	plaintext := []byte("BACKUP_ACCESS_KEY_ID=access\nBACKUP_SECRET_ACCESS_KEY=" + credentialCanary + "\nDATABASE_CONTENT=" + databaseCanary + "\n")

	path, err := engine.InstallProtectionCredentialFile(
		context.Background(), "database", "offsite",
		[]string{"BACKUP_ACCESS_KEY_ID", "BACKUP_SECRET_ACCESS_KEY"}, plaintext,
	)
	if err != nil {
		t.Fatalf("install protection credentials: %v", err)
	}
	if path != "/var/lib/ob/example/protection/secrets/database-offsite.env" {
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

func TestProtectionCredentialErrorsDoNotEchoValues(t *testing.T) {
	fake := &transport.Fake{}
	engine := protectionLockTestEngine(fake)
	secret := "credential-canary-value"
	_, err := engine.InstallProtectionCredentialFile(
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
