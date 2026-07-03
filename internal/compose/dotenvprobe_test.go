package compose

import (
	"context"
	"os"
	"path/filepath"
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
