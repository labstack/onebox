package main

import (
	"bytes"
	"context"
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

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/onebox"
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

func TestConfirmInteractiveDeployRequiresConfirmationWithoutPolicyApproval(t *testing.T) {
	plan := &onebox.DeployPlan{Operation: onebox.OperationPlan{Approval: onebox.ApprovalNone}}
	for _, tt := range []struct {
		name  string
		input string
		want  bool
	}{
		{name: "defaults to no", input: "", want: false},
		{name: "explicit yes", input: "yes\n", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetIn(strings.NewReader(tt.input))
			cmd.SetOut(&out)
			if got := confirmInteractiveDeploy(cmd, plan); got != tt.want {
				t.Fatalf("confirmation = %v, want %v", got, tt.want)
			}
			if !strings.Contains(out.String(), "Apply this plan? [y/N]") {
				t.Fatalf("generic deploy confirmation missing: %q", out.String())
			}
		})
	}
}

func writeProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	obYAML := `
api_version: onebox.run/v1
app: demo
environments: { production: { server: deploy@example.invalid } }
workloads:
  web:      { role: application, image: ghcr.io/x/app:v1, health: { http: /healthz, port: 8080 } }
  postgres: { role: daemon, image: postgres:17, persistence: { mode: durable } }
deployment: { order: [web] }
`
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

func TestPreflightBlocksDeploy(t *testing.T) {
	dir := writeProject(t)
	obYAML := `
api_version: onebox.run/v1
app: demo
environments: { production: { server: deploy@example.invalid } }
workloads:
  web:      { role: application, image: ghcr.io/x/app:v1, health: { http: /healthz, port: 8080 } }
  postgres: { role: daemon, image: postgres:17, persistence: { mode: durable } }
deployment: { order: [web] }
runtime:
  env_checks:
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
func TestNotifyOutcome(t *testing.T) {
	var got map[string]any
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
	}))
	defer srv.Close()

	cfg := &app.Resolved{Spec: &app.Spec{
		Name:          "sample",
		Environments:  map[string]app.Environment{"production": {Server: app.Server{User: "root", Host: "h"}}},
		Notifications: map[string]app.Notification{"ops": {Webhook: srv.URL, On: []string{"failure"}}},
	}}
	g := &globalFlags{Env: "production"}

	// success filtered by on: [failure]
	notifyOutcome(t.Context(), cfg, g, "deploy", "R1", nil)
	if hits != 0 {
		t.Fatal("success must be filtered")
	}
	// failure fires with the verb and host, but detailed errors never leave the
	// trusted local path.
	notifyOutcome(t.Context(), cfg, g, "deploy", "R1", fmt.Errorf("verify: Authorization=Bearer super-secret"))
	if hits != 1 {
		t.Fatal("failure must fire")
	}
	if got["verb"] != "deploy" || got["deploy_id"] != "R1" || got["host"] != "root@h" {
		t.Fatalf("payload: %v", got)
	}
	if text := got["text"].(string); strings.Contains(text, "super-secret") || !strings.Contains(text, "inspect trusted local diagnostics") {
		t.Fatalf("notification text was not safely redacted: %v", text)
	}
	// A saved-plan no-op must not claim that its unactivated release succeeded.
	cfg.Notifications["ops"] = app.Notification{Webhook: cfg.Notifications["ops"].Webhook, On: []string{"success", "failure"}}
	notifyOperationOutcome(t.Context(), cfg, g, "deploy", onebox.OperationResult{
		Status: "no_op", NoOp: true, ReleaseID: "UNACTIVATED",
	}, nil)
	if hits != 1 {
		t.Fatal("no-op must not notify as a successful deploy")
	}
	// Ctrl-C cancels the operation context before the failure outcome is sent.
	// The webhook still gets one bounded attempt under notify's own timeout.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	notifyOperationOutcome(cancelled, cfg, g, "deploy", onebox.OperationResult{EvidenceID: "cancelled"}, context.Canceled)
	if hits != 2 {
		t.Fatal("cancelled operation context suppressed its failure notification")
	}
	// no notification declared: silent no-op
	cfg.Notifications = nil
	notifyOutcome(t.Context(), cfg, g, "deploy", "R1", fmt.Errorf("x"))
	if hits != 2 {
		t.Fatal("nil notify must be a no-op")
	}
}
