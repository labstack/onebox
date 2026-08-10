package onebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

// fakeSops puts a `sops` on PATH that decrypts by echoing the file. The real
// binary's job here is only to turn a path into bytes; what this suite is
// about is which bytes end up in which file, and every defect this covers
// lives on the ob side of that boundary.
func fakeSops(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexec cat \"$2\"\n" // sops -d <file>
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// project writes a two-workload project whose entries differ per workload, and
// returns its ob.yml path.
func twoEncryptedEntries(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("api.enc.env", "TOKEN=api-token\n")
	write("worker.enc.env", "TOKEN=worker-token\n")
	write("shared.env", "REGION=eu\n")
	write("ob.yml", `api_version: onebox.run/v1
app: shop
environments:
  production:
    server: root@h
runtime:
  env_files:
    - shared.env
workloads:
  web:
    image: nginx
    port: 3000
    domain: shop.example.com
    volumes:
      - {source: ., path: /app}
    env_files:
      - shared.env
      - {file: api.enc.env, provider: sops}
  jobs:
    role: worker
    image: nginx
    env_files:
      - {file: worker.enc.env, provider: sops}
`)
	return filepath.Join(dir, "ob.yml")
}

// A root bind copies every project file, including a stale file at the name
// reserved for decrypted output. Decryption must win so an old secret can never
// replace the current SOPS payload.
func TestRootBindCannotOverwriteProjectedSecret(t *testing.T) {
	fakeSops(t)
	configPath := twoEncryptedEntries(t)
	name := app.EnvFile{File: "api.enc.env", Provider: "sops"}.StagedPath()
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), name), []byte("TOKEN=stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lp, err := loadProjectAt(context.Background(), configPath, "production", false, app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	staging, cleanup, err := stageExecution(context.Background(), lp, "production", "R1", "sg-000000000000000000000001", app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	generationPath := filepath.FromSlash(app.SecretGenerationPath("sg-000000000000000000000001", name))
	body, err := os.ReadFile(filepath.Join(staging, generationPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "TOKEN=api-token\n" {
		t.Fatalf("staged secret = %q, want current decrypted value", body)
	}
	info, err := os.Stat(filepath.Join(staging, generationPath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("staged secret mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(staging, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale root-level secret survived generation staging: %v", err)
	}
}

// Every encrypted entry is decrypted into its own file, at the name the
// generated runtime references for it.
//
// This is the whole reason the environment model was restructured, and it was
// the part with no test that could fail: deleting the staging loop, sharing one
// filename across entries, or staging only the first entry all left the suite
// green. Each of those ships an application holding another workload's
// credentials, or none, with a successful deploy reported either way.
func TestEveryEncryptedEntryIsStagedUnderItsOwnName(t *testing.T) {
	fakeSops(t)
	configPath := twoEncryptedEntries(t)

	lp, err := loadProjectAt(context.Background(), configPath, "production", false, app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	staging, cleanup, err := stageExecution(context.Background(), lp, "production", "R1", "sg-000000000000000000000001", app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// The two entries must land in two files, each holding only its own value.
	// A shared staged name passes the "a file exists" check and fails this one.
	want := map[string]string{
		"api.enc.env":    "api-token",
		"worker.enc.env": "worker-token",
	}
	staged := map[string]string{}
	for file, token := range want {
		name := app.EnvFile{File: file, Provider: "sops"}.StagedPath()
		generationPath := filepath.FromSlash(app.SecretGenerationPath("sg-000000000000000000000001", name))
		body, err := os.ReadFile(filepath.Join(staging, generationPath))
		if err != nil {
			t.Fatalf("%s was never staged: %v", file, err)
		}
		if !strings.Contains(string(body), token) {
			t.Errorf("%s holds %q, which is not its own value", name, body)
		}
		for other, otherToken := range want {
			if other != file && strings.Contains(string(body), otherToken) {
				t.Errorf("%s holds %s's credentials", name, other)
			}
		}
		staged[name] = string(body)
	}
	if len(staged) != 2 {
		t.Fatalf("two entries must produce two files, got %d", len(staged))
	}
	runtime, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runtime), "ob.secret-generation: sg-000000000000000000000001") ||
		!strings.Contains(string(runtime), app.SecretGenerationDirectory+"/sg-000000000000000000000001/") {
		t.Fatalf("initial deployment runtime does not bind the opaque generation:\n%s", runtime)
	}

	// A staged file nothing references is staged for nothing. The generated
	// document must name each one, or the container never reads it — which is
	// the failure mode a filename change on one side alone produces.
	body, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name := range staged {
		if !strings.Contains(string(body), name) {
			t.Errorf("the runtime references no %q, so the file is staged for nothing", name)
		}
	}
}

// A plaintext entry is referenced under its own name, never a staged one.
//
// Routing every entry through StagedPath would have the runtime reference
// `.ob-decrypted-…` for a file that is checked in and never decrypted.
func TestAPlaintextEntryKeepsItsOwnName(t *testing.T) {
	fakeSops(t)
	configPath := twoEncryptedEntries(t)

	lp, err := loadProjectAt(context.Background(), configPath, "production", false, app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	staging, cleanup, err := stageExecution(context.Background(), lp, "production", "R1", "sg-000000000000000000000001", app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	body, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "shared.env") {
		t.Error("the plaintext entry is not referenced")
	}
	if strings.Contains(string(body), ".ob-decrypted-sops-shared.env") {
		t.Error("a plaintext entry was given a decrypted name; nothing writes that file")
	}
}

// Which entries a release must decrypt comes from what workloads resolve, not
// from one scope read directly.
//
// The predecessor sorted the declared secrets and took the first, which is how
// one environment's credentials reached another. Returning nothing at all is
// the same class of silence: the deploy succeeds and every container starts
// with its credentials unset.
func TestEncryptedEntriesCoversEveryWorkload(t *testing.T) {
	fakeSops(t)
	configPath := twoEncryptedEntries(t)

	lp, err := loadProjectAt(context.Background(), configPath, "production", false, app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	entries := encryptedEntries(lp.resolved)
	if len(entries) != 2 {
		t.Fatalf("both workloads' encrypted entries must be collected, got %d: %+v", len(entries), entries)
	}
	// Deduplicated, and in a stable order — a set that varies run to run makes
	// the staged release differ from itself.
	// Ordered by workload name — `jobs` before `web` — because ranging a map
	// is not an order, and a set that varies run to run makes a staged release
	// differ from itself.
	if entries[0].File != "worker.enc.env" || entries[1].File != "api.enc.env" {
		t.Errorf("the order must be stable, got %+v", entries)
	}
	for _, e := range entries {
		if !e.Encrypted() {
			t.Errorf("%s is not encrypted and must not be decrypted", e.File)
		}
	}
}

func TestExternalServiceConnectionIsProjectedLeastPrivilegeIntoRelease(t *testing.T) {
	fakeSops(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := "DATABASE_URL=\"postgres://user:p$a#s@db/app\"\nUNRELATED=must-not-reach-workload\n"
	if err := os.WriteFile(filepath.Join(dir, "secrets", "database.env"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	project := `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web:
    image: nginx
    needs:
      - name: database
        condition: healthy
        env: {APP_DATABASE_URL: url}
external_services:
  database:
    driver: postgres
    connection:
      source: {file: secrets/database.env, provider: sops}
      entries: {url: DATABASE_URL}
    protection_owner: platform-team/rds
    probe: {}
`
	configPath := filepath.Join(dir, "ob.yml")
	if err := os.WriteFile(configPath, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	lp, err := loadProjectAt(context.Background(), configPath, "production", false, app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	staging, cleanup, err := stageExecution(context.Background(), lp, "production", "R1", "sg-000000000000000000000001", app.Images{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	projectionPath := ".ob-external-database_web.env"
	generationPath := filepath.FromSlash(app.SecretGenerationPath("sg-000000000000000000000001", projectionPath))
	projected, err := os.ReadFile(filepath.Join(staging, generationPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(projected) != "APP_DATABASE_URL=\"postgres://user:p$a#s@db/app\"\n" {
		t.Fatalf("external projection = %q", projected)
	}
	info, err := os.Stat(filepath.Join(staging, generationPath))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("external projection mode = %v, err=%v", info.Mode().Perm(), err)
	}
	runtime, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runtime), projectionPath) {
		t.Fatal("generated runtime does not reference external projection")
	}
	if strings.Contains(string(runtime), "postgres://") || strings.Contains(string(runtime), "must-not-reach-workload") {
		t.Fatal("external credential value leaked into generated runtime")
	}
	if strings.Contains(string(runtime), "depends_on:\n      database:") {
		t.Fatal("external service was emitted as a nonexistent Compose dependency")
	}
}
