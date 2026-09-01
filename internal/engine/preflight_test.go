package engine

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func fakeEngine(t *testing.T, f *transport.Fake) *Engine {
	t.Helper()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}})
	return e
}

func TestPreflightHappyPath(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`_host/owner`), Result: transport.Result{Stdout: "sample\n"}},
		{Match: regexp.MustCompile(`docker version`), Result: transport.Result{Stdout: "27.0.3\n"}},
		{Match: regexp.MustCompile(`docker compose version`), Result: transport.Result{Stdout: "2.29.1\n"}},
		{Match: regexp.MustCompile(`imagetools inspect --help`), Result: transport.Result{Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n      --format string\n"}},
		{Match: regexp.MustCompile(`docker buildx version`), Result: transport.Result{Stdout: "github.com/docker/buildx v0.33.0\n"}},
		{Match: regexp.MustCompile(`df -Pk`), Result: transport.Result{Stdout: "4194304\n"}}, // 4 GiB in KiB
		{Match: regexp.MustCompile(`docker ps .*postgres`), Result: transport.Result{Stdout: "abc123\n"}},
		{Match: regexp.MustCompile(`docker inspect .*abc123`), Result: transport.Result{Stdout: "healthy\n"}},
	}}
	if err := fakeEngine(t, f).Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v\ncommands:\n%s", err, strings.Join(f.Commands, "\n"))
	}
}

func TestPreflightFailsOnStoppedService(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`_host/owner`), Result: transport.Result{Stdout: "sample\n"}},
		{Match: regexp.MustCompile(`docker version`), Result: transport.Result{Stdout: "27.0.3\n"}},
		{Match: regexp.MustCompile(`docker compose version`), Result: transport.Result{Stdout: "2.29.1\n"}},
		{Match: regexp.MustCompile(`imagetools inspect --help`), Result: transport.Result{Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n      --format string\n"}},
		{Match: regexp.MustCompile(`docker buildx version`), Result: transport.Result{Stdout: "github.com/docker/buildx v0.33.0\n"}},
		{Match: regexp.MustCompile(`df -Pk`), Result: transport.Result{Stdout: "4194304\n"}},
		{Match: regexp.MustCompile(`docker ps .*postgres`), Result: transport.Result{Stdout: "\n"}},
	}}
	err := fakeEngine(t, f).Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("want service-down error, got %v", err)
	}
}

func TestContainerIDsRejectsSuspiciousOutput(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`docker ps -q`), Result: transport.Result{Stdout: "abc123; rm -rf /\n"}},
	}}
	e := fakeEngine(t, f)
	if _, err := e.containerIDs(context.Background(), "web"); err == nil {
		t.Fatal("suspicious container id must be rejected")
	}
}

func TestPreflightManagedProxyMustRun(t *testing.T) {
	mk := func(proxyUp bool) *transport.Fake {
		return &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
			switch {
			case strings.Contains(cmd, "_host/owner"):
				return transport.Result{Stdout: "sample\n"}, true
			case strings.Contains(cmd, "docker version"):
				return transport.Result{Stdout: "27.0.3\n"}, true
			case strings.Contains(cmd, "docker compose version"):
				return transport.Result{Stdout: "2.29.1\n"}, true
			case strings.Contains(cmd, "imagetools inspect --help"):
				return transport.Result{Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n      --format string\n"}, true
			case strings.Contains(cmd, "docker buildx version"):
				return transport.Result{Stdout: "github.com/docker/buildx v0.33.0\n"}, true
			case strings.Contains(cmd, "df -Pk"):
				return transport.Result{Stdout: "4194304\n"}, true
			case strings.Contains(cmd, "project='onebox-proxy'"):
				if proxyUp {
					return transport.Result{Stdout: "PX1\n"}, true
				}
				return transport.Result{Stdout: ""}, true
			case strings.Contains(cmd, "docker ps") && strings.Contains(cmd, "postgres"):
				return transport.Result{Stdout: "abc123\n"}, true
			case strings.Contains(cmd, "docker inspect"):
				return transport.Result{Stdout: "healthy\n"}, true
			}
			return transport.Result{}, false
		}}
	}
	cfg := testConfig()
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}

	f := mk(false)
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}})
	if err := e.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("preflight must fail when the managed proxy is absent, got %v", err)
	}

	f = mk(true)
	e = New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}})
	if err := e.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight with healthy proxy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
}

func TestPreflightRequiresProxyDiscoveryController(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "_host/owner"):
			return transport.Result{Stdout: "sample\n"}, true
		case strings.Contains(cmd, "docker version"):
			return transport.Result{Stdout: "27.0.3\n"}, true
		case strings.Contains(cmd, "docker compose version"):
			return transport.Result{Stdout: "2.29.1\n"}, true
		case strings.Contains(cmd, "imagetools inspect --help"):
			return transport.Result{Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n      --format string\n"}, true
		case strings.Contains(cmd, "docker buildx version"):
			return transport.Result{Stdout: "github.com/docker/buildx v0.33.0\n"}, true
		case strings.Contains(cmd, "df -Pk"):
			return transport.Result{Stdout: "4194304\n"}, true
		case strings.Contains(cmd, "com.docker.compose.service=discovery"):
			return transport.Result{Stdout: ""}, true
		case strings.Contains(cmd, "com.docker.compose.service=proxy"):
			return transport.Result{Stdout: "PX1\n"}, true
		}
		return transport.Result{}, false
	}}
	cfg := testConfig()
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "discovery controller") {
		t.Fatalf("preflight error = %v", err)
	}
}

// The deploy preflight step asserted the runtime and Compose but never the
// image resolver, so a host it accepted could still fail at planning. Both
// gates now ask the same questions.
func TestPreflightRefusesIncompatibleBuildx(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`_host/owner`), Result: transport.Result{Stdout: "sample\n"}},
		{Match: regexp.MustCompile(`docker version`), Result: transport.Result{Stdout: "27.0.3\n"}},
		{Match: regexp.MustCompile(`docker compose version`), Result: transport.Result{Stdout: "2.29.1\n"}},
		{Match: regexp.MustCompile(`imagetools inspect --help`), Result: transport.Result{Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n"}},
		{Match: regexp.MustCompile(`docker buildx version`), Result: transport.Result{Stdout: "github.com/docker/buildx v0.30.1\n"}},
		{Match: regexp.MustCompile(`df -Pk`), Result: transport.Result{Stdout: "4194304\n"}},
	}}
	err := fakeEngine(t, f).Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), app.PrerequisiteResolver) ||
		!strings.Contains(err.Error(), "v0.30.1") {
		t.Fatalf("preflight error = %v, want the resolver refusal naming the client version", err)
	}
}
