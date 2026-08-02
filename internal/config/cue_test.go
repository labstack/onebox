package config

import (
	"strings"
	"testing"
)

const cueSample = `
api_version: onebox.run/v1
app: sample
compose: docker-compose.yaml
environments:
  production: { target: deploy@app.example.com }
components:
  web: { type: application, service: server, deployment: { strategy: rolling }, readiness: { http: /healthz, port: 7500 } }
  postgres: { type: postgres, persistence: { mode: durable } }
  migrate:
    type: job
    data_effect: migration
    command: docker compose run --rm --no-deps migrate
deployment: { order: [web] }
hooks:
  post_deploy: { run: "rsync -az web/dist/ host:/data/web/", local: true }
verification:
  - { http: /healthz, component: web }
  - { url: "https://app.example.com/", contains: 'id="root"', advisory: true }
registry: { server: ghcr.io, username: vishr, password_env: GHCR_TOKEN }
`

func TestCUEAcceptsValidConfig(t *testing.T) {
	if err := ValidateCUE([]byte(cueSample), "ob.yml"); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestCUERejectsTypoField(t *testing.T) {
	bad := strings.Replace(cueSample, "deployment: { order:", "deployment: { orderz:", 1)
	err := ValidateCUE([]byte(bad), "ob.yml")
	if err == nil {
		t.Fatal("typo'd field must be rejected")
	}
	if !strings.Contains(err.Error(), "orderz") || !strings.Contains(err.Error(), "ob.yml:") {
		t.Fatalf("error should name the field with file:line position: %v", err)
	}
}

// The perf knobs (ready.retries, drain.grace) must be accepted by the CUE
// schema — CUE structs are closed (see TestCUERejectsTypoField), so an omitted
// field would make the whole knob unreachable through a real ob.yml even though
// the Go structs and render/roll wiring exist.
func TestCUEAcceptsTimingKnobs(t *testing.T) {
	cfg := strings.Replace(cueSample,
		"readiness: { http: /healthz, port: 7500 }",
		"readiness: { http: /healthz, port: 7500, retries: 1 }, drain: { grace: 8s }", 1)
	if err := ValidateCUE([]byte(cfg), "ob.yml"); err != nil {
		t.Fatalf("ready.retries / drain.grace must validate: %v", err)
	}
}

func TestCUERejectsBadMode(t *testing.T) {
	bad := strings.Replace(cueSample, "strategy: rolling", "strategy: sideways", 1)
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
	if h := cfg.Hooks["post_deploy"]; !h.Local || !strings.Contains(h.Run, "rsync") {
		t.Fatalf("map hook: %+v", h)
	}
	if cfg.Registry == nil || cfg.Registry.PasswordEnv != "GHCR_TOKEN" {
		t.Fatalf("registry: %+v", cfg.Registry)
	}
	if !cfg.Verify[1].Advisory || cfg.Verify[1].URL == "" {
		t.Fatalf("advisory verify: %+v", cfg.Verify[1])
	}
}
