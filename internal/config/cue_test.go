package config

import (
	"strings"
	"testing"
)

const cueSample = `
app: monk
compose: docker-compose.yaml
environments:
  production: { hosts: [deploy@monk.labstack.net] }
roles:
  web: { service: server, mode: rolling, ready: { http: /healthz, port: 7500 } }
order: [web]
accessories: [postgres]
jobs: [migrate]
hooks:
  migrate: docker compose run --rm --no-deps migrate
  publish_web: { run: "rsync -az web/dist/ host:/data/web/", local: true }
verify:
  - { http: /healthz, role: web }
  - { url: "https://monk.trade/", contains: 'id="root"', advisory: true }
registry: { server: ghcr.io, username: vishr, password_env: GHCR_TOKEN }
`

func TestCUEAcceptsValidConfig(t *testing.T) {
	if err := ValidateCUE([]byte(cueSample), "ob.yml"); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestCUERejectsTypoField(t *testing.T) {
	bad := strings.Replace(cueSample, "order:", "orderz:", 1)
	err := ValidateCUE([]byte(bad), "ob.yml")
	if err == nil {
		t.Fatal("typo'd field must be rejected")
	}
	if !strings.Contains(err.Error(), "orderz") || !strings.Contains(err.Error(), "ob.yml:") {
		t.Fatalf("error should name the field with file:line position: %v", err)
	}
}

func TestCUERejectsBadMode(t *testing.T) {
	bad := strings.Replace(cueSample, "mode: rolling", "mode: sideways", 1)
	err := ValidateCUE([]byte(bad), "ob.yml")
	if err == nil {
		t.Fatal("bad mode must be rejected")
	}
	if !strings.Contains(err.Error(), "recreate") {
		t.Fatalf("error should mention allowed values: %v", err)
	}
}

func TestCUERejectsBadDuration(t *testing.T) {
	bad := strings.Replace(cueSample, "port: 7500", "port: 7500, within: soon", 1)
	if err := ValidateCUE([]byte(bad), "ob.yml"); err == nil {
		t.Fatal("bad duration must be rejected")
	}
}

func TestHookUnmarshalForms(t *testing.T) {
	cfg, err := LoadBytes([]byte(cueSample), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if h := cfg.Hooks["migrate"]; h.Run == "" || h.Local {
		t.Fatalf("string hook: %+v", h)
	}
	if h := cfg.Hooks["publish_web"]; !h.Local || !strings.Contains(h.Run, "rsync") {
		t.Fatalf("map hook: %+v", h)
	}
	if cfg.Registry == nil || cfg.Registry.PasswordEnv != "GHCR_TOKEN" {
		t.Fatalf("registry: %+v", cfg.Registry)
	}
	if !cfg.Verify[1].Advisory || cfg.Verify[1].URL == "" {
		t.Fatalf("advisory verify: %+v", cfg.Verify[1])
	}
}
