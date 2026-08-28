package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestRecreateRoleSequence(t *testing.T) {
	waits := 0
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") {
			return transport.Result{Stdout: "W1\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Running}}") {
			return transport.Result{Stdout: "false\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Status}}") {
			return transport.Result{Stdout: "running\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{
		Out: &bytes.Buffer{}, Sleep: noSleep,
		Wait: func(context.Context, time.Duration) error { waits++; return nil },
	})
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
	if waits != 0 {
		t.Fatalf("an immediately exited container consumed %d poll waits", waits)
	}
}

func TestRecreateRoleHonorsDrainGrace(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") {
			return transport.Result{Stdout: "W1\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Running}}") {
			return transport.Result{Stdout: "false\n"}, true
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
		if strings.Contains(cmd, "{{.State.Running}}") {
			return transport.Result{Stdout: "false\n"}, true
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

type recreateClock struct {
	now    time.Time
	waits  []time.Duration
	cancel context.CancelFunc
}

func (c *recreateClock) current() time.Time { return c.now }

func (c *recreateClock) wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	return ctx.Err()
}

func (c *recreateClock) totalWait() time.Duration {
	var total time.Duration
	for _, wait := range c.waits {
		total += wait
	}
	return total
}

func TestRecreateDrainContinuesWhenContainerExits(t *testing.T) {
	inspections := 0
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps -q"):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(cmd, "{{.State.Running}}"):
			inspections++
			if inspections < 3 {
				return transport.Result{Stdout: "true\n"}, true
			}
			return transport.Result{Stdout: "false\n"}, true
		case strings.Contains(cmd, "{{.State.Status}}"):
			return transport.Result{Stdout: "running\n"}, true
		}
		return transport.Result{}, false
	}}
	clock := &recreateClock{now: time.Unix(0, 0)}
	cfg := testConfig()
	worker := cfg.Workloads["worker"]
	worker.Drain.Wait = "5s"
	cfg.Workloads["worker"] = worker
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Now: clock.current, Wait: clock.wait})
	if err := e.RecreateRole(context.Background(), "worker", "F"); err != nil {
		t.Fatal(err)
	}
	if got := clock.totalWait(); got != 500*time.Millisecond {
		t.Fatalf("waited %v after an early exit, want two polls (500ms)", got)
	}
	if seq := strings.Join(f.Commands, "\n"); !strings.Contains(seq, "up -d --no-deps --force-recreate") {
		t.Fatalf("recreate did not continue after exit:\n%s", seq)
	}
}

func TestRecreateDrainWaitsForEveryReplica(t *testing.T) {
	inspections := map[string]int{}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps -q"):
			return transport.Result{Stdout: "W1\nW2\n"}, true
		case strings.Contains(cmd, "{{.State.Running}}"):
			id := strings.Fields(cmd)[len(strings.Fields(cmd))-1]
			inspections[id]++
			if id == "W1" || inspections[id] > 1 {
				return transport.Result{Stdout: "false\n"}, true
			}
			return transport.Result{Stdout: "true\n"}, true
		case strings.Contains(cmd, "{{.State.Status}}"):
			return transport.Result{Stdout: "running\n"}, true
		}
		return transport.Result{}, false
	}}
	clock := &recreateClock{now: time.Unix(0, 0)}
	cfg := testConfig()
	worker := cfg.Workloads["worker"]
	worker.Replicas = 2
	cfg.Workloads["worker"] = worker
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Now: clock.current, Wait: clock.wait})
	if err := e.RecreateRole(context.Background(), "worker", "F"); err != nil {
		t.Fatal(err)
	}
	if inspections["W1"] != 1 || inspections["W2"] != 2 {
		t.Fatalf("inspections = %#v, want exited W1 dropped while W2 remains", inspections)
	}
}

func TestRecreateDrainTimeoutStillRecreates(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps -q"):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(cmd, "{{.State.Running}}"):
			return transport.Result{Stdout: "true\n"}, true
		case strings.Contains(cmd, "{{.State.Status}}"):
			return transport.Result{Stdout: "running\n"}, true
		}
		return transport.Result{}, false
	}}
	fixedNow := time.Unix(0, 0)
	var waits []time.Duration
	cfg := testConfig()
	worker := cfg.Workloads["worker"]
	worker.Drain = &app.Drain{Signal: "USR1", Wait: "600ms", Grace: "7s"}
	cfg.Workloads["worker"] = worker
	e := New(cfg, testProject(t), f, Options{
		Out: &bytes.Buffer{},
		Now: func() time.Time { return fixedNow },
		Wait: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			if len(waits) > 3 {
				return errors.New("drain wait did not consume its elapsed budget")
			}
			return nil
		},
	})
	if err := e.RecreateRole(context.Background(), "worker", "F"); err != nil {
		t.Fatal(err)
	}
	var total time.Duration
	for _, wait := range waits {
		total += wait
	}
	if got := total; got != 600*time.Millisecond {
		t.Fatalf("drain wait = %v, want exact 600ms bound", got)
	}
	if len(waits) != 3 {
		t.Fatalf("drain polls = %d, want 3 with fixed Options.Now", len(waits))
	}
	if seq := strings.Join(f.Commands, "\n"); !strings.Contains(seq, "--force-recreate --timeout 7 worker") {
		t.Fatalf("timeout did not continue through existing grace behavior:\n%s", seq)
	}
}

