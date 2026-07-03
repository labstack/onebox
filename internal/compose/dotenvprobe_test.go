package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDotEnvInterpolation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("MYVAR=hello\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(`
services:
  s:
    image: x
    environment:
      V: ${MYVAR:?required}
`), 0o644)
	p, err := Load(context.Background(), filepath.Join(dir, "docker-compose.yaml"), "d")
	if err != nil {
		t.Fatalf("dotenv interpolation broken: %v", err)
	}
	if v := p.Services["s"].Environment["V"]; v == nil || *v != "hello" {
		t.Fatalf("V=%v", v)
	}
}

// env_file contents must NOT be folded into the rendered compose — that would
// inline the whole secret file into the plan diff/artifact (design §07). The
// reference is kept so `docker compose` reads it at runtime on the host.
func TestEnvFileNotFoldedIntoRender(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET_TOKEN=leaky-value-123\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(`
services:
  s:
    image: x
    env_file: .env
`), 0o644)
	p, err := Load(context.Background(), filepath.Join(dir, "docker-compose.yaml"), "d")
	if err != nil {
		t.Fatal(err)
	}
	if v := p.Services["s"].Environment["SECRET_TOKEN"]; v != nil {
		t.Fatalf("env_file was folded into environment: SECRET_TOKEN=%v", *v)
	}
	rendered, err := p.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "leaky-value-123") {
		t.Fatalf("env_file secret leaked into rendered compose:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), ".env") {
		t.Fatalf("env_file reference was dropped — host runtime can't resolve it:\n%s", rendered)
	}
}
