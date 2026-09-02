package onebox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/transport"
)

func workloadPlanFixture(t *testing.T) (*app.Resolved, string, []OperationStep, engine.HostState) {
	t.Helper()
	spec, err := app.LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api:
    role: application
    image: ghcr.io/example/api:v1
    replicas: 2
    strategy: rolling
    health: {http: /healthz, port: 8080}
  worker: {role: worker, image: ghcr.io/example/worker:v1, strategy: recreate}
  env-worker: {role: worker, image: ghcr.io/example/env:v1, strategy: recreate, env_files: [worker.env]}
  secret-worker: {role: worker, image: ghcr.io/example/secret:v1, strategy: recreate, env_files: [{file: worker.enc.env, provider: sops}]}
  bind-worker:
    role: worker
    image: ghcr.io/example/bind:v1
    strategy: recreate
    volumes: [{source: ./payload, path: /payload, mode: ro}]
  host-bind-worker:
    role: worker
    image: ghcr.io/example/host-bind:v1
    strategy: recreate
    volumes: [{source: /data/sample, path: /data, mode: rw}]
deployment: {order: [api, worker, env-worker, secret-worker, bind-worker, host-bind-worker]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := resolved.Render("production", "20260825-120000-abcd", nil)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := DeploymentGraph(resolved, "20260825-120000-abcd")
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := workloadRevisions(string(rendered.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	host := engine.HostState{
		CurrentRelease: "20260825-110000-prev",
		WorkloadRevisions: map[string][]string{
			"api": {revisions["api"], revisions["api"]}, "worker": {revisions["worker"]},
			"env-worker": {revisions["env-worker"]}, "secret-worker": {revisions["secret-worker"]},
			"bind-worker":      {revisions["bind-worker"]},
			"host-bind-worker": {revisions["host-bind-worker"]},
		},
		WorkloadHealth: map[string][]string{
			"api": {"healthy", "healthy"}, "worker": {"none"},
			"env-worker": {"none"}, "secret-worker": {"none"}, "bind-worker": {"none"},
			"host-bind-worker": {"none"},
		},
	}
	return resolved, string(rendered.Bytes), steps, host
}

func TestPlanWorkloadActionsRetainsOnlyProvenRuntimeMatches(t *testing.T) {
	resolved, rendered, steps, host := workloadPlanFixture(t)
	planned, err := planWorkloadActions(resolved, steps, rendered, host, false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]OperationStep{}
	for _, step := range planned {
		if step.Kind == StepWorkloadRelease {
			byName[step.Service] = step
		}
	}
	for _, name := range []string{"api", "worker", "env-worker", "secret-worker", "bind-worker", "host-bind-worker"} {
		step := byName[name]
		if step.Action != "retain" || step.Mutation || step.Reason != "runtime_unchanged" || !strings.HasPrefix(step.Revision, "sha256:") {
			t.Errorf("%s plan = %#v, want non-mutating retain", name, step)
		}
	}
}

func TestPlanWorkloadActionsFailsClosedOnDriftAndHealth(t *testing.T) {
	resolved, rendered, steps, host := workloadPlanFixture(t)
	host.WorkloadRevisions["worker"] = []string{"sha256:" + strings.Repeat("0", 64)}
	host.WorkloadHealth["api"] = []string{"healthy", "starting"}
	planned, err := planWorkloadActions(resolved, steps, rendered, host, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range planned {
		switch step.Service {
		case "api":
			if step.Action != "rolling" || step.Reason != "health_not_ready" {
				t.Errorf("api plan = %#v", step)
			}
		case "worker":
			if step.Action != "recreate" || step.Reason != "runtime_changed_or_drifted" {
				t.Errorf("worker plan = %#v", step)
			}
		}
	}
}

func TestPlanWorkloadActionsTreatsLegacyAndFirstDeploySafely(t *testing.T) {
	resolved, rendered, steps, host := workloadPlanFixture(t)
	host.WorkloadRevisions["worker"] = []string{""}
	planned, err := planWorkloadActions(resolved, steps, rendered, host, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range planned {
		if step.Service == "worker" && (step.Action != "recreate" || step.Reason != "revision_unavailable") {
			t.Errorf("legacy worker plan = %#v", step)
		}
	}
	host.CurrentRelease = ""
	planned, err = planWorkloadActions(resolved, steps, rendered, host, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range planned {
		if step.Kind == StepWorkloadRelease && (step.Action == "retain" || step.Reason != "initial_deploy") {
			t.Errorf("first deploy step = %#v", step)
		}
	}
}

func TestPlanWorkloadActionsSealsNoOpRedeployAsFreshRoll(t *testing.T) {
	resolved, rendered, steps, host := workloadPlanFixture(t)
	planned, err := planWorkloadActions(resolved, steps, rendered, host, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range planned {
		if step.Kind != StepWorkloadRelease {
			continue
		}
		if step.Action != step.Strategy || !step.Mutation || step.Reason != "redeploy_only" {
			t.Errorf("redeploy step = %#v, want sealed fresh roll", step)
		}
	}
}

func TestPlanDeployRetainsUnchangedWorkerWhenAnotherWorkloadChanges(t *testing.T) {
	digestA := strings.Repeat("1", 64)
	digestB := strings.Repeat("2", 64)
	workerDigest := strings.Repeat("3", 64)
	project := func(apiDigest string) string {
		return `api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api:
    role: application
    image: ghcr.io/example/api@sha256:` + apiDigest + `
    strategy: rolling
    health: {http: /healthz, port: 8080}
  worker: {role: worker, image: ghcr.io/example/worker@sha256:` + workerDigest + `, strategy: recreate}
deployment: {order: [api, worker]}
`
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ob.yml")
	oldSpec, err := app.LoadBytes([]byte(project(digestA)), configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldRendered, err := oldSpec.Render("production", "20260825-110000-prev", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldRevisions, err := workloadRevisions(string(oldRendered.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(project(digestB)), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &transport.Fake{HostName: "example.test", TargetName: "deploy@example.test"}
	fake.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/20260825-110000-prev\n"}, true
		case strings.Contains(command, "docker ps --filter") && strings.Contains(command, "--format"):
			return transport.Result{Stdout: "A1|api|20260825-110000-prev|" + oldRevisions["api"] + "|Up (healthy)\n" +
				"W1|worker|20260825-110000-prev|" + oldRevisions["worker"] + "|Up\n"}, true
		case strings.Contains(command, "docker ps -q") && strings.Contains(command, "service='api'"):
			return transport.Result{Stdout: "A1\n"}, true
		case strings.Contains(command, "docker ps -q") && strings.Contains(command, "service='worker'"):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(command, "docker inspect") && strings.Contains(command, "{{.Image}}"):
			return transport.Result{Stdout: "sha256:" + strings.Repeat("a", 64) + "\n"}, true
		case strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml"):
			return transport.Result{Stdout: string(oldRendered.Bytes)}, true
		case strings.Contains(command, "find . -type f"):
			return transport.Result{Stdout: strings.Repeat("b", 64) + "\n"}, true
		case strings.Contains(command, "for f in"):
			return transport.Result{}, true
		}
		return transport.Result{}, false
	}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	service := New(Options{
		ConfigPath: configPath, Now: func() time.Time { return now },
		Connect: func(_ context.Context, _ transport.Route) (transport.Transport, error) { return fake, nil },
	})
	plan, err := service.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byService := map[string]OperationStep{}
	for _, step := range plan.Operation.Steps {
		if step.Kind == StepWorkloadRelease {
			byService[step.Service] = step
		}
	}
	if step := byService["api"]; step.Action != "rolling" || step.Reason != "runtime_changed_or_drifted" {
		t.Fatalf("api plan = %#v", step)
	}
	if step := byService["worker"]; step.Action != "retain" || step.Reason != "runtime_unchanged" || step.Mutation {
		t.Fatalf("worker plan = %#v", step)
	}
	commands := strings.Join(plan.Artifact.Commands, "\n")
	if !strings.Contains(commands, "release worker (retain, runtime_unchanged): no runtime mutation") || strings.Contains(commands, "--force-recreate") {
		t.Fatalf("command preview does not match retain action:\n%s", commands)
	}
}
