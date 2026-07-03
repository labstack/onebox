package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/transport"
)

func TestBootstrapSequence(t *testing.T) {
	f := happyFake()
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Hooks["bootstrap"] = config.Hook{Run: "apt-get install -y something-host-specific"}
	cfg.Registry = &config.Registry{Server: "ghcr.io", Username: "vishr", PasswordEnv: "TEST_GHCR_TOKEN"}
	t.Setenv("TEST_GHCR_TOKEN", "s3cret")
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: dir})
	if err := e.Bootstrap(context.Background(), "R1-bootstrap", t.TempDir()); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	ordered := []string{
		"mkdir -p", // dirs
		"apt-get install -y something-host-specific",     // bootstrap hook
		"docker login ghcr.io -u vishr --password-stdin", // registry (stdin)
		"up -d --no-deps --no-recreate postgres",         // accessories
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
	if err := e.Bootstrap(context.Background(), "R1-bootstrap", t.TempDir()); err != nil {
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
	if err := e.Bootstrap(context.Background(), "R1-bootstrap", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "get.docker.com") {
		t.Fatal("must not reinstall a present runtime")
	}
}

func TestBootstrapFailsEarlyWithoutPassword(t *testing.T) {
	f := &transport.Fake{}
	cfg := testConfig()
	cfg.Registry = &config.Registry{Server: "ghcr.io", Username: "v", PasswordEnv: "NOPE_UNSET_VAR"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Bootstrap(context.Background(), "R1", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "NOPE_UNSET_VAR") {
		t.Fatalf("want early env error, got %v (commands: %v)", err, f.Commands)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("must fail before touching host: %v", f.Commands)
	}
}
