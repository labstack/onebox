package onebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/transport"
)

const testSecret = "mcp-must-not-see-this"

func writeServiceProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"app.env": "RUNTIME_SECRET=initial-value\n",
		"docker-compose.yaml": `
services:
  database:
    image: ghcr.io/example/postgres:` + testSecret + `
`,
		"ob.yml": `
api_version: onebox.run/v1
app: demo
environments:
  production:
    server: deploy@example.invalid
    policy:
      require_approval: true
      allow_agent_proposals: true
workloads:
  web:
    role: application
    image: ghcr.io/example/app:v1
    strategy: rolling
    health: { http: /healthz, port: 8080 }
    env: { SECRET_TOKEN: "` + testSecret + `" }
  database:
    role: daemon
    compose: "docker-compose.yaml#database"
    persistence: { mode: durable }
    volumes: [{ name: data, path: /var/lib/postgresql/data }]
deployment:
  order: [web]
  retain_releases: 5
  migration_policy: manual
runtime:
  env_files: [app.env]
hooks:
  post_deploy: "echo ` + testSecret + `"
checks:
  url:
    - { url: "https://example.invalid/private/` + testSecret + `?token=` + testSecret + `", advisory: true }
  http:
    - { workload: web, path: "/private/` + testSecret + `" }
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "ob.yml")
}

func serviceFake() *transport.Fake {
	digest := "sha256:" + strings.Repeat("ab", 32)
	return &transport.Fake{
		HostName:   "example.invalid",
		TargetName: "deploy@example.invalid",
		Dynamic: func(cmd string) (transport.Result, bool) {
			switch {
			case strings.Contains(cmd, "/_host/owner"):
				return transport.Result{Stdout: "demo\n"}, true
			case strings.Contains(cmd, "readlink"):
				return transport.Result{Stdout: "releases/R0\n"}, true
			case strings.Contains(cmd, "docker ps") && strings.Contains(cmd, "--format"):
				return transport.Result{Stdout: "S1|web|R0|Up (healthy)\nPG1|database|R0|Up (healthy)\n"}, true
			case strings.Contains(cmd, "for f in"):
				return transport.Result{}, true
			case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "'web'"):
				return transport.Result{Stdout: "S1\n"}, true
			case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "'database'"):
				return transport.Result{Stdout: "PG1\n"}, true
			// Health first: an inspect that asks for health must not fall
			// through to the image matcher and report a digest as a health
			// state.
			case strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "Health"):
				return transport.Result{Stdout: "healthy\n"}, true
			case strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "{{.Image}}"):
				return transport.Result{Stdout: "sha256:" + strings.Repeat("ef", 32) + "\n"}, true
			case cmd == app.BuildxCapabilityCommand:
				return transport.Result{Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n      --format string\n"}, true
			case strings.Contains(cmd, "docker buildx imagetools inspect"):
				return transport.Result{Stdout: `"` + digest + `"` + "\n"}, true
			case strings.Contains(cmd, "cat ") && strings.Contains(cmd, "compose.yaml"):
				return transport.Result{Stdout: "services:\n  web:\n    image: ghcr.io/example/app:v0\n    environment:\n      SECRET_TOKEN: live-secret\n"}, true
			case strings.Contains(cmd, "find . -type f"):
				return transport.Result{Stdout: strings.Repeat("cd", 32) + "\n"}, true
			}
			return transport.Result{}, false
		},
	}
}

func newTestService(t *testing.T, f *transport.Fake) *Service {
	t.Helper()
	base := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	tick := 0
	return New(Options{
		ConfigPath: writeServiceProject(t),
		Now: func() time.Time {
			tick++
			return base.Add(time.Duration(tick) * time.Minute)
		},
		Connect: func(_ context.Context, route transport.Route) (transport.Transport, error) {
			if route.String() != "deploy@example.invalid" {
				t.Fatalf("connector target = %q", route)
			}
			return f, nil
		},
	})
}

func TestPlanDeployBindsAndRendersEveryRuntimeImage(t *testing.T) {
	plan, err := newTestService(t, serviceFake()).PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("ab", 32)
	wants := map[string]string{
		"web":      "ghcr.io/example/app@" + digest,
		"database": "ghcr.io/example/postgres@" + digest,
	}
	if len(plan.Artifact.PinnedImages) != len(wants) {
		t.Fatalf("pinned images = %#v", plan.Artifact.PinnedImages)
	}
	for workload, want := range wants {
		if plan.Artifact.PinnedImages[workload] != want {
			t.Fatalf("%s pin = %q, want %q", workload, plan.Artifact.PinnedImages[workload], want)
		}
		if !strings.Contains(plan.Artifact.RenderedCompose, "image: "+want) {
			t.Fatalf("rendered runtime does not use %s pin:\n%s", workload, plan.Artifact.RenderedCompose)
		}
	}
	if !strings.Contains(plan.Artifact.RenderedCompose, "ob.workload: database") {
		t.Fatalf("pinning the adopted Compose service dropped authored/overlay keys:\n%s", plan.Artifact.RenderedCompose)
	}
}

func writeComposeBuildProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pinnedWeb := "ghcr.io/example/app@sha256:" + strings.Repeat("1", 64)
	files := map[string]string{
		"compose.yaml": "services:\n  database:\n    build: .\n    command: [postgres, -c, shared_buffers=256MB]\n",
		"ob.yml": `api_version: onebox.run/v1
