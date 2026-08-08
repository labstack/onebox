package transport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Upload must not report success for a copy that failed.
//
// Run reports a command that ran and failed through Result.ExitCode and reserves
// err for a process that could not start, so reading only err made every failed
// copy look like a successful upload. These tests drive the failure through a
// permission error rather than a cancellation: the fault is then deterministic,
// and it exercises the branch that formats the exit code, which a cancellation
// skips in favour of the context error.

func TestUploadFailsWhenTheDestinationCannotBeWritten(t *testing.T) {
	source := t.TempDir()
	writeFile(t, filepath.Join(source, "compose.yaml"), "services: {}\n")

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	err := NewLocal().Upload(context.Background(), source, filepath.Join(parent, "releases"))
	if err == nil {
		t.Fatal("upload reported success for a destination it could not create")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Errorf("failure does not report the exit status: %v", err)
	}
	// The operator needs the reason, not just the fact.
	if !strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Errorf("failure discards the command's own diagnosis: %v", err)
	}
}

func TestUploadFailsWhenTheSourceCannotBeRead(t *testing.T) {
	source := t.TempDir()
	writeFile(t, filepath.Join(source, "compose.yaml"), "services: {}\n")
	secret := filepath.Join(source, "secret")
	writeFile(t, secret, "x")
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })

	if os.Geteuid() == 0 {
		t.Skip("running as root; a mode-000 file is still readable")
	}

	if err := NewLocal().Upload(context.Background(), source, filepath.Join(t.TempDir(), "r1")); err == nil {
		t.Fatal("upload reported success for a source it could not fully read")
	}
}

func TestUploadSucceedsAndCopiesEverything(t *testing.T) {
	source := t.TempDir()
	writeFile(t, filepath.Join(source, "compose.yaml"), "services: {}\n")
	writeFile(t, filepath.Join(source, "ob.snapshot.yml"), "app: shop\n")

	dest := filepath.Join(t.TempDir(), "releases", "20260808-120000-abc")
	if err := NewLocal().Upload(context.Background(), source, dest); err != nil {
		t.Fatalf("upload: %v", err)
	}
	for _, name := range []string{"compose.yaml", "ob.snapshot.yml"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("%s did not arrive: %v", name, err)
		}
	}
}

// A context already done before the process starts is a transport fault, not a
// command failure, so it must arrive as err rather than as an exit code.
func TestUploadReportsAnAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewLocal().Upload(ctx, t.TempDir(), filepath.Join(t.TempDir(), "r1"))
	if err == nil {
		t.Fatal("upload reported success for an already-cancelled context")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
