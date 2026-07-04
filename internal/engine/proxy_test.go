package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

// proxyFixture: a managed-proxy engine whose LocalDir holds traefik/{traefik.yml,.env},
// plus the staged payload hash the engine will compute for it.
func proxyFixture(t *testing.T, f *transport.Fake) (*Engine, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"traefik.yml": "ping: {}\n",
		".env":        "CF_DNS_API_TOKEN=supersecrettoken\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "traefik", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	cfg.Proxy = config.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	hash, err := proxy.Stage(filepath.Join(dir, "traefik"), t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep, LocalDir: dir})
	return e, hash, &out
}

// proxyPS answers container queries for the yeet-proxy project: present only
// after `up -d` (or from the start when preRunning).
func proxyPS(f *transport.Fake, preRunning bool) func(string) (transport.Result, bool) {
	upRan := func() bool {
		for _, c := range f.Commands {
			if strings.Contains(c, "up -d") {
				return true
			}
		}
		return false
	}
	return func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "project=yeet-proxy") {
			if preRunning || upRan() {
				return transport.Result{Stdout: "PX1\n"}, true
			}
			return transport.Result{Stdout: ""}, true
		}
		if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "PX1") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		return transport.Result{}, false
	}
}

func TestEnsureProxyFreshHost(t *testing.T) {
	f := &transport.Fake{}
	f.Dynamic = proxyPS(f, false)
	e, hash, _ := proxyFixture(t, f)
	if err := e.EnsureProxy(context.Background(), "R1", false); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if len(f.Uploads) != 1 || !strings.Contains(f.Uploads[0], "/var/lib/yeet/_host/proxy") {
		t.Fatalf("payload must upload to the host proxy dir: %v", f.Uploads)
	}
	if !strings.Contains(seq, "docker compose -p yeet-proxy -f '/var/lib/yeet/_host/proxy/compose.yaml' up -d") {
		t.Fatalf("fresh host must up the proxy project:\n%s", seq)
	}
	if !strings.Contains(seq, "echo '"+hash+"' > '/var/lib/yeet/_host/proxy/apps/monk'") {
		t.Fatalf("app must register with its config hash:\n%s", seq)
	}
	if !strings.Contains(seq, "test -f '/var/lib/yeet/_host/proxy/acme/acme.json' ||") {
		t.Fatalf("acme.json creation must be guarded (never touch an existing one):\n%s", seq)
	}
	if !strings.Contains(seq, "/var/lib/yeet/_host/journal") {
		t.Fatalf("host journal must record the converge:\n%s", seq)
	}
	// secrets rule: .env content never appears in any command
	if strings.Contains(seq, "supersecrettoken") {
		t.Fatalf(".env content leaked into a command:\n%s", seq)
	}
}

func TestEnsureProxyUnchangedIsNoOp(t *testing.T) {
	f := &transport.Fake{}
	e, hash, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "cat '/var/lib/yeet/_host/proxy/config.hash'") {
			return transport.Result{Stdout: hash + "\n"}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "R2", false); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "up -d") || strings.Contains(seq, "docker restart") {
		t.Fatalf("unchanged proxy must not be touched (ACME safety):\n%s", seq)
	}
	if len(f.Uploads) != 0 {
		t.Fatalf("unchanged proxy must not re-upload: %v", f.Uploads)
	}
	if !strings.Contains(seq, "echo '"+hash+"' > '/var/lib/yeet/_host/proxy/apps/monk'") {
		t.Fatalf("registration must still happen:\n%s", seq)
	}
}

