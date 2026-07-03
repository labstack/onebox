package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cueConfig = `
package yeet

let HOST = "root@monk.labstack.net"

app:     "monk"
compose: "docker-compose.yaml"

environments: production: hosts: [HOST]

roles: {
	web: {
		service: "server"
		mode:    "rolling"
		ready: {http: "/healthz", port: 7500}
	}
	worker: {service: "worker", mode: "recreate"}
}
order: ["web", "worker"]
accessories: ["postgres"]
hooks: publish: {run: "rsync dist/ \(HOST):/data/web/", local: true}
`

func TestLoadCUEConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "yeet.cue")
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
	if !strings.Contains(cfg.Hooks["publish"].Run, "root@monk.labstack.net:/data/web/") {
		t.Fatalf("interpolation not resolved: %+v", cfg.Hooks["publish"])
	}
}

func TestLoadCUERejectsSchemaViolation(t *testing.T) {
	bad := strings.Replace(cueConfig, `mode:    "rolling"`, `mode:    "sideways"`, 1)
	p := filepath.Join(t.TempDir(), "yeet.cue")
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("cue config must be schema-checked too")
	}
}
