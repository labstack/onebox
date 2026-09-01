package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

type bootstrapNetworkLocal struct {
	*transport.Local
	owner string
}

func (l *bootstrapNetworkLocal) Run(ctx context.Context, command string) (transport.Result, error) {
	if strings.Contains(command, "docker network inspect --format") {
		return transport.Result{Stdout: "abc123|" + l.owner + "|\n"}, nil
	}
	return l.Local.Run(ctx, command)
}

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
		"mkdir -p",                                           // dirs
		"> '/var/lib/ob/sample/lock'",                        // application lock
		"> '/var/lib/ob/sample/fence'",                       // mutation fence
		`"phase":"bootstrap","event":"start"`,                // durable journal boundary
		"apt-get install -y something-host-specific",         // bootstrap hook
		"docker version --format '{{.Server.Version}}'",      // prerequisites after authored provisioning
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

func TestConcurrentBootstrapDoesNotRunSecondHook(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\n[ \"$1\" = version ] && printf '27.0.3\\n'\n[ \"$1\" = compose ] && [ \"$2\" = version ] && printf '2.29.1\\n'\n[ \"$1\" = buildx ] && [ \"$2\" = imagetools ] && printf -- '--format string\\n'\n[ \"$1\" = buildx ] && [ \"$2\" = version ] && printf 'buildx v0.33.0\\n'\n[ \"$1\" = network ] && [ \"$2\" = inspect ] && exit 1\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	entered := filepath.Join(dir, "hook-entered")
	release := filepath.Join(dir, "release-hook")
	runs := filepath.Join(dir, "hook-runs")
	cfg := testConfig()
	cfg.BasePath = filepath.Join(dir, "state")
	cfg.Services = nil
	cfg.Hooks["bootstrap"] = app.Command{Run: "printf x >> " + q(runs) + "; touch " + q(entered) + "; while [ ! -f " + q(release) + " ]; do sleep 0.01; done"}

	local := &bootstrapNetworkLocal{Local: transport.NewLocal(), owner: cfg.Name}
	first := New(cfg, testProject(t), local, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	second := New(cfg, testProject(t), local, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.Bootstrap(context.Background(), engineTestBootstrapReleaseID)
	}()
	defer func() { _ = os.WriteFile(release, nil, 0o600) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("first bootstrap did not enter its hook")
		}
		time.Sleep(10 * time.Millisecond)
	}

	err := second.Bootstrap(context.Background(), engineTestDeployReleaseID)
	if err == nil || !strings.Contains(err.Error(), "deploy lock held") {
		t.Fatalf("concurrent bootstrap error = %v, want held lock", err)
	}
	body, readErr := os.ReadFile(runs)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "x" {
		t.Fatalf("bootstrap hooks ran concurrently: %q", body)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first bootstrap: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first bootstrap did not finish")
	}
}

func TestBootstrapRefusesMissingRuntimeWithoutImplicitInstaller(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker version") {
			return transport.Result{ExitCode: 127, Stderr: "docker: command not found"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID)
	if err == nil || !strings.Contains(err.Error(), "after the bootstrap hook") ||
		!strings.Contains(err.Error(), "container runtime unavailable") ||
		!strings.Contains(err.Error(), "install Docker") || !strings.Contains(err.Error(), "remote bootstrap hook") {
		t.Fatalf("missing runtime error = %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	for _, forbidden := range []string{"get.docker.com", "curl -fsSL", "systemctl enable", "apt-get install", "dnf install", "yum install", "apk add", "docker login", "docker compose", "mkdir -m 700"} {
		if strings.Contains(seq, forbidden) {
			t.Fatalf("bootstrap ran implicit installer %q:\n%s", forbidden, seq)
		}
	}
	runtimeCheck := strings.Index(seq, "docker version")
	if runtimeCheck < 0 {
		t.Fatalf("bootstrap did not check the runtime:\n%s", seq)
	}
	for _, before := range []string{"> '/var/lib/ob/sample/lock'", "> '/var/lib/ob/sample/fence'", `"phase":"bootstrap","event":"start"`} {
		if index := strings.Index(seq, before); index < 0 || index > runtimeCheck {
			t.Fatalf("%q did not precede the runtime check:\n%s", before, seq)
		}
	}
}

func TestBootstrapHookMayProvisionPinnedRuntime(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	installed := false
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "install-pinned-docker") {
			installed = true
			return transport.Result{}, true
		}
		if strings.Contains(cmd, "docker version") {
			if !installed {
				return transport.Result{ExitCode: 127, Stderr: "docker: command not found"}, true
			}
			return transport.Result{Stdout: "27.0.3\n"}, true
		}
		return base(cmd)
	}
	cfg := testConfig()
	cfg.Hooks["bootstrap"] = app.Command{Run: "install-pinned-docker"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID); err != nil {
		t.Fatalf("bootstrap after authored runtime provisioning: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
}

func TestBootstrapUsesPresentRuntimeWithoutInstaller(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "get.docker.com") || strings.Contains(seq, "systemctl enable") {
		t.Fatal("must not install a present runtime")
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
	if err := os.WriteFile(filepath.Join(dir, "traefik", "traefik.yml"), []byte(testManagedProxyStatic), 0o600); err != nil {
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

// Bootstrap asserted only the container runtime, so a host with a working
// daemon and no Compose plugin — Ubuntu's `docker.io` package, for one —
// completed bootstrap and failed two commands later. The gate now asserts the
// same set every other gate does.
func TestBootstrapRefusesHostMissingComposePlugin(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "compose version") {
			return transport.Result{ExitCode: 125, Stderr: "docker: 'compose' is not a docker command"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID)
	if err == nil || !strings.Contains(err.Error(), app.PrerequisiteCompose) ||
		!strings.Contains(err.Error(), "after the bootstrap hook") {
		t.Fatalf("missing compose error = %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	// The authored hook still gets its chance first: refusing before it runs
	// would defeat the pinned-installer escape hatch it exists for.
	seq := strings.Join(f.Commands, "\n")
	for _, forbidden := range []string{"get.docker.com", "curl -fsSL", "apt-get install", "docker login"} {
		if strings.Contains(seq, forbidden) {
			t.Fatalf("bootstrap ran %q rather than refusing:\n%s", forbidden, seq)
		}
	}
}

// A dropped connection is not a missing prerequisite. Framing one as the other
// answers "the SSH session reset" with "declare a pinned installer hook", which
// sends an operator to provision software that may already be installed.
func TestBootstrapSeparatesAnUnreachableServerFromAnUnmetPrerequisite(t *testing.T) {
	f := happyFake()
	f.Err = func(cmd string) error {
		if strings.Contains(cmd, "docker version") {
			return errors.New("ssh: connection reset by peer")
		}
		return nil
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("error = %v, want the transport failure", err)
	}
	if strings.Contains(err.Error(), "bootstrap hook") || strings.Contains(err.Error(), "install") {
		t.Fatalf("a transport failure must not be reported as a prerequisite to provision: %v", err)
	}
}

// A typed failure prints its code first, the way every other one does. Wrapping
// prose around the rendered error buried `host_prerequisite_unmet:` in the
// middle of the sentence an operator reads on a fresh host.
func TestBootstrapRefusalReadsAsOneSentence(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "compose version") {
			return transport.Result{ExitCode: 125, Stderr: "docker: 'compose' is not a docker command"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Bootstrap(context.Background(), engineTestBootstrapReleaseID)
	if err == nil {
		t.Fatal("a missing Compose plugin must refuse")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "host_prerequisite_unmet: host is not deployable") {
		t.Fatalf("refusal does not lead with its code and then the sentence: %q", message)
	}
	if strings.Count(message, "host_prerequisite_unmet") != 1 {
		t.Fatalf("the code appears more than once: %q", message)
	}
	var typed *app.Error
	if !errors.As(err, &typed) || typed.Next != "ob preflight" {
		t.Fatalf("the restated refusal lost its type or command: %v", err)
	}
}
