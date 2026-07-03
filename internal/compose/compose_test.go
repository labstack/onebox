package compose

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/config"
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
