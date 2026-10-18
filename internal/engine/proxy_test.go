package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

const testManagedProxyStatic = "ping: {}\nproviders:\n  file:\n    directory: /etc/traefik/dynamic\ncertificatesResolvers:\n  " + app.ManagedCertificateResolver + ":\n    acme:\n      storage: /letsencrypt/acme.json\n      httpChallenge:\n        entryPoint: web\n"

// proxyFixture: a managed-proxy engine whose LocalDir holds traefik/{traefik.yml,.env},
// plus the staged payload hash the engine will compute for it.
func proxyFixture(t *testing.T, f *transport.Fake) (*Engine, proxy.Identity, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"traefik.yml": testManagedProxyStatic,
		".env":        "CF_DNS_API_TOKEN=supersecrettoken\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "traefik", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	hash, err := proxy.StageForApp(filepath.Join(dir, "traefik"), t.TempDir(), "",
		proxy.DiscoveryImage("dev"), cfg.Name, "", nil, cfg.HasTerminatingTLS())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep, LocalDir: dir})
	return e, hash, &out
}

func TestEnsureProxyRefusesMissingResolverBeforeHostMutation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}
	static := "ping: {}\nproviders:\n  file:\n    directory: /etc/traefik/dynamic\n"
	if err := os.WriteFile(filepath.Join(dir, "traefik", "traefik.yml"), []byte(static), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	web := cfg.Workloads["web"]
	web.Routes = []app.Route{{Hostname: "app.example.com", Port: 7500, Path: "/", Entrypoint: "websecure", Protocol: "http", Scheme: "http", TLS: "terminate"}}
	cfg.Workloads["web"] = web
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	f := &transport.Fake{}
	e := New(cfg, testProject(t), f, Options{Sleep: noSleep, LocalDir: dir})
	err := e.EnsureProxy(context.Background(), "D1", false)
	var missing *proxy.CertificateResolverMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("missing resolver must be refused with its typed error: %v", err)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("resolver contract must be checked before host mutation: %v", f.Commands)
	}
}

// proxyPS answers container queries for the onebox-proxy project: present only
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
		if cmd == outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml") {
			return transport.Result{Stdout: "12\n"}, true
		}
		if strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "project='onebox-proxy'") {
			if preRunning || upRan() {
				if strings.Contains(cmd, "service=discovery") {
					return transport.Result{Stdout: "DISC1\n"}, true
				}
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

// legacyProxyPS models the upgrade boundary: the old Traefik container is
// healthy, but the new discovery service does not exist until convergence.
func legacyProxyPS(f *transport.Fake) func(string) (transport.Result, bool) {
	discoveryStarted := func() bool {
		for _, command := range f.Commands {
			if strings.Contains(command, "up -d --force-recreate discovery") {
				return true
			}
		}
		return false
	}
	return func(command string) (transport.Result, bool) {
		if strings.Contains(command, "docker ps -q") && strings.Contains(command, "project='onebox-proxy'") {
			switch {
			case strings.Contains(command, "service=discovery"):
				if discoveryStarted() {
					return transport.Result{Stdout: "DISC1\n"}, true
				}
				return transport.Result{}, true
			case strings.Contains(command, "service=proxy"):
				return transport.Result{Stdout: "PX1\n"}, true
			}
		}
		if strings.Contains(command, "docker inspect") && strings.Contains(command, "PX1") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		return transport.Result{}, false
	}
}

func TestEnsureProxyFreshHost(t *testing.T) {
	f := &transport.Fake{}
	f.Dynamic = proxyPS(f, false)
	e, _, _ := proxyFixture(t, f)
	if err := e.EnsureProxy(context.Background(), "R1", false); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if len(f.Uploads) != 1 || !strings.Contains(f.Uploads[0], "/var/lib/onebox/_host/proxy") {
		t.Fatalf("payload must upload to the host proxy dir: %v", f.Uploads)
	}
	if !strings.Contains(seq, "docker compose -p onebox-proxy -f '/var/lib/onebox/_host/proxy/compose.yaml' up -d") {
		t.Fatalf("fresh host must up the proxy project:\n%s", seq)
	}
	if strings.Contains(seq, "/proxy/apps") {
		t.Fatalf("proxy convergence must not create a cross-application registry:\n%s", seq)
	}
	if !strings.Contains(seq, "test -f '/var/lib/onebox/_host/proxy/acme/acme.json' ||") {
		t.Fatalf("acme.json creation must be guarded (never touch an existing one):\n%s", seq)
	}
	if !strings.Contains(seq, "/var/lib/onebox/_host/journal") {
		t.Fatalf("host journal must record the converge:\n%s", seq)
	}
	// secrets rule: .env content never appears in any command
	if strings.Contains(seq, "supersecrettoken") {
		t.Fatalf(".env content leaked into a command:\n%s", seq)
	}
}

func TestEnsureProxyLogsShortDigests(t *testing.T) {
	f := &transport.Fake{}
	f.Dynamic = proxyPS(f, false)
	e, id, out := proxyFixture(t, f)
	if err := e.EnsureProxy(context.Background(), "R1", false); err != nil {
		t.Fatal(err)
	}
	want := "proxy: healthy at config " + strings.TrimPrefix(id.Config, "sha256:")[:8] +
		" compose " + strings.TrimPrefix(id.Compose, "sha256:")[:8]
	for _, line := range strings.Split(out.String(), "\n") {
		if start := strings.Index(line, "proxy: healthy at config "); start >= 0 {
			if line[start:] != want {
				t.Fatalf("healthy log = %q, want %q", line, want)
			}
			return
		}
	}
	t.Fatalf("healthy log missing from output:\n%s", out.String())
}

func TestEnsureProxyStopsWhenJournalStartFails(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	ps := proxyPS(f, false)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, `"phase":"proxy-apply","event":"start"`) {
			return transport.Result{ExitCode: 74, Stderr: "journal is read-only"}, true
		}
		return ps(cmd)
	}
	err := e.EnsureProxy(context.Background(), "R1", false)
	if err == nil || !strings.Contains(err.Error(), "journal proxy apply start") {
		t.Fatalf("proxy apply error = %v", err)
	}
	if len(f.Uploads) != 0 || strings.Contains(strings.Join(f.Commands, "\n"), "up -d") {
		t.Fatalf("proxy converged after journal failure: uploads=%v\n%s", f.Uploads, strings.Join(f.Commands, "\n"))
	}
}

