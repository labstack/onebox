package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{
		App: "demo",
		Roles: map[string]config.Role{
			"web":    {Service: "server", Mode: "rolling", Ready: &config.Ready{HTTP: "/healthz", Port: 8080}},
			"worker": {Service: "worker", Mode: "recreate"},
		},
		Accessories: []string{"postgres"},
		Jobs:        []string{"migrate"},
	}
}

func TestLoadAndClassifyReportsOrphan(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	err = Classify(p, testCfg())
	if err == nil || !strings.Contains(err.Error(), "rogue") {
		t.Fatalf("want orphan error naming 'rogue', got %v", err)
	}
}

func TestCheckRollable(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	cfg.Roles["edge"] = config.Role{Service: "rogue", Mode: "rolling", Ready: &config.Ready{HTTP: "/", Port: 8080}}
	errs := CheckRollable(p, cfg)
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "container_name") || !strings.Contains(joined, "host port") {
		t.Fatalf("want container_name and host port violations for 'rogue', got:\n%s", joined)
	}
	// server rolls clean
	if strings.Contains(joined, `"server"`) {
		t.Fatalf("server should be rollable:\n%s", joined)
	}
}

// replicas > 1 triggers the multi-copy constraints even for a recreate role: a
// service with a container_name can't run a fleet.
func TestCheckRollableEnforcesReplicasConstraints(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	// 'rogue' has a container_name + host port; as a 2-replica recreate role it
	// must be rejected even though it isn't rolling.
	cfg.Roles["edge"] = config.Role{Service: "rogue", Mode: "recreate", Replicas: 2}
	joined := ""
	for _, e := range CheckRollable(p, cfg) {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "container_name") {
		t.Fatalf("replicas>1 must forbid container_name, got:\n%s", joined)
	}
}

func TestCheckRollableAdoptsComposeHealthcheck(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	// server: rolling, NO ready kind — its compose healthcheck is adopted
	cfg.Roles["web"] = config.Role{Service: "server", Mode: "rolling"}
	if errs := CheckRollable(p, cfg); len(errs) != 0 {
		t.Fatalf("compose healthcheck must satisfy the readiness rule: %v", errs)
	}
	// worker: rolling, no ready, no compose healthcheck — must refuse
	cfg.Roles["worker"] = config.Role{Service: "worker", Mode: "rolling"}
	errs := CheckRollable(p, cfg)
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "adopt") {
		t.Fatalf("rolling without any healthcheck must be refused: %v", errs)
	}
}

func TestLoadLenientToleratesMissingRequiredVars(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "docker-compose.yaml")
	src := `
services:
  server:
    image: ghcr.io/x/app:${MISSING_VERSION:?must be set}
    environment:
      WITH_DEFAULT: ${ABSENT:-fallback}
      EMPTY_REQUIRED: ${EMPTY_SET:?must be non-empty}
      # a failing :? and a defaulted var in the SAME string — the default
      # must still apply (per-match leniency, not whole-string retry)
      MIXED: ${ABSENT:-user}:${MISSING_VERSION:?must be set}@db
`
	t.Setenv("EMPTY_SET", "") // set-but-empty: :? errors on this too
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// strict load: the :? contract holds for deploy-path verbs
	if _, err := Load(context.Background(), p, "demo"); err == nil || !strings.Contains(err.Error(), "MISSING_VERSION") {
		t.Fatalf("strict load must fail on required var, got %v", err)
	}
	// lenient load: read-only verbs (status/logs/exec) never consume the
	// values — missing vars resolve to a visible placeholder
	proj, err := LoadLenient(context.Background(), p, "demo")
	if err != nil {
		t.Fatalf("lenient load: %v", err)
	}
	if img := proj.Services["server"].Image; img != "ghcr.io/x/app:${MISSING_VERSION}" {
		t.Fatalf("missing var must resolve to a placeholder, got %q", img)
	}
	// :- defaults still apply
	if v := proj.Services["server"].Environment["WITH_DEFAULT"]; v == nil || *v != "fallback" {
		t.Fatalf("default fallback broken: %v", v)
	}
	// :? on a SET-but-EMPTY var must also be lenient (it errors like unset)
	if v := proj.Services["server"].Environment["EMPTY_REQUIRED"]; v == nil || *v != "${EMPTY_SET}" {
		t.Fatalf("set-but-empty required var must resolve to placeholder: %v", v)
	}
	// per-match: the failing :? gets the placeholder, the neighbor keeps its default
	if v := proj.Services["server"].Environment["MIXED"]; v == nil || *v != "user:${MISSING_VERSION}@db" {
		got := "<nil>"
		if v != nil {
			got = *v
		}
		t.Fatalf("mixed string: want %q, got %q", "user:${MISSING_VERSION}@db", got)
	}
}

func TestCheckRollableRejectsNetworkModeWithManagedProxy(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "docker-compose.yaml")
	src := `
services:
  server:
    image: ghcr.io/x/app:v1
    network_mode: host
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	proj, err := Load(context.Background(), p, "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		App:   "demo",
		Roles: map[string]config.Role{"server": {Service: "server", Mode: "recreate"}},
		Proxy: config.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"},
	}
	errs := CheckRollable(proj, cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "network_mode") {
			found = true
		}
	}
	if !found {
		t.Fatalf("managed proxy + network_mode must be a validate error, got %v", errs)
	}
	// without managed proxy it stays legal
	cfg.Proxy = config.Proxy{}
	if errs := CheckRollable(proj, cfg); len(errs) != 0 {
		t.Fatalf("network_mode without managed proxy must pass: %v", errs)
	}
}
