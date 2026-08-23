package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
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
	if !strings.Contains(seq, "up -d --no-deps --force-recreate --timeout 30 worker") {
		t.Fatalf("missing force-recreate:\n%s", seq)
	}
	if !strings.Contains(seq, "docker kill --signal=TERM W1") {
		t.Fatalf("TERM drain signal must precede recreate:\n%s", seq)
	}
}

func TestRecreateRoleHonorsDrainGrace(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") {
			return transport.Result{Stdout: "W1\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Status}}") {
			return transport.Result{Stdout: "running\n"}, true
		}
		return transport.Result{}, false
	}}
	cfg := testConfig()
	worker := cfg.Workloads["worker"]
	worker.Drain = &app.Drain{Signal: "TERM", Wait: "1s", Grace: "45s"}
	cfg.Workloads["worker"] = worker
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RecreateRole(context.Background(), "worker", "F"); err != nil {
		t.Fatal(err)
	}
	if seq := strings.Join(f.Commands, "\n"); !strings.Contains(seq, "up -d --no-deps --force-recreate --timeout 45 worker") {
		t.Fatalf("recreate did not honor drain grace:\n%s", seq)
	}
}

func TestRecreateRoleSurfacesFailedDrainSignal(t *testing.T) {
	// A misspelled drain.signal makes `docker kill` exit non-zero. That must be
	// surfaced (not silently swallowed) so the operator learns their declared
	// drain never fires — while the recreate still proceeds.
	out := &bytes.Buffer{}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") {
			return transport.Result{Stdout: "W1\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Status}}") {
			return transport.Result{Stdout: "running\n"}, true
		}
		if strings.Contains(cmd, "docker kill --signal=") {
			return transport.Result{ExitCode: 125, Stderr: "Error response from daemon: invalid signal: TERM_TYPO"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: out, Sleep: noSleep})
	if err := e.RecreateRole(context.Background(), "worker", "F"); err != nil {
		t.Fatalf("a per-container drain-signal rejection must not abort recreate: %v", err)
	}
	if !strings.Contains(out.String(), "drain signal") || !strings.Contains(out.String(), "invalid signal") {
		t.Fatalf("failed drain signal was not surfaced to the operator: %q", out.String())
	}
	if seq := strings.Join(f.Commands, "\n"); !strings.Contains(seq, "up -d --no-deps --force-recreate") {
		t.Fatalf("recreate must still proceed after a surfaced drain failure:\n%s", seq)
	}
}

// A local hook must see the FULL user@host in $OB_SERVER (not the bare
// hostname), so hooks can ssh/rsync the deploy host without hardcoding it.
func TestLocalHookGetsFullTargetInEnv(t *testing.T) {
	f := &transport.Fake{
		HostName: "2001:db8::1", TargetName: "root@2001:db8::1",
		SSHUserName: "root", SSHPortName: "2222",
	}
	cfg := testConfig()
	cfg.Hooks["pre_release"] = app.Command{
		Run:   `test "$OB_SERVER" = "root@2001:db8::1" && test "$OB_SSH_USER" = "root" && test "$OB_HOST" = "2001:db8::1" && test "$OB_SSH_PORT" = "2222" || { echo "got server=[$OB_SERVER] user=[$OB_SSH_USER] host=[$OB_HOST] port=[$OB_SSH_PORT]" >&2; exit 1; }`,
		Local: true,
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: t.TempDir()})
	if err := e.RunHook(context.Background(), "pre_release", "/r", "/r/compose.yaml"); err != nil {
		t.Fatalf("hook must receive an OpenSSH/rsync target and separate port: %v", err)
	}
}

func TestRunHookSetsComposeEnvAndFailsHard(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "run --rm --no-deps migrate") {
			return transport.Result{ExitCode: 7, Stderr: "alembic exploded"}, true
		}
		return transport.Result{}, false
	}}
	cfg := testConfig()
	cfg.Hooks["migrate"] = app.Command{Run: "docker compose run --rm --no-deps migrate"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.RunHook(context.Background(), "migrate", "/var/lib/ob/sample/releases/R1", "/var/lib/ob/sample/releases/R1/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "alembic exploded") {
		t.Fatalf("hook failure must halt deploy with stderr, got %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "COMPOSE_PROJECT_NAME=sample") || !strings.Contains(seq, "COMPOSE_FILE=") {
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

// A local hook runs on the operator's machine, which has no tunnel of its own,
// so a hook that reaches the host itself needs the bastion named. Empty on a
// direct connection, so `ssh ${OB_SSH_JUMP:+-J $OB_SSH_JUMP}` works either way.
func TestLocalHookGetsTheJumpHostInEnv(t *testing.T) {
	f := &transport.Fake{
		HostName: "10.20.0.10", TargetName: "root@10.20.0.10",
		SSHUserName: "root", SSHPortName: "22", SSHJumpName: "deploy@bastion.example.com:2222",
	}
	cfg := testConfig()
	cfg.Hooks["pre_release"] = app.Command{
		Run:   `test "$OB_SSH_JUMP" = "deploy@bastion.example.com:2222" || { echo "got jump=[$OB_SSH_JUMP]" >&2; exit 1; }`,
		Local: true,
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: t.TempDir()})
	if err := e.RunHook(context.Background(), "pre_release", "/r", "/r/compose.yaml"); err != nil {
		t.Fatalf("hook must receive the jump host: %v", err)
	}
}

func TestLocalHookGetsAnEmptyJumpOnADirectConnection(t *testing.T) {
	f := &transport.Fake{HostName: "10.20.0.10", TargetName: "root@10.20.0.10", SSHUserName: "root", SSHPortName: "22"}
	cfg := testConfig()
	cfg.Hooks["pre_release"] = app.Command{
		Run:   `test -z "$OB_SSH_JUMP" || { echo "got jump=[$OB_SSH_JUMP]" >&2; exit 1; }`,
		Local: true,
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: t.TempDir()})
	if err := e.RunHook(context.Background(), "pre_release", "/r", "/r/compose.yaml"); err != nil {
		t.Fatalf("a direct connection must leave OB_SSH_JUMP empty: %v", err)
	}
}