func TestEnsureProxyUnchangedIsNoOp(t *testing.T) {
	f := &transport.Fake{}
	e, hash, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if cmd == "rm -f '/var/lib/onebox/_host/proxy/config.hash'" && len(f.Uploads) != 0 {
			t.Fatal("marker invalidated after upload")
		}
		if strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/config.hash'") {
			return transport.Result{Stdout: hash.String() + "\n"}, true
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
	if strings.Contains(seq, "/proxy/apps") {
		t.Fatalf("unchanged proxy must not create a cross-application registry:\n%s", seq)
	}
}

func TestEnsureProxyRepairsMissingDiscoveryController(t *testing.T) {
	f := &transport.Fake{}
	e, hash, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/config.hash'"):
			return transport.Result{Stdout: hash.String() + "\n"}, true
		case strings.Contains(cmd, "com.docker.compose.service=discovery") && !strings.Contains(cmd, "up -d"):
			return transport.Result{Stdout: ""}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "R2-repair", false); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "up -d --force-recreate discovery") {
		t.Fatalf("missing discovery controller was treated as an unchanged proxy:\n%s", strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "docker restart") || strings.Contains(seq, "/config' '/var/lib/onebox/_host/proxy/config'") {
		t.Fatalf("discovery repair must preserve Traefik and its config:\n%s", seq)
	}
}

func TestEnsureProxyRepairsMissingDiscoveryOutput(t *testing.T) {
	f := &transport.Fake{}
	e, hash, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/config.hash'"):
			return transport.Result{Stdout: hash.String() + "\n"}, true
		case cmd == outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml"):
			for _, command := range f.Commands {
				if strings.Contains(command, "up -d --force-recreate discovery") {
					return transport.Result{}, true
				}
			}
			return transport.Result{}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "R2-repair-output", false); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "up -d --force-recreate discovery") {
		t.Fatalf("missing discovery output was treated as an unchanged proxy:\n%s", strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "docker restart") || strings.Contains(seq, "/config' '/var/lib/onebox/_host/proxy/config'") {
		t.Fatalf("discovery repair must preserve Traefik and its config:\n%s", seq)
	}
}

