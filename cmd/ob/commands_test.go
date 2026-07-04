package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/config"
)

func TestConfirmDefaultsToNo(t *testing.T) {
	cases := map[string]bool{
		"y\n": true, "yes\n": true, "Y\n": true, "YES\n": true,
		"n\n": false, "no\n": false, "\n": false, "": false, "garbage\n": false,
	}
	for in, want := range cases {
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader(in))
		cmd.SetOut(&bytes.Buffer{})
		if got := confirm(cmd, "?"); got != want {
			t.Fatalf("confirm(%q) = %v, want %v", in, got, want)
		}
	}
}

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
	obYAML := `
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
	if err := os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(obYAML), 0o644); err != nil {
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
	cmd.SetArgs(append([]string{"-c", filepath.Join(dir, "ob.yml")}, args...))
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
	if !strings.Contains(out, "/tmp/ob-drain") || !strings.Contains(out, "ob.release") {
		t.Fatalf("render missing injections:\n%s", out)
	}
}

// render must never print interpolated secret values — a secret referenced
// inline in environment is redacted to a content hash (design §07).
func TestRenderRedactsEnvSecrets(t *testing.T) {
	dir := writeProject(t)
	composeYAML := `
services:
  server:
    image: ghcr.io/x/app:v1
    environment:
      STRIPE_SECRET_KEY: ${STRIPE_SECRET_KEY:-sk_live_TOPSECRET}
  postgres:
    image: postgres:17
`
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(composeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dir, "render")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Contains(out, "sk_live_TOPSECRET") {
		t.Fatalf("render leaked a secret value:\n%s", out)
	}
	if !strings.Contains(out, "STRIPE_SECRET_KEY") || !strings.Contains(out, "redacted:sha256:") {
		t.Fatalf("render should keep the key and show a hash:\n%s", out)
	}
}

// env_files render onto role AND job services as env_file refs (container
// runtime env), while their secret contents never appear in the output.
func TestRenderInjectsEnvFiles(t *testing.T) {
	dir := writeProject(t)
	composeYAML := `
services:
  server:
    image: ghcr.io/x/app:v1
  migrate:
    image: ghcr.io/x/app:v1
    command: migrate
  postgres:
    image: postgres:17
`
	obYAML := `
app: demo
compose: docker-compose.yaml
environments: { production: { hosts: [deploy@example.invalid] } }
roles:
  web: { service: server, mode: rolling, ready: { http: /healthz, port: 8080 } }
order: [web]
accessories: [postgres]
jobs: [migrate]
env_files: [app.env]
`
	for name, body := range map[string]string{
		"docker-compose.yaml": composeYAML,
		"ob.yml":              obYAML,
		"app.env":             "SECRET=leaky-xyz\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := run(t, dir, "render")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Contains(out, "leaky-xyz") {
		t.Fatalf("env_file secret leaked into render:\n%s", out)
	}
	if strings.Count(out, "app.env") < 2 {
		t.Fatalf("app.env must attach to both role and job:\n%s", out)
	}
}

// A preflight check with a missing key halts the deploy before any host
// contact, naming the file and key.
func TestPreflightBlocksDeploy(t *testing.T) {
	dir := writeProject(t)
	obYAML := `
app: demo
compose: docker-compose.yaml
environments: { production: { hosts: [deploy@example.invalid] } }
roles:
  web: { service: server, mode: rolling, ready: { http: /healthz, port: 8080 } }
order: [web]
accessories: [postgres]
preflight:
  - { file: secrets.env, require: [MISSING_KEY] }
`
	if err := os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(obYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("PRESENT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dir, "deploy", "-y")
	if err == nil {
		t.Fatalf("preflight should have blocked the deploy: %s", out)
	}
	if !strings.Contains(err.Error()+out, "MISSING_KEY") || !strings.Contains(err.Error()+out, "secrets.env") {
		t.Fatalf("error must name file and key: %v\n%s", err, out)
	}
}

// An env_files entry that resolves outside the project must be rejected at load
// (it could never ship with the release) — caught by validate, before deploy.
func TestEnvFilesOutsideProjectRejected(t *testing.T) {
	dir := writeProject(t)
	obYAML := `
app: demo
compose: docker-compose.yaml
environments: { production: { hosts: [deploy@example.invalid] } }
roles:
  web: { service: server, mode: rolling, ready: { http: /healthz, port: 8080 } }
order: [web]
accessories: [postgres]
env_files: ["../escapes.env"]
`
	if err := os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(obYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dir, "validate")
	if err == nil {
		t.Fatalf("expected rejection of out-of-project env_files: %s", out)
	}
	if !strings.Contains(err.Error()+out, "outside the project") {
		t.Fatalf("error should explain the constraint: %v\n%s", err, out)
	}
}

func TestNotifyOutcome(t *testing.T) {
	var got map[string]any
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
	}))
	defer srv.Close()

	cfg := &config.Config{
		App:          "monk",
		Environments: map[string]config.Environment{"production": {Hosts: []string{"root@h"}}},
		Notify:       &config.Notify{Webhook: srv.URL, On: []string{"failure"}},
	}
	g := &globalFlags{Env: "production"}

	// success filtered by on: [failure]
	notifyOutcome(cfg, g, "deploy", "R1", nil)
	if hits != 0 {
		t.Fatal("success must be filtered")
	}
	// failure fires with the verb, host, and error
	notifyOutcome(cfg, g, "deploy", "R1", fmt.Errorf("verify: HALT-AND-PAGE"))
	if hits != 1 {
		t.Fatal("failure must fire")
	}
	if got["verb"] != "deploy" || got["deploy_id"] != "R1" || got["host"] != "root@h" {
		t.Fatalf("payload: %v", got)
	}
	if !strings.Contains(got["text"].(string), "HALT-AND-PAGE") {
		t.Fatalf("text: %v", got["text"])
	}
	// nil notify config: silent no-op
	cfg.Notify = nil
	notifyOutcome(cfg, g, "deploy", "R1", fmt.Errorf("x"))
	if hits != 1 {
		t.Fatal("nil notify must be a no-op")
	}
}
