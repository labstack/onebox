package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/config"
)

// env_files feed ${VAR} interpolation (later files win), like a hand-assembled
// .env used to — without any service `env_file:` in the compose file.
func TestEnvFilesInterpolation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "base.env"), []byte("TAG=old\nKEEP=yes\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "prod.env"), []byte("TAG=new\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(`
services:
  s:
    image: app:${TAG:?required}
    environment:
      K: ${KEEP:?required}
`), 0o644)
	p, err := Load(context.Background(), filepath.Join(dir, "docker-compose.yaml"), "d",
		filepath.Join(dir, "base.env"), filepath.Join(dir, "prod.env"))
	if err != nil {
		t.Fatalf("env_files interpolation broken: %v", err)
	}
	if got := p.Services["s"].Image; got != "app:new" {
		t.Fatalf("later env file must win: image=%s", got)
	}
	if v := p.Services["s"].Environment["K"]; v == nil || *v != "yes" {
		t.Fatalf("K=%v", v)
	}
}

// InjectEnvFiles attaches env_files to roles + jobs (not accessories) as
// env_file refs, which then stage as payload without their secret contents
// leaking into the rendered compose.
func TestInjectEnvFilesTargetsRolesAndJobs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.env"), []byte("SECRET=leaky-xyz\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(`
services:
  web:      { image: x }
  migrate:  { image: x, command: migrate }
  postgres: { image: x }
`), 0o644)
	p, err := Load(context.Background(), filepath.Join(dir, "docker-compose.yaml"), "d")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Roles:    map[string]config.Role{"web": {Service: "web"}},
		Jobs:     []string{"migrate"},
		EnvFiles: []string{"app.env"},
	}
	InjectEnvFiles(p, cfg)

	want := filepath.Join(dir, "app.env")
	if got := p.Services["web"].EnvFiles; len(got) != 1 || got[0].Path != want {
		t.Fatalf("web env_file: %v", got)
	}
	if got := p.Services["migrate"].EnvFiles; len(got) != 1 || got[0].Path != want {
		t.Fatalf("migrate env_file: %v", got)
	}
	if got := p.Services["postgres"].EnvFiles; len(got) != 0 {
		t.Fatalf("accessory must not get env_files: %v", got)
	}

	// The injected file stages as payload and its secret never renders inline.
	staging := t.TempDir()
	rewrites, err := StagePayload(p, staging)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, "app.env")); err != nil {
		t.Fatalf("env_file not staged: %v", err)
	}
	rendered, err := p.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	rendered = RewriteSources(rendered, rewrites)
	if strings.Contains(string(rendered), "leaky-xyz") {
		t.Fatalf("secret leaked into rendered compose:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), "app.env") {
		t.Fatalf("env_file ref dropped:\n%s", rendered)
	}
}

// Injection order encodes precedence: env_files first, then the secrets file,
// so compose (later env_file wins) lets a decrypted secret override a
// same-named plaintext key. This is the order stageRelease calls them in.
func TestSecretsEnvWinsOverEnvFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.env"), []byte("K=plain\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(`
services:
  web: { image: x }
`), 0o644)
	p, err := Load(context.Background(), filepath.Join(dir, "docker-compose.yaml"), "d")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Roles:    map[string]config.Role{"web": {Service: "web"}},
		EnvFiles: []string{"app.env"},
	}
	InjectEnvFiles(p, cfg)
	InjectSecretsEnv(p, cfg, "./.ob-secrets.env")

	ef := p.Services["web"].EnvFiles
	if len(ef) == 0 || ef[len(ef)-1].Path != "./.ob-secrets.env" {
		t.Fatalf("secrets file must be the last env_file (authoritative): %+v", ef)
	}
}