func TestEnsureProxyConfigOnlyChangeRestarts(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	// remote compose identical to what we render; only the config hash differs
	rendered := string(proxy.RenderComposeForApp("", proxy.DiscoveryImage("dev"), "sample", "", true, nil))
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if cmd == "rm -f '/var/lib/onebox/_host/proxy/config.hash'" && len(f.Uploads) != 0 {
			t.Fatal("marker invalidated after upload")
		}
		if strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/config.hash'") {
			return transport.Result{Stdout: "deadbeef\n"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'") {
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
	iRestart := strings.Index(seq, "docker restart onebox-proxy")
	if iUp < 0 || iRestart < 0 || iRestart < iUp {
		t.Fatalf("config-only change must up (no-op) then restart:\n%s", seq)
	}
	if !strings.Contains(seq, "rm -f '/var/lib/onebox/_host/proxy/config.hash'") {
		t.Fatalf("marker must be invalidated before uploading:\n%s", seq)
	}
	if len(f.Uploads) != 1 {
		t.Fatalf("changed config must upload: %v", f.Uploads)
	}
	// the config swap must be staged: never a bare upload into the live dir
	if !strings.Contains(f.Uploads[0], ".staged") {
		t.Fatalf("upload must land in the staging dir, not the live one: %v", f.Uploads)
	}
	if !strings.Contains(seq, "mv '/var/lib/onebox/_host/proxy/.staged-") || !strings.Contains(seq, "/config' '/var/lib/onebox/_host/proxy/config'") {
		t.Fatalf("config must swap in atomically:\n%s", seq)
	}
	// applied-state marker written ONLY after health confirms — an interrupted
	// converge must be retried, never mistaken for "unchanged"
	iHealth := strings.LastIndex(seq, "docker inspect")
	iHash := strings.Index(seq, "mv -f '/var/lib/onebox/_host/proxy/config.hash.tmp'")
	if iHash < 0 || iHash < iHealth || !strings.Contains(seq, "printf '%s\\n'") {
		t.Fatalf("config.hash must be written after the health check:\n%s", seq)
	}
}

func TestEnsureProxyFailedConvergeLeavesHashUnwritten(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	rendered := string(proxy.RenderComposeForApp("", proxy.DiscoveryImage("dev"), "sample", "", true, nil))
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if cmd == "rm -f '/var/lib/onebox/_host/proxy/config.hash'" && len(f.Uploads) != 0 {
			t.Fatal("marker invalidated after upload")
		}
		if strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/config.hash'") {
			return transport.Result{Stdout: "deadbeef\n"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'") {
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
	if !strings.Contains(seq, "rm -f '/var/lib/onebox/_host/proxy/config.hash'") || strings.Contains(seq, "config.hash.tmp") {
		t.Fatalf("failed converge must NOT record the applied hash (retry depends on it):\n%s", seq)
	}
}

func TestProxyApplyRefusesForeignOwnerBeforeMutation(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "_host/owner") {
			return transport.Result{Stdout: "another-app production\n"}, true
		}
		return transport.Result{}, false
	}
	e.Opts.ForceLock = true
	err := e.ProxyApply(context.Background(), "R4")
	if err == nil || !strings.Contains(err.Error(), "another-app") {
		t.Fatalf("foreign host owner was accepted: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "up -d") || strings.Contains(seq, "docker restart") || len(f.Uploads) != 0 {
		t.Fatalf("foreign owner must stop before proxy mutation:\n%s", seq)
	}
}

func TestEnsureProxyReleasesHostLock(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	f.Dynamic = proxyPS(f, false)
	_ = e.EnsureProxy(context.Background(), "R6", false)
	if !strings.Contains(strings.Join(f.Commands, "\n"), "rm -f '/var/lib/onebox/_host/lock'") {
		t.Fatalf("host lock must be released on error:\n%s", strings.Join(f.Commands, "\n"))
	}
}

// The host journal ages out by name, in one command, whatever its files hold:
// a torn record must not stop pruning for the life of the host.
func TestHostJournalPrunesOldestFiles(t *testing.T) {
	dir := t.TempDir()
	for i := range 25 {
		body := `{"phase":"proxy-apply"}` + "\n"
		if i == 3 {
			body = `{"phase":"pro` // torn
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("20260901-1200%02d-nogit-proxy.jsonl", i)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "sh", "-c", pruneHostJournalCommand(dir, 20)).CombinedOutput(); err != nil {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 21 || names[0] != "20260901-120005-nogit-proxy.jsonl" || names[20] != "unrelated.txt" {
		t.Fatalf("after pruning: %v", names)
	}
	if out, err := exec.CommandContext(t.Context(), "sh", "-c", pruneHostJournalCommand(filepath.Join(dir, "absent"), 20)).CombinedOutput(); err != nil {
		t.Fatalf("an absent journal is nothing to prune: %v\n%s", err, out)
	}
}

func TestEnsureProxyComposeOnlyChangeSkipsSwapAndRestart(t *testing.T) {
	f := &transport.Fake{}
	e, id, _ := proxyFixture(t, f)
	id.Compose = "sha256:" + strings.Repeat("0", 64)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'" {
			return transport.Result{Stdout: id.String()}, true
		}
		if strings.Contains(cmd, "service=discovery") {
			name := "D1"
			if strings.Contains(strings.Join(f.Commands, "\n"), "up -d discovery") {
				name = "D2"
			}
			return transport.Result{Stdout: name}, true
		}
		if cmd == outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml") {
			return transport.Result{Stdout: "100"}, true
		}
		if strings.HasPrefix(cmd, "i=0;") {
			if !strings.Contains(cmd, "100") || !strings.Contains(cmd, "stat -c %i") {
				t.Fatalf("publication must require a fresh inode: %s", cmd)
			}
			return transport.Result{}, true // inode 101 was published
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "compose-change", false); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	for _, forbidden := range []string{"/config' '/var/lib/onebox/_host/proxy/config'", "docker restart", "--force-recreate"} {
		if strings.Contains(seq, forbidden) {
			t.Fatalf("unexpected %s:\n%s", forbidden, seq)
		}
	}
	if !strings.Contains(seq, "/compose.yaml' '/var/lib/onebox/_host/proxy/compose.yaml'") || !strings.Contains(seq, "i=0;") {
		t.Fatalf("compose and publication must converge:\n%s", seq)
	}
	iMarker := strings.LastIndex(seq, "mv -f '/var/lib/onebox/_host/proxy/config.hash.tmp'")
	if iMarker < 0 || iMarker < strings.LastIndex(seq, "docker inspect") {
		t.Fatalf("marker precedes health:\n%s", seq)
	}
	if iMarker < strings.LastIndex(seq, "i=0;") {
		t.Fatalf("marker precedes publication:\n%s", seq)
	}
}

func TestEnsureProxyMissingOutputForceRecreatesDiscoveryWithoutRestart(t *testing.T) {
	f := &transport.Fake{}
	e, id, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	inode := ""
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'" {
			return transport.Result{Stdout: id.String()}, true
		}
		if strings.Contains(cmd, "up -d --force-recreate discovery") {
			inode = "200"
			return transport.Result{}, true
		}
		if cmd == outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml") {
			return transport.Result{Stdout: inode}, true
		}
		if strings.HasPrefix(cmd, "i=0;") {
			if inode == "" {
				return transport.Result{ExitCode: 1}, true
			}
			return transport.Result{}, true // inode 200 was published
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "missing-output", false); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "up -d --force-recreate discovery") || !strings.Contains(seq, "i=0;") {
		t.Fatalf("missing output must force discovery and wait for publication:\n%s", seq)
	}
	if strings.Contains(seq, "docker restart") || strings.Contains(seq, "/config' '/var/lib/onebox/_host/proxy/config'") {
		t.Fatalf("missing output must not swap config or restart Traefik:\n%s", seq)
	}
}

