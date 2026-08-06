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

// An encrypted entry's plaintext may already be an environment file.
//
// The field is `env_files`, so an author names a `.env`, puts `KEY=value` in
// it, and encrypts it. SOPS decrypts by the file's own format, so the plaintext
// comes back as dotenv — and demanding a flat YAML map rejected exactly the
// shape the field's name invites. Found on a host: the deploy failed with
// "decrypted content is not a YAML map", naming nothing an author could act on.
func TestDecryptedPlaintextMayAlreadyBeAnEnvironmentFile(t *testing.T) {
	for name, plaintext := range map[string]string{
		"dotenv":   "B=two\nA=one\n",
		"yaml map": "A: one\nB: two\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := renderDecrypted("probe", []byte(plaintext))
			if err != nil {
				t.Fatalf("%s plaintext must be accepted: %v", name, err)
			}
			if string(got) != "A=one\nB=two\n" {
				t.Errorf("both forms must render identically, got %q", got)
			}
		})
	}
}

// A decrypted secret is bytes, not a template.
//
// A general dotenv parser expands `$VAR` while reading, so a bcrypt hash or a
// generated password containing `$` was silently truncated at the first one —
// and the parser logged the fragment it could not resolve as a warning, putting
// part of the credential on stderr. Both are severe and neither was visible:
// the deploy succeeded and the application got a shorter password than the one
// in the file.
func TestDecryptedValuesAreNotExpanded(t *testing.T) {
	for name, plaintext := range map[string]string{
		"dotenv":   "HASH=$2y$10$abcdefghijklmno\nPW=ab$cd\n",
		"yaml map": "HASH: $2y$10$abcdefghijklmno\nPW: ab$cd\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := renderDecrypted("probe", []byte(plaintext))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out := string(got)
			if !strings.Contains(out, "HASH=$2y$10$abcdefghijklmno") {
				t.Errorf("the hash was altered: %q", out)
			}
			if !strings.Contains(out, "PW=ab$cd") {
				t.Errorf("the password was altered: %q", out)
			}
		})
	}
}
