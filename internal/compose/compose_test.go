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