func TestEnsureProxyComposePathRejectsStaleOutput(t *testing.T) {
	f := &transport.Fake{}
	e, id, _ := proxyFixture(t, f)
	live := string(testProxyCompose(t, e))
	staged := id
	id.Compose = "sha256:" + strings.Repeat("0", 64)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'" {
			return transport.Result{Stdout: id.String()}, true
		}
		if strings.HasPrefix(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'") {
			return transport.Result{Stdout: live}, true
		}
		if strings.Contains(cmd, "service=discovery") {
			name := "D1"
			if strings.Contains(strings.Join(f.Commands, "\n"), "up -d discovery") {
				name = "D2"
			}
			return transport.Result{Stdout: name}, true
		}
		if cmd == outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml") {
			return transport.Result{Stdout: "100"}, true
		}
		if strings.HasPrefix(cmd, "i=0;") {
			return transport.Result{ExitCode: 1}, true
		}
		return ps(cmd)
	}
	err := e.EnsureProxy(context.Background(), "stale", false)
	if err == nil || !strings.Contains(err.Error(), "did not publish") || !strings.Contains(err.Error(), "docker logs onebox-discovery") {
		t.Fatalf("stale output error: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	iPending := strings.Index(seq, testProxyIdentityWrite(id))
	iMutation := strings.Index(seq, "rm -rf '/var/lib/onebox/_host/proxy/.staged-")
	if iPending < 0 || iMutation < 0 || iPending > iMutation ||
		strings.Contains(seq, "rm -f '/var/lib/onebox/_host/proxy/config.hash'") ||
		strings.Contains(seq, testProxyIdentityWrite(staged)) {
		t.Fatalf("failed converge must atomically mark pending before mutation without committing the staged identity:\n%s", seq)
	}
}

func testProxyIdentityWrite(id proxy.Identity) string {
	path := "/var/lib/onebox/_host/proxy/config.hash"
	return "printf '%s\\n' " + q(id.String()) + " > " + q(path+".tmp") +
		" && mv -f " + q(path+".tmp") + " " + q(path)
}

func TestEnsureProxyFailedConvergeRetriesAsConverge(t *testing.T) {
	f := &transport.Fake{}
	e, id, _ := proxyFixture(t, f)
	live := string(testProxyCompose(t, e))
	applied := id
	applied.Compose = "sha256:" + strings.Repeat("a", 64)
	pending := proxy.Identity{Config: applied.Config, Compose: "sha256:" + strings.Repeat("0", 64)}
	marker := applied.String()
	fail := true
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'":
			return transport.Result{Stdout: marker}, true
		case strings.HasPrefix(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'"):
			return transport.Result{Stdout: live}, true
		case strings.Contains(cmd, testProxyIdentityWrite(pending)):
			marker = pending.String()
			return transport.Result{}, true
		case strings.Contains(cmd, testProxyIdentityWrite(id)):
			marker = id.String()
			return transport.Result{}, true
		case strings.Contains(cmd, "rm -f '/var/lib/onebox/_host/proxy/config.hash'"):
			marker = ""
			return transport.Result{}, true
		}
		if strings.Contains(cmd, "rm -rf '/var/lib/onebox/_host/proxy/.staged-") && marker != pending.String() {
			t.Error("converge mutated staging before atomically recording the pending identity")
		}
		if strings.Contains(cmd, "up -d discovery") && fail {
			return transport.Result{ExitCode: 1, Stderr: "cannot start discovery"}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "failed-converge", false); err == nil {
		t.Fatal("failed discovery up must error")
	}
	if marker != pending.String() || strings.Contains(strings.Join(f.Commands, "\n"), testProxyIdentityWrite(id)) {
		t.Fatalf("failed converge must keep only the pending identity: %q", marker)
	}
	beforeRetry := len(f.Commands)
	fail = false
	if err := e.EnsureProxy(context.Background(), "retry-converge", false); err != nil {
		t.Fatal(err)
	}
	retry := strings.Join(f.Commands[beforeRetry:], "\n")
	for _, forbidden := range []string{"docker restart", "/config' '/var/lib/onebox/_host/proxy/config'", "rm -f '/var/lib/onebox/_host/proxy/config.hash'"} {
		if strings.Contains(retry, forbidden) {
			t.Fatalf("retry must remain a converge, found %s:\n%s", forbidden, retry)
		}
	}
	if marker != id.String() || !strings.Contains(retry, "i=0;") {
		t.Fatalf("retry must publish before committing the final identity:\n%s", retry)
	}
}

func TestEnsureProxyConvergeStopsDiscoveryBeforeSampling(t *testing.T) {
	f := &transport.Fake{}
	e, id, _ := proxyFixture(t, f)
	id.Compose = "sha256:" + strings.Repeat("a", 64)
	ps := proxyPS(f, true)
	stopped, started := false, false
	inodeCommand := outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml")
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'" {
			return transport.Result{Stdout: id.String()}, true
		}
		if strings.Contains(cmd, " stop discovery") {
			stopped = true
			return transport.Result{}, true
		}
		if cmd == inodeCommand {
			if stopped {
				return transport.Result{Stdout: "101"}, true
			}
			return transport.Result{Stdout: "100"}, true
		}
		if strings.Contains(cmd, "up -d discovery") {
			started = true
			return transport.Result{}, true
		}
		if strings.HasPrefix(cmd, "i=0;") {
			if !stopped || !started || !strings.Contains(cmd, "!= '101'") {
				t.Fatalf("publication must use the inode sampled with discovery stopped: %s", cmd)
			}
			return transport.Result{}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "fresh-publication", false); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	iStop, iSample, iUp := strings.Index(seq, " stop discovery"), strings.LastIndex(seq, "\n"+inodeCommand+"\n"), strings.Index(seq, " up -d discovery")
	if iStop < 0 || iSample < iStop || iUp < iSample || !strings.Contains(seq, "i=0;") {
		t.Fatalf("converge must stop, sample, start and await publication even when the discovery id stays the same:\n%s", seq)
	}
}

