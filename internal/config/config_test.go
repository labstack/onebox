package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/yeet/internal/proxy"
)

const sample = `
app: monk
compose: docker-compose.yaml
environments:
  production: { hosts: [deploy@monk.labstack.net] }
roles:
  web:    { service: server, mode: rolling, ready: { http: /healthz, port: 7500 } }
  worker: { service: worker, mode: recreate, drain: { signal: TERM, wait: 30s } }
order: [web, worker]
accessories: [postgres, redis, traefik]
jobs: [migrate]
hooks: { migrate: docker compose run --rm --no-deps migrate }
verify:
  - { http: /healthz, role: web }
`

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "yeet.yml")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Roles["web"].Ready.HTTP != "/healthz" || cfg.Roles["web"].Ready.Port != 7500 {
		t.Fatalf("ready parsed wrong: %+v", cfg.Roles["web"].Ready)
	}
	if time.Duration(cfg.Roles["worker"].Drain.Wait) != 30*time.Second {
		t.Fatalf("drain wait: %v", cfg.Roles["worker"].Drain.Wait)
	}
	env, err := cfg.Environment("production")
	if err != nil || env.Hosts[0] != "deploy@monk.labstack.net" {
		t.Fatalf("env: %+v err=%v", env, err)
	}
	if cfg.Retain != 5 { // default
		t.Fatalf("retain default: %d", cfg.Retain)
	}
	// ready timing has NO config-level defaults — they'd stomp adopted
	// compose-healthcheck timings; defaults live at the point of use
	if cfg.Roles["web"].Ready.Within != 0 {
		t.Fatalf("within must stay unset: %v", cfg.Roles["web"].Ready.Within)
	}
}

func TestValidateRejects(t *testing.T) {
	badMode := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: sideways } }
order: [web]
`
	// shape errors are now caught by CUE at Load time
	if _, err := Load(write(t, badMode)); err == nil {
		t.Fatal("expected CUE error for mode 'sideways'")
	}

	// rolling without ready is legal at config level — the readiness contract
	// may be ADOPTED from the compose healthcheck; compose.CheckRollable
	// enforces the cross-file rule
	noReady := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: rolling } }
order: [web]
`
	cfg, err := Load(write(t, noReady))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("rolling without ready must pass config validation (adopt path): %v", err)
	}

	orderGap := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles:
  web: { service: server, mode: recreate }
  bg:  { service: bg, mode: recreate }
order: [web]
`
	cfg, err = Load(write(t, orderGap))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: order must cover every role")
	}

	badSignal := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: recreate, drain: { signal: "TERM; rm -rf /", wait: 1s } } }
order: [web]
`
	if _, err := Load(write(t, badSignal)); err == nil {
		t.Fatal("expected CUE error: signal must be [A-Z0-9]+")
	}

	badRole := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { "web$(x)": { service: server, mode: recreate } }
order: ["web$(x)"]
`
	if _, err := Load(write(t, badRole)); err == nil {
		t.Fatal("expected CUE error: role name must be identifier-safe")
	}
}

func TestEnvFilesParse(t *testing.T) {
	cfg, err := Load(write(t, sample+"env_files: [server/.env, server/.env.production]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EnvFiles; len(got) != 2 || got[0] != "server/.env" || got[1] != "server/.env.production" {
		t.Fatalf("env_files parsed wrong: %v", got)
	}
}

func TestRunPreflight(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.env"),
		[]byte("TOKEN=abc\nexport EXPORTED=yes\nVAPID=\nEMPTY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := &Config{Preflight: []PreflightCheck{
		{File: "app.env", Require: []string{"TOKEN", "EXPORTED"}, Present: []string{"VAPID"}},
	}}
	if err := base.RunPreflight(dir); err != nil {
		t.Fatalf("preflight should pass: %v", err)
	}

	missingFile := &Config{Preflight: []PreflightCheck{{File: "nope.env", Require: []string{"X"}}}}
	if err := missingFile.RunPreflight(dir); err == nil {
		t.Fatal("expected missing-file error")
	}

	missingKey := &Config{Preflight: []PreflightCheck{{File: "app.env", Require: []string{"ABSENT"}}}}
	if err := missingKey.RunPreflight(dir); err == nil {
		t.Fatal("expected missing-key error")
	}

	emptyRequired := &Config{Preflight: []PreflightCheck{{File: "app.env", Require: []string{"EMPTY"}}}}
	if err := emptyRequired.RunPreflight(dir); err == nil {
		t.Fatal("expected empty-required-value error")
	}

	// Present tolerates an empty value (the VAPID case).
	presentEmpty := &Config{Preflight: []PreflightCheck{{File: "app.env", Present: []string{"EMPTY"}}}}
	if err := presentEmpty.RunPreflight(dir); err != nil {
		t.Fatalf("present should tolerate empty value: %v", err)
	}
}

func TestReplicasParseAndCount(t *testing.T) {
	cfg, err := Load(write(t, strings.ReplaceAll(sample,
		"web:    { service: server, mode: rolling, ready: { http: /healthz, port: 7500 } }",
		"web:    { service: server, mode: rolling, replicas: 3, ready: { http: /healthz, port: 7500 } }")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Roles["web"].Replicas; got != 3 {
		t.Fatalf("replicas parsed wrong: %d", got)
	}
	if got := cfg.Roles["web"].Count(); got != 3 {
		t.Fatalf("Count() = %d, want 3", got)
	}
	// absent → defaults to 1
	if got := cfg.Roles["worker"].Count(); got != 1 {
		t.Fatalf("default Count() = %d, want 1", got)
	}
}

func TestProxyManaged(t *testing.T) {
	managed := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: recreate } }
order: [web]
proxy: { managed: true, config: traefik }
`
	cfg, err := Load(write(t, managed))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("managed proxy with config dir must validate: %v", err)
	}
	if cfg.Proxy.Kind != "traefik-docker" {
		t.Fatalf("kind must default to traefik-docker when managed, got %q", cfg.Proxy.Kind)
	}

	noConfig := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: recreate } }
order: [web]
proxy: { managed: true }
`
	cfg, err = Load(write(t, noConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "proxy.config") {
		t.Fatalf("managed without config must fail naming proxy.config, got %v", err)
	}

	kindNone := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: recreate } }
order: [web]
proxy: { kind: none, managed: true, config: traefik }
`
	cfg, err = Load(write(t, kindNone))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("managed with kind none must fail")
	}

	// external stays legal and unconstrained
	external := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: recreate } }
order: [web]
proxy: { kind: traefik-docker, managed: false }
`
	cfg, err = Load(write(t, external))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("external proxy must validate: %v", err)
	}

	badKind := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: recreate } }
order: [web]
proxy: { kind: nginx, managed: true, config: traefik }
`
	if _, err := Load(write(t, badKind)); err == nil {
		t.Fatal("expected CUE error: kind must be traefik-docker|none")
	}
}

func TestAppNameYeetProxyReserved(t *testing.T) {
	reserved := `
app: yeet-proxy
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: recreate } }
order: [web]
`
	cfg, err := Load(write(t, reserved))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("app name yeet-proxy must be reserved, got %v", err)
	}
}

// Drift guard: Validate's reserved-name literal must track proxy.Project —
// test-level coupling keeps internal/config a leaf package.
func TestReservedAppNameTracksProxyProject(t *testing.T) {
	if proxy.Project != "yeet-proxy" {
		t.Fatalf("proxy.Project changed to %q — update the reserved-name check in config.Validate", proxy.Project)
	}
}
