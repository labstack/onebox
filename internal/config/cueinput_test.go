package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cueConfig = `
package ob

let HOST = "root@monk.labstack.net"

api_version: "onebox.run/v1"
app:     "monk"
compose: "docker-compose.yaml"

environments: production: target: HOST

components: {
	web: {
		type:       "application"
		service:    "server"
		deployment: strategy: "rolling"
		readiness: {http: "/healthz", port: 7500}
	}
	worker: {type: "worker", service: "worker", deployment: strategy: "recreate"}
	postgres: {type: "postgres", persistence: mode: "durable"}
}
deployment: order: ["web", "worker"]
hooks: post_deploy: {run: "rsync dist/ \(HOST):/data/web/", local: true}
`

func TestLoadCUEConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ob.cue")
	if err := os.WriteFile(p, []byte(cueConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.App != "monk" || cfg.Roles["web"].Ready.Port != 7500 {
		t.Fatalf("cue config decoded wrong: %+v", cfg)
	}
	// let-binding interpolation resolved
	if cfg.Environments["production"].Hosts[0] != "root@monk.labstack.net" {
		t.Fatalf("hosts: %+v", cfg.Environments["production"])
	}
	if !strings.Contains(cfg.Hooks["post_deploy"].Run, "root@monk.labstack.net:/data/web/") {
		t.Fatalf("interpolation not resolved: %+v", cfg.Hooks["post_deploy"])
	}
}

func TestLoadCUERejectsSchemaViolation(t *testing.T) {
	bad := strings.Replace(cueConfig, `strategy: "rolling"`, `strategy: "sideways"`, 1)
	p := filepath.Join(t.TempDir(), "ob.cue")
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("cue config must be schema-checked too")
	}
}
