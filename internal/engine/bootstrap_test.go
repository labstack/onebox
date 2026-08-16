package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestBootstrapSequence(t *testing.T) {
	f := happyFake()
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Hooks["bootstrap"] = app.Command{Run: "apt-get install -y something-host-specific"}
	cfg.Registries = map[string]app.Registry{"default": {Server: "ghcr.io", Username: "vishr", PasswordEnv: "TEST_GHCR_TOKEN"}}
	t.Setenv("TEST_GHCR_TOKEN", "s3cret")
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: dir})
	if err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	ordered := []string{
		"mkdir -p", // dirs
		"apt-get install -y something-host-specific",         // bootstrap hook
		"docker login 'ghcr.io' -u 'vishr' --password-stdin", // registry (stdin, quoted)
		"docker compose -p 'ob_sample_postgres'",             // services
	}
	last := -1
	for _, want := range ordered {
		i := strings.Index(seq, want)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", want, seq)
		}
		if i < last {
			t.Fatalf("%q out of order:\n%s", want, seq)
		}
		last = i
	}
	if strings.Contains(seq, "s3cret") {
		t.Fatal("password must never appear in a command string")
	}
	if strings.Contains(seq, "COMPOSE_FILE=") {
		t.Fatal("the host-only bootstrap hook must not receive an application Compose file")
	}
	if len(f.Uploads) != 0 {
		t.Fatalf("bootstrap must not upload an application payload: %v", f.Uploads)
	}
	found := false
	for _, in := range f.Inputs {
		if strings.Contains(in, "s3cret") {
			found = true
		}
	}
	if !found {
		t.Fatalf("password must travel via stdin: %v", f.Inputs)
	}
	// bootstrap never activates
	if strings.Contains(seq, "ln -sfn") {
		t.Fatal("bootstrap must not activate a release")
	}
}

func TestBootstrapRefusesExistingEvidenceIdentity(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "mkdir -m 700") && strings.Contains(command, "/releases/"+engineTestBootstrapReleaseID) {
			return transport.Result{ExitCode: 1, Stderr: "File exists"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID)
	if err == nil || !strings.Contains(err.Error(), "bootstrap evidence directory") {
		t.Fatalf("existing bootstrap evidence identity was not refused: %v", err)
	}
}

func TestBootstrapStopsWhenJournalStartFails(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, `"phase":"bootstrap","event":"start"`) {
			return transport.Result{ExitCode: 74, Stderr: "journal is read-only"}, true
		}
		return base(cmd)
	}
	cfg := testConfig()
	cfg.Hooks["bootstrap"] = app.Command{Run: "touch /tmp/bootstrap-ran"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID)
	if err == nil || !strings.Contains(err.Error(), "journal bootstrap start") {
		t.Fatalf("bootstrap error = %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "touch /tmp/bootstrap-ran") {
		t.Fatalf("bootstrap hook ran after journal failure:\n%s", strings.Join(f.Commands, "\n"))
	}
}

func TestBootstrapInstallsMissingRuntime(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	installed := false
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker version") {
			if !installed {
				return transport.Result{ExitCode: 127, Stderr: "docker: command not found"}, true
			}
			return transport.Result{Stdout: "27.0.3\n"}, true
		}
		if strings.Contains(cmd, "get.docker.com") {
			installed = true
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID); err != nil {
		t.Fatalf("bootstrap with runtime install: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "get.docker.com") || !strings.Contains(seq, "systemctl enable --now docker") {
		t.Fatalf("missing runtime install:\n%s", seq)
	}
}

func TestBootstrapSkipsInstallWhenRuntimePresent(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "get.docker.com") {
		t.Fatal("must not reinstall a present runtime")
	}
}

func TestBootstrapFailsEarlyWithoutPassword(t *testing.T) {
	f := &transport.Fake{}
	cfg := testConfig()
	cfg.Registries = map[string]app.Registry{"default": {Server: "ghcr.io", Username: "v", PasswordEnv: "NOPE_UNSET_VAR"}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Bootstrap(context.Background(), engineTestDeployReleaseID)
	if err == nil || !strings.Contains(err.Error(), "NOPE_UNSET_VAR") {
		t.Fatalf("want early env error, got %v (commands: %v)", err, f.Commands)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("must fail before touching host: %v", f.Commands)
	}
}

func TestBootstrapEnsuresManagedProxyBeforeServices(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	ps := proxyPS(f, false)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if res, ok := ps(cmd); ok {
			return res, ok
		}
		return base(cmd)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "traefik", "traefik.yml"), []byte("ping: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	cfg.Registries = map[string]app.Registry{"default": {Server: "ghcr.io", Username: "vishr", PasswordEnv: "TEST_GHCR_TOKEN"}}
	t.Setenv("TEST_GHCR_TOKEN", "s3cret")
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: dir})
	if err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	ordered := []string{
		"docker login 'ghcr.io'",
		"docker compose -p onebox-proxy -f '/var/lib/ob/_host/proxy/compose.yaml' up -d",
		"docker compose -p 'ob_sample_postgres'",
	}
	last := -1
	for _, want := range ordered {
		i := strings.Index(seq, want)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", want, seq)
		}
		if i < last {
			t.Fatalf("%q out of order (proxy must precede services):\n%s", want, seq)
		}
		last = i
	}
}
