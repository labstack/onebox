package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/compose"
	"github.com/labstack/yeet/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		App: "monk", Compose: "docker-compose.yaml", Retain: 5,
		Environments: map[string]config.Environment{"production": {Hosts: []string{"deploy@h"}}},
		Roles: map[string]config.Role{
			"web": {Service: "server", Mode: "rolling", Ready: &config.Ready{
				HTTP: "/healthz", Port: 7500,
				Interval:    config.Duration(5 * time.Second),
				StartPeriod: config.Duration(5 * time.Second),
				Within:      config.Duration(120 * time.Second),
			}},
			"worker": {Service: "worker", Mode: "recreate", Drain: &config.Drain{Signal: "TERM", Wait: config.Duration(time.Second)}},
		},
		Order:       []string{"web", "worker"},
		Accessories: []string{"postgres"},
		Jobs:        []string{"migrate"},
		Hooks:       map[string]config.Hook{"migrate": {Run: "docker compose run --rm --no-deps migrate"}},
		Verify:      []config.VerifyCheck{{HTTP: "/healthz", Role: "web"}},
	}
}

const engineCompose = `
services:
  server:
    image: ghcr.io/x/app:v2
  worker:
    image: ghcr.io/x/app:v2
    command: work
  postgres:
    image: postgres:17
    healthcheck: { test: ["CMD", "pg_isready"] }
  migrate:
    image: ghcr.io/x/app:v2
    command: migrate
`

// testProject loads the fixture through the real compose loader.
func testProject(t *testing.T) *ctypes.Project {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(p, []byte(engineCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	proj, err := compose.Load(context.Background(), p, "monk")
	if err != nil {
		t.Fatal(err)
	}
	return proj
}
