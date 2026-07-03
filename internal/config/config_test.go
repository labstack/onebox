package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	// ready timing defaults applied by Validate
	if time.Duration(cfg.Roles["web"].Ready.Within) != 120*time.Second {
		t.Fatalf("within default: %v", cfg.Roles["web"].Ready.Within)
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
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: rolling role requires ready http|exec")
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