app: demo
environments: {production: {server: deploy@example.invalid}}
workloads:
  web: {role: application, image: ` + pinnedWeb + `, strategy: recreate}
  database: {role: daemon, compose: compose.yaml#database}
deployment: {order: [web, database]}
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "ob.yml")
}

func composeBuildService(t *testing.T, images app.Images) *Service {
	t.Helper()
	return New(Options{
		ConfigPath: writeComposeBuildProject(t), Environment: "production", Images: images,
		Connect: func(context.Context, transport.Route) (transport.Transport, error) { return serviceFake(), nil },
	})
}

func TestPlanDeployRefusesComposeBuildWithoutReleaseImage(t *testing.T) {
	_, err := composeBuildService(t, nil).PlanDeploy(context.Background(), PlanDeployRequest{})
	var resolution *engine.ImageResolutionError
	if !errors.As(err, &resolution) || resolution.Workload != "database" {
		t.Fatalf("error = %v, want database ImageResolutionError", err)
	}
	if resolution.ResolvingCommand != "ob plan --image database=<digest-reference>" {
		t.Fatalf("resolving command = %q", resolution.ResolvingCommand)
	}
}

func TestPlanDeployRetainsComposeBuildImageForBoundReplay(t *testing.T) {
	image := "ghcr.io/example/postgres@sha256:" + strings.Repeat("2", 64)
	plan, err := composeBuildService(t, app.Images{"database": image}).PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Artifact.BuildImages["database"] != image || plan.Artifact.PinnedImages["database"] != image {
		t.Fatalf("build/pinned images = %#v / %#v", plan.Artifact.BuildImages, plan.Artifact.PinnedImages)
	}
	for _, want := range []string{"image: " + image, "shared_buffers=256MB"} {
		if !strings.Contains(plan.Artifact.RenderedCompose, want) {
			t.Fatalf("rendered Compose dropped %q:\n%s", want, plan.Artifact.RenderedCompose)
		}
	}
}

func TestPlanDeployUsesDeployedSecretGraphDuringTransition(t *testing.T) {
	fakeSops(t)
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	imageWeb := "ghcr.io/example/web@sha256:" + strings.Repeat("1", 64)
	imageWorker := "ghcr.io/example/worker@sha256:" + strings.Repeat("2", 64)
	project := func(workerSecret bool) string {
		workerEnv := ""
		if workerSecret {
			workerEnv = "\n    env_files: [{file: worker.enc.env, provider: sops}]"
		}
		return `api_version: onebox.run/v1
app: demo
environments:
  production: {server: deploy@example.invalid}
workloads:
  web:
    role: application
    image: ` + imageWeb + `
    strategy: recreate
    env_files: [{file: web.enc.env, provider: sops}]
  worker:
    role: daemon
    image: ` + imageWorker + workerEnv + `
deployment: {order: [web, worker]}
`
	}
	write("web.enc.env", "WEB_TOKEN=web\n")
	write("worker.enc.env", "WORKER_TOKEN=worker\n")
	write("ob.yml", project(true))

	const oldGeneration = "sg-111111111111111111111111"
	liveCompose := `services:
  web:
    image: ` + imageWeb + `
    env_file: [.ob-secret-generations/` + oldGeneration + `/.ob-decrypted-sops-web.enc.env]
    labels: {ob.app: demo, ob.release: R0, ob.workload: web, ob.secret-generation: ` + oldGeneration + `}
  worker:
    image: ` + imageWorker + `
    labels: {ob.app: demo, ob.release: R0, ob.workload: worker}
`
	fake := serviceFake()
	baseDynamic := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ob.snapshot.yml"):
			return transport.Result{Stdout: project(false)}, true
		case strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml"):
			return transport.Result{Stdout: liveCompose}, true
		case strings.Contains(command, "docker ps") && strings.Contains(command, "--format"):
			return transport.Result{Stdout: "S1|web|R0|Up\nW1|worker|R0|Up\n"}, true
		default:
			return baseDynamic(command)
		}
	}
	service := New(Options{
		ConfigPath: filepath.Join(dir, "ob.yml"),
		Now:        func() time.Time { return time.Date(2026, 8, 23, 19, 0, 0, 0, time.UTC) },
		Connect: func(context.Context, transport.Route) (transport.Transport, error) {
			return fake, nil
		},
	})
	plan, err := service.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Artifact.SecretGeneration == "" || plan.Artifact.SecretGeneration == oldGeneration {
		t.Fatalf("replacement generation = %q", plan.Artifact.SecretGeneration)
	}
	for _, secret := range []string{"web.enc.env", "worker.enc.env"} {
		staged := app.EnvFile{File: secret, Provider: "sops"}.StagedPath()
		want := app.SecretGenerationPath(plan.Artifact.SecretGeneration, staged)
		if !strings.Contains(plan.Artifact.RenderedCompose, want) {
			t.Fatalf("planned runtime does not select %s:\n%s", want, plan.Artifact.RenderedCompose)
		}
	}
}
