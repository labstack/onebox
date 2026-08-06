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
			// A dotenv plaintext is passed through byte for byte, so it keeps
			// the order and the quoting the author wrote. Parsing and
			// re-emitting it lost both: `KEY="a #b"` came back as `KEY=a #b`,
			// which the container runtime then reads as `a`.
			if !strings.Contains(string(got), "A=one") || !strings.Contains(string(got), "B=two") {
				t.Errorf("%s: both values must survive, got %q", name, got)
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

// Quoting and padding survive, because the plaintext is passed through rather
// than parsed and re-emitted.
//
// Stripping the quotes here and writing the value bare meant the container
// runtime re-parsed it and applied its own rules: `KEY="a #b"` became `a`, and
// `KEY="  padded  "` lost its padding. The same class as the `$` truncation —
// a secret altered in transit with nothing downstream able to tell.
func TestQuotedSecretsSurviveIntact(t *testing.T) {
	got, err := renderDecrypted("probe", []byte("KEY=\"a #b\"\nPAD=\"  padded  \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "KEY=\"a #b\"\nPAD=\"  padded  \"\n" {
		t.Errorf("the plaintext was altered: %q", got)
	}
}

// A payload declaring nothing is a failure, not an empty environment.
func TestAnEmptyPayloadIsRefused(t *testing.T) {
	for name, plaintext := range map[string]string{
		"empty map":     "{}\n",
		"empty":         "",
		"blank lines":   "\n\n",
		"comments only": "# nothing here\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := renderDecrypted("probe", []byte(plaintext)); err == nil {
				t.Error("an empty payload must be refused, or the application starts with every credential unset")
			}
		})
	}
}

// A value containing a colon is an environment line, not a YAML mapping.
//
// Deciding by "does YAML parse it" made `CONFIG={"a": 1}` — a service-account
// blob, among the most common things anyone encrypts — fail naming a key the
// author never wrote.
func TestColonBearingValuesAreEnvironmentLines(t *testing.T) {
	got, err := renderDecrypted("probe", []byte("CONFIG={\"a\": 1}\n"))
	if err != nil {
		t.Fatalf("a JSON value must be accepted: %v", err)
	}
	if string(got) != "CONFIG={\"a\": 1}\n" {
		t.Errorf("the value was altered: %q", got)
	}
}
