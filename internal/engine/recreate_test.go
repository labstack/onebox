package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/transport"
)

func TestRecreateRoleSequence(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") {
			return transport.Result{Stdout: "W1\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Status}}") {
			return transport.Result{Stdout: "running\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RecreateRole(context.Background(), "worker", "F"); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "up -d --no-deps --force-recreate worker") {
		t.Fatalf("missing force-recreate:\n%s", seq)
	}
	// worker drain is TERM: left to docker's own stop during recreate
	if strings.Contains(seq, "--signal=TERM") {
		t.Fatalf("TERM bleed should be left to stop/recreate:\n%s", seq)
	}
}

// A local hook must see the FULL user@host in $YEET_TARGET (not the bare
// hostname), so hooks can ssh/rsync the deploy host without hardcoding it.
func TestLocalHookGetsFullTargetInEnv(t *testing.T) {
	f := &transport.Fake{HostName: "myhost", TargetName: "root@myhost"}
	cfg := testConfig()
	cfg.Hooks["pre_release"] = config.Hook{
		Run:   `test "$YEET_TARGET" = "root@myhost" || { echo "got [$YEET_TARGET] want [root@myhost]" >&2; exit 1; }`,
		Local: true,
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: t.TempDir()})
	if err := e.RunHook(context.Background(), "pre_release", "/r", "/r/compose.yaml"); err != nil {
		t.Fatalf("YEET_TARGET must be the full user@host: %v", err)
	}
}

func TestRunHookSetsComposeEnvAndFailsHard(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "run --rm --no-deps migrate") {
			return transport.Result{ExitCode: 7, Stderr: "alembic exploded"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.RunHook(context.Background(), "migrate", "/var/lib/yeet/monk/releases/R1", "/var/lib/yeet/monk/releases/R1/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "alembic exploded") {
		t.Fatalf("hook failure must halt deploy with stderr, got %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "COMPOSE_PROJECT_NAME=monk") || !strings.Contains(seq, "COMPOSE_FILE=") {
		t.Fatalf("hook must run with compose env exported:\n%s", seq)
	}
}

func TestRunHookNoopWhenAbsent(t *testing.T) {
	f := &transport.Fake{}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RunHook(context.Background(), "nope", "/d", "/d/c.yaml"); err != nil {
		t.Fatal(err)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("absent hook must run nothing: %v", f.Commands)
	}
}