type proxyUploadObserver struct {
	*transport.Fake
	inspect func(string)
}

func (f *proxyUploadObserver) Upload(ctx context.Context, localDir, remoteDir string) error {
	f.inspect(localDir)
	return f.Fake.Upload(ctx, localDir, remoteDir)
}

func TestEnsureProxyConvergeUploadsOnlyCompose(t *testing.T) {
	f := &transport.Fake{}
	e, id, _ := proxyFixture(t, f)
	id.Compose = "sha256:" + strings.Repeat("a", 64)
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'" {
			return transport.Result{Stdout: id.String()}, true
		}
		return ps(cmd)
	}
	inspected := false
	e.T = &proxyUploadObserver{Fake: f, inspect: func(dir string) {
		inspected = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "compose.yaml" || entries[0].IsDir() {
			t.Fatalf("converge upload must hold only compose.yaml: %v", entries)
		}
	}}
	if err := e.EnsureProxy(context.Background(), "compose-upload", false); err != nil {
		t.Fatal(err)
	}
	if !inspected {
		t.Fatal("converge did not upload compose")
	}
}

func TestEnsureProxyConfigSwapRetiresLiveDirsInsteadOfDeleting(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	f.Dynamic = proxyPS(f, true)
	if err := e.EnsureProxy(context.Background(), "retire-config", false); err != nil {
		t.Fatal(err)
	}
	staged := strings.SplitN(f.Uploads[0], " -> ", 2)[1]
	seq := strings.Join(f.Commands, "\n")
	for _, dir := range []string{"config", "dynamic"} {
		live := "/var/lib/onebox/_host/proxy/" + dir
		if strings.Contains(seq, "rm -rf "+q(live)) || !strings.Contains(seq, "mv "+q(live)+" "+q(staged+"/retired/"+dir)) {
			t.Fatalf("live %s must be retired without deleting its bind-mounted contents:\n%s", dir, seq)
		}
	}
	iCleanup := strings.LastIndex(seq, "-name '.staged-*'")
	if iCleanup < strings.LastIndex(seq, "docker inspect") || iCleanup < strings.LastIndex(seq, "config.hash.tmp") {
		t.Fatalf("retired directories must survive until health and the final marker:\n%s", seq)
	}
}

