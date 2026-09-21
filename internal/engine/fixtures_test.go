package engine

import (
	"context"
	"testing"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/compose"
)

const (
	engineTestPreviousReleaseID  = "20251231-235959-prev"
	engineTestDeployReleaseID    = "20260101-000000-next"
	engineTestBootstrapReleaseID = "20260101-000000-bootstrap"
)

// The engine's fixture is now a project declaration rather than a hand-built
// normalised config. Loading it through the real loader is the point: a test
// that assembles the struct directly can assert on a shape the loader would
// never produce.
const engineProject = `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: sample
spec:
  environments:
    production:
      server: deploy@h
  workloads:
    web:
      role: Application
      image: ghcr.io/x/app:v2
      health: {http: /healthz, port: 7500, interval: 5s, startPeriod: 5s, within: 120s}
    worker:
      role: Worker
      image: ghcr.io/x/app:v2
      command: work
      strategy: Recreate
      drain: {signal: TERM, wait: 1s}
    migrate:
      role: Job
      image: ghcr.io/x/app:v2
      command: migrate
      deploymentPhase: PreRelease
      dataEffect: Unknown
  services:
    postgres:
      driver: postgres
      version: 17
  deployment:
    order: [web, worker]
  checks:
    http:
      - {workload: web, path: /healthz}
`

func testConfig() *app.Resolved {
	spec, err := app.LoadBytes([]byte(engineProject), "ob.yml")
	if err != nil {
		panic("engine fixture does not load: " + err.Error())
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		panic("engine fixture does not resolve: " + err.Error())
	}
	// Tests shape a case by adding a hook; an absent hooks block would make
	// every one of them a nil-map panic rather than an assertion.
	if resolved.Hooks == nil {
		resolved.Hooks = map[string]app.Command{}
	}
	return resolved
}

// testProject parses the runtime the fixture generates, the same way every
// execution path does.
func testProject(t *testing.T) *ctypes.Project {
	t.Helper()
	rendered, err := testConfig().Render("production", "test-release", nil)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := compose.LoadBytes(context.Background(), rendered.Bytes, "sample", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return proj
}