func TestEnsureProxyConfigOnlyChangeRestarts(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	// remote compose identical to what we render; only the config hash differs
	rendered := string(proxy.RenderCompose("", "", true))
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "cat '/var/lib/yeet/_host/proxy/config.hash'") {
			return transport.Result{Stdout: "deadbeef\n"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/_host/proxy/compose.yaml'") {
			return transport.Result{Stdout: rendered}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "R3", false); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	// converge by observation: up -d is the no-op probe (container matches the
	// compose file, so it doesn't recreate) and the restart loads the config
	iUp := strings.Index(seq, "up -d")
	iRestart := strings.Index(seq, "docker restart yeet-proxy")
	if iUp < 0 || iRestart < 0 || iRestart < iUp {
		t.Fatalf("config-only change must up (no-op) then restart:\n%s", seq)
	}
	if len(f.Uploads) != 1 {
		t.Fatalf("changed config must upload: %v", f.Uploads)
	}
	// the config swap must be staged: never a bare upload into the live dir
	if !strings.Contains(f.Uploads[0], ".staged") {
		t.Fatalf("upload must land in the staging dir, not the live one: %v", f.Uploads)
	}
	if !strings.Contains(seq, "mv '/var/lib/yeet/_host/proxy/.staged/config' '/var/lib/yeet/_host/proxy/config'") {
		t.Fatalf("config must swap in atomically:\n%s", seq)
	}
	// applied-state marker written ONLY after health confirms — an interrupted
	// converge must be retried, never mistaken for "unchanged"
	iHealth := strings.LastIndex(seq, "docker inspect")
	iHash := strings.Index(seq, "> '/var/lib/yeet/_host/proxy/config.hash'")
	if iHash < 0 || iHash < iHealth {
		t.Fatalf("config.hash must be written after the health check:\n%s", seq)
	}
}

func TestEnsureProxyFailedConvergeLeavesHashUnwritten(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	rendered := string(proxy.RenderCompose("", "", true))
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "cat '/var/lib/yeet/_host/proxy/config.hash'") {
			return transport.Result{Stdout: "deadbeef\n"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/_host/proxy/compose.yaml'") {
			return transport.Result{Stdout: rendered}, true
		}
		if strings.Contains(cmd, "docker restart") {
			return transport.Result{ExitCode: 1, Stderr: "cannot restart"}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "R7", false); err == nil {
		t.Fatal("failed restart must error")
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "> '/var/lib/yeet/_host/proxy/config.hash'") {
		t.Fatalf("failed converge must NOT record the applied hash (retry depends on it):\n%s", seq)
	}
}

func TestEnsureProxyConflictNamesApps(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "ls -1 '/var/lib/yeet/_host/proxy/apps'") {
			return transport.Result{Stdout: "monk\nunlock\n"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/_host/proxy/apps/unlock'") {
			return transport.Result{Stdout: "0123456789abcdef\n"}, true
		}
		return ps(cmd)
	}
	err := e.EnsureProxy(context.Background(), "R4", false)
	if err == nil || !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("divergent config must conflict naming the other app, got %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "up -d") || strings.Contains(seq, "docker restart") || len(f.Uploads) != 0 {
		t.Fatalf("conflict must stop before any mutation:\n%s", seq)
	}

	// --force proceeds
	f2 := &transport.Fake{}
	e2, _, _ := proxyFixture(t, f2)
	ps2 := proxyPS(f2, true)
	f2.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "ls -1 '/var/lib/yeet/_host/proxy/apps'") {
			return transport.Result{Stdout: "unlock\n"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/_host/proxy/apps/unlock'") {
			return transport.Result{Stdout: "0123456789abcdef\n"}, true
		}
		return ps2(cmd)
	}
	if err := e2.EnsureProxy(context.Background(), "R5", true); err != nil {
		t.Fatalf("force must proceed past the conflict: %v", err)
	}
}

func TestEnsureProxyLockReleasedOnConflict(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "ls -1 '/var/lib/yeet/_host/proxy/apps'") {
			return transport.Result{Stdout: "unlock\n"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/_host/proxy/apps/unlock'") {
			return transport.Result{Stdout: "0123456789abcdef\n"}, true
		}
		return ps(cmd)
	}
	_ = e.EnsureProxy(context.Background(), "R6", false)
	if !strings.Contains(strings.Join(f.Commands, "\n"), "rm -f '/var/lib/yeet/_host/lock'") {
		t.Fatalf("host lock must be released on error:\n%s", strings.Join(f.Commands, "\n"))
	}
}
