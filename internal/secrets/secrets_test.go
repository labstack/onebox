package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubSops puts a fake `sops` first in PATH that prints the given plaintext.
func stubSops(t *testing.T, plaintext string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + plaintext + "EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRenderContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RenderContext(ctx, t.TempDir(), "secrets.enc.yaml"); !errors.Is(err, context.Canceled) {
		t.Fatalf("RenderContext error = %v, want context canceled", err)
	}
}

func TestRenderSortedEnv(t *testing.T) {
	stubSops(t, "ZKEY: last\nDATABASE_URL: postgres://u:p@h/db\nDEBUG: false\n")
	b, err := Render(t.TempDir(), "secrets.enc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := "DATABASE_URL=postgres://u:p@h/db\nDEBUG=false\nZKEY=last\n"
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
}

func TestRenderRejectsNestedAndBadKeys(t *testing.T) {
	stubSops(t, "nested:\n  a: 1\n")
	if _, err := Render(t.TempDir(), "s.enc.yaml"); err == nil || !strings.Contains(err.Error(), "flat") {
		t.Fatalf("nested must be rejected: %v", err)
	}
	stubSops(t, "\"bad key\": x\n")
	if _, err := Render(t.TempDir(), "s.enc.yaml"); err == nil {
		t.Fatal("invalid env name must be rejected")
	}
}