func TestRecreateDrainTreatsVanishedContainerAsExited(t *testing.T) {
	waits := 0
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps -q"):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(cmd, "{{.State.Running}}"):
			return transport.Result{ExitCode: 1, Stderr: "Error: No such object: W1"}, true
		case strings.Contains(cmd, "{{.State.Status}}"):
			return transport.Result{Stdout: "running\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{
		Out: &bytes.Buffer{}, Wait: func(context.Context, time.Duration) error { waits++; return nil },
	})
	if err := e.RecreateRole(context.Background(), "worker", "F"); err != nil {
		t.Fatal(err)
	}
	if waits != 0 {
		t.Fatalf("vanished container consumed %d poll waits", waits)
	}
}

func TestRecreateDrainFailsClosedOnInspectionFailure(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") {
			return transport.Result{Stdout: "W1\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Running}}") {
			return transport.Result{ExitCode: 1, Stderr: "permission denied"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}})
	err := e.RecreateRole(context.Background(), "worker", "F")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("inspection failure = %v, want explicit refusal", err)
	}
	if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "--force-recreate") {
		t.Fatalf("inspection failure must halt before replacement:\n%s", seq)
	}
}

func TestRecreateDrainCancellationInterruptsWait(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") {
			return transport.Result{Stdout: "W1\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Running}}") {
			return transport.Result{Stdout: "true\n"}, true
		}
		return transport.Result{}, false
	}}
	ctx, cancel := context.WithCancel(context.Background())
	clock := &recreateClock{now: time.Unix(0, 0), cancel: cancel}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Now: clock.current, Wait: clock.wait})
	err := e.RecreateRole(ctx, "worker", "F")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v, want context.Canceled", err)
	}
	if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "--force-recreate") {
		t.Fatalf("cancelled drain must halt before replacement:\n%s", seq)
	}
}

// A local hook must see the FULL user@host in $ONEBOX_SERVER (not the bare
// hostname), so hooks can ssh/rsync the deploy host without hardcoding it.
func TestLocalHookGetsFullTargetInEnv(t *testing.T) {
	f := &transport.Fake{
		HostName: "2001:db8::1", TargetName: "root@2001:db8::1",
		SSHUserName: "root", SSHPortName: "2222",
	}
	cfg := testConfig()
	cfg.Hooks["pre_release"] = app.Command{
		Run:   `test "$ONEBOX_SERVER" = "root@2001:db8::1" && test "$ONEBOX_SSH_USER" = "root" && test "$ONEBOX_HOST" = "2001:db8::1" && test "$ONEBOX_SSH_PORT" = "2222" || { echo "got server=[$ONEBOX_SERVER] user=[$ONEBOX_SSH_USER] host=[$ONEBOX_HOST] port=[$ONEBOX_SSH_PORT]" >&2; exit 1; }`,
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
// direct connection, so `ssh ${ONEBOX_SSH_JUMP:+-J $ONEBOX_SSH_JUMP}` works either way.
func TestLocalHookGetsTheJumpHostInEnv(t *testing.T) {
	f := &transport.Fake{
		HostName: "10.20.0.10", TargetName: "root@10.20.0.10",
		SSHUserName: "root", SSHPortName: "22", SSHJumpName: "deploy@bastion.example.com:2222",
	}
	cfg := testConfig()
	cfg.Hooks["pre_release"] = app.Command{
		Run:   `test "$ONEBOX_SSH_JUMP" = "deploy@bastion.example.com:2222" || { echo "got jump=[$ONEBOX_SSH_JUMP]" >&2; exit 1; }`,
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
		Run:   `test -z "$ONEBOX_SSH_JUMP" || { echo "got jump=[$ONEBOX_SSH_JUMP]" >&2; exit 1; }`,
		Local: true,
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: t.TempDir()})
	if err := e.RunHook(context.Background(), "pre_release", "/r", "/r/compose.yaml"); err != nil {
		t.Fatalf("a direct connection must leave ONEBOX_SSH_JUMP empty: %v", err)
	}
}