func TestEnsureProxyFailedConfigApplyKeepsRetiredDirsUntilRetrySucceeds(t *testing.T) {
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	fail := true
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker restart") && fail {
			return transport.Result{ExitCode: 1, Stderr: "cannot restart"}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "failed-config", false); err == nil {
		t.Fatal("failed restart must error")
	}
	staged := strings.SplitN(f.Uploads[0], " -> ", 2)[1]
	first := strings.Join(f.Commands, "\n")
	iSwap := strings.Index(first, "/config' '/var/lib/onebox/_host/proxy/config'")
	if iSwap < 0 {
		t.Fatal("config was not swapped")
	}
	if strings.Contains(first, "-name '.staged-*'") || strings.Contains(first[iSwap:], "rm -rf "+q(staged)) {
		t.Fatalf("failed apply must preserve the retired directories:\n%s", first)
	}
	beforeRetry := len(f.Commands)
	fail = false
	if err := e.EnsureProxy(context.Background(), "retry-config", false); err != nil {
		t.Fatal(err)
	}
	retry := strings.Join(f.Commands[beforeRetry:], "\n")
	iCleanup := strings.Index(retry, "-name '.staged-*'")
	if iCleanup < strings.LastIndex(retry, "docker inspect") || iCleanup < strings.LastIndex(retry, "config.hash.tmp") {
		t.Fatalf("retry must keep retired directories until it succeeds:\n%s", retry)
	}
}

func TestEnsureProxyMissingMarkerTakesConfigPath(t *testing.T) {
	for _, marker := range []string{"", "sha256:" + strings.Repeat("a", 64), "truncated marker"} {
		t.Run(marker, func(t *testing.T) {
			f := &transport.Fake{}
			e, _, _ := proxyFixture(t, f)
			ps := proxyPS(f, true)
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				if cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'" {
					return transport.Result{Stdout: marker}, true
				}
				return ps(cmd)
			}
			if err := e.EnsureProxy(context.Background(), "unknown-marker", false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(f.Commands, "\n"), "/config' '/var/lib/onebox/_host/proxy/config'") {
				t.Fatal("unknown marker did not apply config")
			}
		})
	}
}
