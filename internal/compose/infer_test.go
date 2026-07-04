package compose

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/labstack/onebox/internal/config"
)

func TestInferClassifiesAndModes(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Accessories: []string{"postgres"},
		Jobs:        []string{"migrate"},
	}
	Infer(cfg, p)

	// Every unclassified service became a role keyed by its own name.
	want := map[string]string{"server": "rolling", "worker": "recreate", "rogue": "recreate"}
	if len(cfg.Roles) != len(want) {
		t.Fatalf("roles = %v, want keys %v", cfg.Roles, want)
	}
	for name, mode := range want {
		r, ok := cfg.Roles[name]
		if !ok {
			t.Fatalf("missing inferred role %q", name)
		}
		if r.Service != name {
			t.Fatalf("role %q service = %q, want %q", name, r.Service, name)
		}
		if r.Mode != mode {
			t.Fatalf("role %q mode = %q, want %q (server rolls: hc+rollable; worker/rogue recreate)", name, r.Mode, mode)
		}
	}
	// postgres/migrate stayed accessory/job — not roles.
	if _, ok := cfg.Roles["postgres"]; ok {
		t.Fatal("postgres should stay an accessory, not become a role")
	}
	// order: no depends_on ⇒ alphabetical role names.
	if got := cfg.Order; !reflect.DeepEqual(got, []string{"rogue", "server", "worker"}) {
		t.Fatalf("order = %v, want alphabetical [rogue server worker]", got)
	}
}

func TestInferRespectsExplicitOverrides(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		// server rolls by default; force recreate, and key a role by service name
		// (service omitted, defaults to the key).
		Roles:       map[string]config.Role{"server": {Mode: "recreate"}},
		Accessories: []string{"postgres"},
		Jobs:        []string{"migrate"},
		Order:       []string{"worker", "server", "rogue"},
	}
	Infer(cfg, p)
	if r := cfg.Roles["server"]; r.Mode != "recreate" || r.Service != "server" {
		t.Fatalf("explicit override lost: %+v (want mode=recreate service=server)", r)
	}
	if got := cfg.Order; !reflect.DeepEqual(got, []string{"worker", "server", "rogue"}) {
		t.Fatalf("explicit order overridden: %v", got)
	}
}

func TestInferOrderFromDependsOn(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(`
services:
  api:
    image: x
    depends_on: [cache]
    healthcheck:
      test: ["CMD", "true"]
  cache:
    image: y
    healthcheck:
      test: ["CMD", "true"]
`), 0o644)
	p, err := Load(context.Background(), filepath.Join(dir, "docker-compose.yaml"), "d")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	Infer(cfg, p)
	// drain is never inferred — a choreography judgment, left to the operator.
	if cfg.Roles["api"].Drain != nil {
		t.Fatalf("drain must not be inferred: %+v", cfg.Roles["api"].Drain)
	}
	// api depends_on cache ⇒ cache is released first.
	if got := cfg.Order; !reflect.DeepEqual(got, []string{"cache", "api"}) {
		t.Fatalf("order = %v, want [cache api] from depends_on", got)
	}
}
