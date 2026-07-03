package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	composeYAML := `
services:
  server:
    image: ghcr.io/x/app:v1
  postgres:
    image: postgres:17
`
	yeetYAML := `
app: demo
compose: docker-compose.yaml
environments: { production: { hosts: [deploy@example.invalid] } }
roles:
  web: { service: server, mode: rolling, ready: { http: /healthz, port: 8080 } }
order: [web]
accessories: [postgres]
`
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(composeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "yeet.yml"), []byte(yeetYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func run(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"-c", filepath.Join(dir, "yeet.yml")}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestValidateOK(t *testing.T) {
	dir := writeProject(t)
	out, err := run(t, dir, "validate")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("want ok, got: %s", out)
	}
}

func TestValidateCatchesRollabilityViolation(t *testing.T) {
	dir := writeProject(t)
	bad := `
services:
  server:
    image: ghcr.io/x/app:v1
    container_name: pinned
  postgres:
    image: postgres:17
`
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dir, "validate")
	if err == nil {
		t.Fatalf("expected rollability error, got: %s", out)
	}
	if !strings.Contains(out, "container_name") {
		t.Fatalf("error should name container_name: %s", out)
	}
}

func TestRenderInjectsDrainGuard(t *testing.T) {
	dir := writeProject(t)
	out, err := run(t, dir, "render")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "/tmp/yeet-drain") || !strings.Contains(out, "yeet.release") {
		t.Fatalf("render missing injections:\n%s", out)
	}
}
