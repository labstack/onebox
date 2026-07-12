package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const monkShaped = `
services:
  traefik:
    image: traefik:v3.7
    ports: ["80:80", "443:443"]
  postgres:
    image: ghcr.io/x/monk-postgres:1
    ports: ["127.0.0.1:5432:5432"]
  mysql:
    image: mariadb:11
  redis:
    image: redis:8-alpine
  migrate:
    image: ghcr.io/x/app:v1
    command: alembic upgrade head
    profiles: [jobs]
  server:
    image: ghcr.io/x/app:v1
    container_name: pinned-server
    healthcheck: { test: ["CMD", "curl", "-f", "http://localhost:7500/healthz"] }
    labels: [traefik.enable=true]
  worker:
    image: ghcr.io/x/app:v1
    command: taskiq worker
`

func TestInitClassifiesAndDoctors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(monkShaped), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dir, "init")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ob.yml"))
	if err != nil {
		t.Fatal(err)
	}
	y := string(b)
	for _, want := range []string{
		"api_version: onebox.run/v1",
		"target: deploy@CHANGE-ME",
		"components:",
		"type: service",
		"type: postgres",
		"type: mysql",
		"type: redis",
		"type: job",
		"type: application",
		"type: worker",
		"data_effect: migration",
		"persistence: { mode: durable }",
		"persistence: { mode: ephemeral }",
		"deployment: { strategy: rolling }",
		"deployment: { strategy: recreate }",
		"readiness: { http: /healthz, port: 7500 }",
		"order: [server, worker]",
	} {
		if !strings.Contains(y, want) {
			t.Fatalf("scaffold missing %q:\n%s", want, y)
		}
	}
	for _, legacy := range []string{"roles:", "accessories:", "jobs:"} {
		if strings.Contains(y, legacy) {
			t.Fatalf("scaffold must not emit legacy field %q:\n%s", legacy, y)
		}
	}
	// doctor output: server wants rolling but has container_name
	if !strings.Contains(out, "container_name") || !strings.Contains(out, "server") {
		t.Fatalf("doctor should name server's container_name blocker:\n%s", out)
	}
	// The scaffold's target is deliberately unusable until the operator makes
	// the production authority explicit.
	validateOut, err := run(t, dir, "validate")
	if err == nil || !strings.Contains(err.Error()+validateOut, "CHANGE-ME") {
		t.Fatalf("untouched scaffold target must be rejected: %v\n%s", err, validateOut)
	}
	y = strings.Replace(y, "deploy@CHANGE-ME", "deploy@example.test", 1)
	if err := os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	// After completing the target, validation reaches the expected cross-file
	// rollability error instead of failing schema decoding or losing the
	// profile-gated migration component.
	validateOut, err = run(t, dir, "validate")
	if err == nil || !strings.Contains(err.Error()+validateOut, "container_name") {
		t.Fatalf("generated config should reach rollability validation: %v\n%s", err, validateOut)
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := writeProject(t)
	out, err := run(t, dir, "init")
	if err == nil {
		t.Fatalf("init must refuse to overwrite existing ob.yml: %s", out)
	}
}
