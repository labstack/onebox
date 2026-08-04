package onebox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

const testSecret = "mcp-must-not-see-this"

func writeServiceProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"app.env": "RUNTIME_SECRET=initial-value\n",
		"docker-compose.yaml": `
services:
  database:
    image: ghcr.io/example/postgres:` + testSecret + `
`,
		"ob.yml": `
api_version: onebox.run/v1
app: demo
environments:
  production:
    server: deploy@example.invalid
    policy:
      require_approval: true
      allow_agent_proposals: true
workloads:
  web:
    role: application
    image: ghcr.io/example/app:v1
    strategy: rolling
    health: { http: /healthz, port: 8080 }
    env: { SECRET_TOKEN: "` + testSecret + `" }
  database:
    role: daemon
    compose: "docker-compose.yaml#database"
    persistence: { mode: durable }
    volumes: [{ name: data, path: /var/lib/postgresql/data }]
deployment:
  order: [web]
  retain_releases: 5
  migration_policy: manual
runtime:
  env_files: [app.env]
hooks:
  post_deploy: "echo ` + testSecret + `"
verification:
  - { url: "https://example.invalid/private/` + testSecret + `?token=` + testSecret + `", advisory: true }
  - { workload: web, http: "/private/` + testSecret + `" }
observability:
  logs: { enabled: true, retention_days: 14 }
  metrics: { enabled: true }
  alerts: { unhealthy_after: 5m }
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "ob.yml")
}

func serviceFake() *transport.Fake {
	digest := "sha256:" + strings.Repeat("ab", 32)
	return &transport.Fake{
		HostName:   "example.invalid",
		TargetName: "deploy@example.invalid",
		Dynamic: func(cmd string) (transport.Result, bool) {
			switch {
			case strings.Contains(cmd, "readlink"):
				return transport.Result{Stdout: "releases/R0\n"}, true
			case strings.Contains(cmd, "docker ps") && strings.Contains(cmd, "--format"):
				return transport.Result{Stdout: "S1|web|R0|Up (healthy)\nPG1|database|R0|Up (healthy)\n"}, true
			case strings.Contains(cmd, "for f in"):
				return transport.Result{}, true
			case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "'web'"):
				return transport.Result{Stdout: "S1\n"}, true
			case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "'database'"):
				return transport.Result{Stdout: "PG1\n"}, true
			// Health first: an inspect that asks for health must not fall
			// through to the image matcher and report a digest as a health
			// state.
			case strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "Health"):
				return transport.Result{Stdout: "healthy\n"}, true
			case strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "{{.Image}}"):
				return transport.Result{Stdout: "sha256:" + strings.Repeat("ef", 32) + "\n"}, true
			case strings.Contains(cmd, "docker buildx imagetools inspect"):
				return transport.Result{Stdout: digest + "\n"}, true
			case strings.Contains(cmd, "cat ") && strings.Contains(cmd, "compose.yaml"):
				return transport.Result{Stdout: "services:\n  web:\n    image: ghcr.io/example/app:v0\n    environment:\n      SECRET_TOKEN: live-secret\n"}, true
			case strings.Contains(cmd, "find . -type f"):
				return transport.Result{Stdout: strings.Repeat("cd", 32) + "\n"}, true
			}
			return transport.Result{}, false
		},
	}
}

func newTestService(t *testing.T, f *transport.Fake) *Service {
	t.Helper()
	base := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	tick := 0
	return New(Options{
		ConfigPath: writeServiceProject(t),
		Now: func() time.Time {
			tick++
			return base.Add(time.Duration(tick) * time.Minute)
		},
		Connect: func(_ context.Context, target string) (transport.Transport, error) {
			if target != "deploy@example.invalid" {
				t.Fatalf("connector target = %q", target)
			}
			return f, nil
		},
	})
}

func jobOnlyProject(t *testing.T, hooks map[string]app.Command) *app.Resolved {
	t.Helper()
	spec, err := app.LoadBytes([]byte(`
api_version: onebox.run/v1
app: sample
environments: {production: {server: root@h}}
workloads:
  web:     {role: application, image: x:1}
  migrate: {role: job, image: x:1, data_effect: migration}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	resolved.Hooks = hooks
	return resolved
}
