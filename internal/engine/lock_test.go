package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/transport"
)

func lockEngine(t *testing.T, f *transport.Fake) *Engine {
	t.Helper()
	return New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
}

func TestAcquireLockHappyPath(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "cat '/var/lib/yeet/monk/epoch'") {
			return transport.Result{Stdout: "6\n"}, true
		}
		return transport.Result{}, false
	}}
	e := lockEngine(t, f)
	epoch, err := e.AcquireLock(context.Background(), "R9", false)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	if epoch != 7 {
		t.Fatalf("epoch: %d", epoch)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "set -C") || !strings.Contains(seq, "/var/lib/yeet/monk/lock") {
		t.Fatalf("noclobber lock creation missing:\n%s", seq)
	}
	if !strings.Contains(seq, "echo 7 > '/var/lib/yeet/monk/epoch'") {
		t.Fatalf("epoch not persisted:\n%s", seq)
	}
}

func TestAcquireLockHeldFreshRefuses(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "set -C") {
			return transport.Result{ExitCode: 1, Stderr: "cannot overwrite"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/monk/lock'") {
			return transport.Result{Stdout: `{"owner":"alice@laptop","deploy_id":"R8","epoch":6}`}, true
		}
		if strings.Contains(cmd, "date +%s") { // age computation
			return transport.Result{Stdout: "42\n"}, true
		}
		return transport.Result{}, false
	}}
	e := lockEngine(t, f)
	_, err := e.AcquireLock(context.Background(), "R9", false)
	if err == nil || !strings.Contains(err.Error(), "alice@laptop") {
		t.Fatalf("want held error naming holder, got %v", err)
	}
}

func TestAcquireLockStaleTTLTakesOver(t *testing.T) {
	creates := 0
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "set -C") {
			creates++
			if creates == 1 {
				return transport.Result{ExitCode: 1}, true
			}
			return transport.Result{}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/monk/lock'") {
			return transport.Result{Stdout: `{"owner":"dead@runner","deploy_id":"R7","epoch":5}`}, true
		}
		if strings.Contains(cmd, "date +%s") {
			return transport.Result{Stdout: "999999\n"}, true // way past TTL
		}
		return transport.Result{}, false
	}
	e := lockEngine(t, f)
	if _, err := e.AcquireLock(context.Background(), "R9", false); err != nil {
		t.Fatalf("stale lock should be taken over: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "rm -f '/var/lib/yeet/monk/lock'") {
		t.Fatal("stale lock not removed")
	}
}

func TestAcquireLockSameDeployReclaims(t *testing.T) {
	creates := 0
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "set -C") {
			creates++
			if creates == 1 {
				return transport.Result{ExitCode: 1}, true
			}
			return transport.Result{}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/monk/lock'") {
			return transport.Result{Stdout: `{"owner":"dead@runner","deploy_id":"R9","epoch":6}`}, true
		}
		if strings.Contains(cmd, "date +%s") {
			return transport.Result{Stdout: "10\n"}, true // fresh — but same deploy
		}
		return transport.Result{}, false
	}
	e := lockEngine(t, f)
	if _, err := e.AcquireLock(context.Background(), "R9", false); err != nil {
		t.Fatalf("same-deploy lock must be reclaimable (resume after crash): %v", err)
	}
}

func TestForceBreakPrintsHolderJournalTail(t *testing.T) {
	creates := 0
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "set -C") {
			creates++
			if creates == 1 {
				return transport.Result{ExitCode: 1}, true
			}
			return transport.Result{}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/yeet/monk/lock'") {
			return transport.Result{Stdout: `{"owner":"bob@ci","deploy_id":"R8","epoch":6}`}, true
		}
		if strings.Contains(cmd, "date +%s") {
			return transport.Result{Stdout: "10\n"}, true // fresh — needs force
		}
		if strings.Contains(cmd, "tail") && strings.Contains(cmd, "R8.jsonl") {
			return transport.Result{Stdout: `{"phase":"release","role":"web"}`}, true
		}
		return transport.Result{}, false
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if _, err := e.AcquireLock(context.Background(), "R9", true); err != nil {
		t.Fatalf("force must break: %v", err)
	}
	if !strings.Contains(out.String(), "bob@ci") || !strings.Contains(out.String(), "release") {
		t.Fatalf("force break must print holder + journal tail: %s", out.String())
	}
}

func TestMutateWrapsWithFenceAndTranslates97(t *testing.T) {
	f := &transport.Fake{}
	e := lockEngine(t, f)
	if err := e.WriteFence(context.Background(), "R9", 7); err != nil {
		t.Fatal(err)
	}
	res, err := e.mutate(context.Background(), "docker stop OLD1")
	if err != nil || res.ExitCode != 0 {
		t.Fatal(err)
	}
	last := f.Commands[len(f.Commands)-1]
	if !strings.Contains(last, `[ "$(cat '/var/lib/yeet/monk/fence' 2>/dev/null)" = 'R9 7' ]`) {
		t.Fatalf("fence guard missing: %s", last)
	}
	if !strings.Contains(last, "docker stop OLD1") {
		t.Fatalf("command missing: %s", last)
	}

	f.Dynamic = func(cmd string) (transport.Result, bool) {
		return transport.Result{ExitCode: 97, Stderr: "ob-fenced"}, true
	}
	_, err = e.mutate(context.Background(), "docker stop OLD1")
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("exit 97 must translate to ErrFenced, got %v", err)
	}
}

func TestHeartbeatTouchesLock(t *testing.T) {
	f := &transport.Fake{}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LockTTL: 200 * time.Millisecond})
	stop := e.StartHeartbeat(context.Background())
	time.Sleep(90 * time.Millisecond) // > 2 intervals at TTL/10
	stop()
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "touch '/var/lib/yeet/monk/lock'") {
		t.Fatalf("heartbeat never touched lock:\n%s", seq)
	}
}
