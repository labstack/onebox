package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/transport"
)

func opsFake(remoteSecretsHash string) *transport.Fake {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/R7\n"}, true
		}
		if strings.Contains(cmd, "sha256sum") {
			return transport.Result{Stdout: remoteSecretsHash + "\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestSecretsPushBouncesOnChange(t *testing.T) {
	f := opsFake("deadbeef") // remote hash differs from anything
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.SecretsPush(context.Background(), []byte("KEY=new\n")); err != nil {
		t.Fatalf("push: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, `"phase":"secrets-push"`) {
		t.Fatalf("not journaled:\n%s", seq)
	}
	if !strings.Contains(seq, "--force-recreate worker") || !strings.Contains(seq, "--scale server=2") {
		t.Fatalf("roles not bounced:\n%s", seq)
	}
	if len(f.Uploads) != 1 || !strings.Contains(f.Uploads[0], "releases/R7") {
		t.Fatalf("secrets not shipped to current release: %v", f.Uploads)
	}
	if strings.Contains(seq, "KEY=new") {
		t.Fatal("secret content must never appear in a command")
	}
}

func TestSecretsPushNoopOnMatch(t *testing.T) {
	// sha256("KEY=same\n")
	const h = "1c9f79ee3d19a731d0a1a301a1a175467bcb99a8ea4b09b8b25e00b46a5a1a75"
	f := opsFake(h)
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	// compute actual hash instead of hardcoding
	_ = h
	env := []byte("KEY=same\n")
	// prime fake with the real hash of env
	f2 := opsFake(HashBytesHex(env))
	e = New(testConfig(), testProject(t), f2, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.SecretsPush(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(f2.Uploads) != 0 {
		t.Fatalf("no-op push must not upload: %v", f2.Uploads)
	}
	if strings.Contains(strings.Join(f2.Commands, "\n"), "force-recreate") {
		t.Fatal("no-op push must not bounce roles")
	}
}

func TestDestroySequence(t *testing.T) {
	f := opsFake("x")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, false); err != nil {
		t.Fatalf("destroy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "down --remove-orphans") {
		t.Fatalf("compose down missing:\n%s", seq)
	}
	if strings.Contains(seq, "down --remove-orphans -v") {
		t.Fatal("volumes must be kept without --volumes")
	}
	if !strings.Contains(seq, "rm -rf '/var/lib/yeet/monk'") {
		t.Fatalf("state dir not removed:\n%s", seq)
	}
}

func TestLogsAndExecShapes(t *testing.T) {
	f := opsFake("x")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Logs(context.Background(), "web", true, 50, &out); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "logs --tail 50 --follow server") {
		t.Fatalf("logs shape wrong:\n%s", seq)
	}
	if err := e.ExecIn(context.Background(), "web", "alembic current", &out); err != nil {
		t.Fatal(err)
	}
	seq = strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker exec OLD1 sh -c 'alembic current'") {
		t.Fatalf("exec shape wrong:\n%s", seq)
	}
	if _, err := e.resolveService("nope"); err == nil {
		t.Fatal("unknown name must error")
	}
}

func proxyManagedCfg() *config.Config {
	cfg := testConfig()
	cfg.Proxy = config.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	return cfg
}

func TestDestroyDeregistersFromProxy(t *testing.T) {
	f := opsFake("x")
	e := New(proxyManagedCfg(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, false); err != nil {
		t.Fatalf("destroy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "rm -f '/var/lib/yeet/_host/proxy/apps/monk'") {
		t.Fatalf("destroy must deregister the app from the shared proxy:\n%s", seq)
	}
	if strings.Contains(seq, "docker compose -p yeet-proxy") && strings.Contains(seq, "down") && strings.Contains(seq, "yeet-proxy") == strings.Contains(seq, "-p yeet-proxy' down") {
		// guard below asserts precisely
	}
	if strings.Contains(seq, "-p yeet-proxy -f '/var/lib/yeet/_host/proxy/compose.yaml' down") {
		t.Fatalf("without --proxy the shared proxy must survive:\n%s", seq)
	}
}

func TestDestroyProxyTeardownWhenLastApp(t *testing.T) {
	f := opsFake("x")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "ls -1 '/var/lib/yeet/_host/proxy/apps'") {
			return transport.Result{Stdout: "monk\n"}, true // we are the last app
		}
		return base(cmd)
	}
	e := New(proxyManagedCfg(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, true); err != nil {
		t.Fatalf("destroy --proxy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker compose -p yeet-proxy -f '/var/lib/yeet/_host/proxy/compose.yaml' down") {
		t.Fatalf("last app with --proxy must tear the proxy down:\n%s", seq)
	}
	if !strings.Contains(seq, "rm -rf '/var/lib/yeet/_host/proxy'") {
		t.Fatalf("proxy state dir must go with it:\n%s", seq)
	}
}

func TestDestroyProxyRefusedWhenShared(t *testing.T) {
	f := opsFake("x")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "ls -1 '/var/lib/yeet/_host/proxy/apps'") {
			return transport.Result{Stdout: "monk\nunlock\n"}, true
		}
		return base(cmd)
	}
	e := New(proxyManagedCfg(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Destroy(context.Background(), false, true)
	if err == nil || !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("--proxy with other registered apps must refuse naming them, got %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "down --remove-orphans") {
		t.Fatalf("refusal must happen BEFORE any app teardown:\n%s", seq)
	}
}

func TestDestroyProxyFlagRequiresManaged(t *testing.T) {
	f := opsFake("x")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, true); err == nil {
		t.Fatal("--proxy without proxy.managed must error")
	}
}

func TestDestroyVolumesOnSweepPath(t *testing.T) {
	// no release ever activated (bootstrap-only host): teardown sweeps by
	// label — --volumes must still remove the project's named volumes
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: ""}, true // never activated
		}
		if strings.Contains(cmd, "docker ps -aq") {
			return transport.Result{Stdout: "C1\n"}, true
		}
		if strings.Contains(cmd, "docker volume ls") {
			return transport.Result{Stdout: "monk_pgdata\nmonk_cache\n"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), true, false); err != nil {
		t.Fatalf("destroy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker volume rm monk_pgdata monk_cache") {
		t.Fatalf("--volumes on the sweep path must remove labeled volumes:\n%s", seq)
	}

	// and WITHOUT --volumes the sweep must not touch them
	f2 := happyFake()
	base2 := f2.Dynamic
	f2.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: ""}, true
		}
		if strings.Contains(cmd, "docker ps -aq") {
			return transport.Result{Stdout: "C1\n"}, true
		}
		return base2(cmd)
	}
	e2 := New(testConfig(), testProject(t), f2, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e2.Destroy(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f2.Commands, "\n"), "docker volume") {
		t.Fatalf("volumes must be kept without --volumes:\n%s", strings.Join(f2.Commands, "\n"))
	}
}
