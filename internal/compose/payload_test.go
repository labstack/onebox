package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/config"
)

func TestStagePayloadRewritesProjectRelativeSources(t *testing.T) {
	p, err := Load(context.Background(), "testdata/payload/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		App:   "demo",
		Roles: map[string]config.Role{"app": {Service: "server", Mode: "recreate"}},
	}
	staging := t.TempDir()
	rewrites, err := StagePayload(p, staging)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(p, cfg, "R1")
	if err != nil {
		t.Fatal(err)
	}
	rendered = RewriteSources(rendered, rewrites)
	out := string(rendered)

	// project-relative bind mount: staged + rewritten
	if _, err := os.Stat(filepath.Join(staging, "conf", "app.conf")); err != nil {
		t.Fatalf("conf/app.conf not staged: %v", err)
	}
	if !strings.Contains(out, "./conf/app.conf") {
		t.Fatalf("bind source not rewritten to release-relative:\n%s", out)
	}
	if strings.Contains(out, "testdata/payload/conf") {
		t.Fatalf("absolute runner path leaked into rendered compose:\n%s", out)
	}
	// host-absolute mounts untouched
	if !strings.Contains(out, "/var/run/docker.sock") {
		t.Fatalf("host-absolute mount must stay:\n%s", out)
	}
	// env_file: either folded into environment (values present) or staged+rewritten
	if strings.Contains(out, "env_file") {
		if _, err := os.Stat(filepath.Join(staging, ".env")); err != nil {
			t.Fatalf("env_file survived render but .env not staged: %v", err)
		}
		if strings.Contains(out, "testdata/payload/.env") {
			t.Fatalf("env_file path not rewritten:\n%s", out)
		}
	} else if !strings.Contains(out, "FOO") {
		t.Fatalf("env_file neither preserved nor folded into environment:\n%s", out)
	}
}
