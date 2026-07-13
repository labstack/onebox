package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
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
		if strings.Contains(cmd, "cat '/var/lib/ob/monk/epoch'") {
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
	if !strings.Contains(seq, "set -C") || !strings.Contains(seq, "/var/lib/ob/monk/lock") {
		t.Fatalf("noclobber lock creation missing:\n%s", seq)
	}
	if !strings.Contains(seq, "echo 7 > '/var/lib/ob/monk/epoch'") {
		t.Fatalf("epoch not persisted:\n%s", seq)
	}
}

func TestReleaseLockRemovesOnlyOwnedToken(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "cat '/var/lib/ob/monk/epoch'") {
			return transport.Result{Stdout: "2\n"}, true
		}
		return transport.Result{}, false
	}}
	e := lockEngine(t, f)
	if _, err := e.AcquireLock(context.Background(), "R-owned", false); err != nil {
		t.Fatal(err)
	}
	f.Commands = nil
	e.ReleaseLock(context.Background())
	if len(f.Commands) != 1 {
		t.Fatalf("release commands = %v", f.Commands)
	}
	command := f.Commands[0]
	if !strings.Contains(command, "$(cat '/var/lib/ob/monk/lock'") ||
		!strings.Contains(command, `"deploy_id":"R-owned"`) ||
		!strings.Contains(command, "then rm -f '/var/lib/ob/monk/lock'") {
		t.Fatalf("release is not ownership-conditional: %s", command)
	}
}

func TestAcquireLockHeldFreshRefuses(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "set -C") {
			return transport.Result{ExitCode: 1, Stderr: "cannot overwrite"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/ob/monk/lock'") {
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
		if strings.Contains(cmd, "cat '/var/lib/ob/monk/lock'") {
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
	if !strings.Contains(strings.Join(f.Commands, "\n"), "rm -f '/var/lib/ob/monk/lock'") {
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
		if strings.Contains(cmd, "cat '/var/lib/ob/monk/lock'") {
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

// After breaking a stale lock, the epoch must be read FRESH — a value read once
// before the retry loop could reuse one a concurrent winner persisted meanwhile,
// violating the strictly-increasing-epoch invariant all of fencing rests on.
func TestAcquireLockReReadsEpochAfterBreakingStaleLock(t *testing.T) {
	epochReads, creates := 0, 0
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "cat '/var/lib/ob/monk/epoch'"):
			epochReads++
			if epochReads == 1 {
				return transport.Result{Stdout: "5\n"}, true // stale holder's value
			}
			return transport.Result{Stdout: "6\n"}, true // advanced by a concurrent winner before our retry
		case strings.Contains(cmd, "set -C"):
			creates++
			if creates == 1 {
				return transport.Result{ExitCode: 1}, true // held → forces a break + retry
			}
			return transport.Result{}, true // win on retry
		case strings.Contains(cmd, "cat '/var/lib/ob/monk/lock'"):
			return transport.Result{Stdout: `{"owner":"dead@runner","deploy_id":"R7","epoch":5}`}, true
		case strings.Contains(cmd, "date +%s"):
			return transport.Result{Stdout: "999999\n"}, true // past TTL → take over
		}
		return transport.Result{}, false
	}
	e := lockEngine(t, f)
	epoch, err := e.AcquireLock(context.Background(), "R9", false)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	if epochReads < 2 {
		t.Fatalf("epoch must be re-read after breaking the lock; reads=%d", epochReads)
	}
	// 6 (fresh) + 1, NOT 5 (stale pre-loop value) + 1 = 6.
	if epoch != 7 {
		t.Fatalf("epoch must derive from the FRESH read (want 7), got %d", epoch)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "echo 7 > '/var/lib/ob/monk/epoch'") {
		t.Fatalf("fresh epoch 7 not persisted:\n%s", strings.Join(f.Commands, "\n"))
	}
}

// The heartbeat must never resurrect a lock a newer deploy deleted on takeover,
// and must stop refreshing once re-fenced. Driven over the real-shell Local
// transport so `touch -c` and the fence guard are actually evaluated.
func TestRefreshLockNeverResurrectsDeletedLock(t *testing.T) {
	base := t.TempDir()
	t.Setenv("OB_BASE_DIR", base)
	e := New(testConfig(), testProject(t), transport.NewLocal(), Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	ctx := context.Background()
	if err := os.MkdirAll(e.base(), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := e.lockPath()
	if err := os.WriteFile(lock, []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.lockVal = "held"
	if err := e.WriteFence(ctx, "D1", 1); err != nil {
		t.Fatal(err)
	}

	// fence matches + lock exists → refresh succeeds.
	if res, err := e.refreshLock(ctx); err != nil || res.ExitCode != 0 {
		t.Fatalf("refresh with matching fence must succeed: exit %d err %v", res.ExitCode, err)
	}

	// a newer deploy deleted the lock on takeover → `touch -c` must NOT recreate it.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if _, err := e.refreshLock(ctx); err != nil {
		t.Fatalf("refresh transport error: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("touch -c resurrected a deleted lock — split-brain risk")
	}

	// a newer deploy re-fenced us → refresh is a no-op (exit 3), even if the lock exists.
	if err := os.WriteFile(lock, []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteFence(ctx, "D2", 2); err != nil { // re-stamps the host fence
		t.Fatal(err)
	}
	e.fenceVal = "D1 1" // this (now-stale) runner still believes it owns D1
	res, err := e.refreshLock(ctx)
	if err != nil {
		t.Fatalf("refresh transport error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("re-fenced refresh must be a no-op (exit 3), got exit %d", res.ExitCode)
	}
}

func TestLockAgeCmdIsPortable(t *testing.T) {
	got := lockAgeCmd("'/var/lib/ob/monk/lock'")
	for _, want := range []string{
		"[ -L '/var/lib/ob/monk/lock' ] && [ ! -e '/var/lib/ob/monk/lock' ]", // dangling symlink → refuse, portably
		"stat -c %Y '/var/lib/ob/monk/lock'",                                 // GNU
		"stat -f %m '/var/lib/ob/monk/lock'",                                 // BSD/macOS fallback
		"[ -e '/var/lib/ob/monk/lock' ] || [ -L '/var/lib/ob/monk/lock' ]",   // present → refuse (echo 0)
		"else date +%s; fi", // truly absent → take over
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("lockAgeCmd missing %q:\n%s", want, got)
		}
	}
}

// lockAgeCmd fails CLOSED, executed over the real shell so branch LOGIC (not the
// string) is verified: absent → maximally old (take over), present+readable →
// young, present-but-unstattable (a dangling symlink) → 0 so the caller refuses.
func TestLockAgeCmdBehavior(t *testing.T) {
	dir := t.TempDir()
	age := func(path string) int {
		res, err := transport.NewLocal().Run(context.Background(), lockAgeCmd(q(path)))
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
		if err != nil {
			t.Fatalf("non-numeric age %q", res.Stdout)
		}
		return n
	}

	lock := dir + "/lock"
	if a := age(lock); a < 1_000_000 { // absent → ~current epoch, well past any TTL
		t.Fatalf("absent lock must read as maximally old, got %d", a)
	}
	if err := os.WriteFile(lock, []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a := age(lock); a < 0 || a > 60 { // just created → tiny
		t.Fatalf("fresh lock must read young, got %d", a)
	}

	// A dangling symlink is present (`-L`) but unstattable (stat follows it and
	// fails) — the fail-closed branch. Age 0 → caller refuses, not take-over.
	link := dir + "/link"
	if err := os.Symlink(dir+"/nonexistent-target", link); err != nil {
		t.Fatal(err)
	}
	if a := age(link); a != 0 {
		t.Fatalf("present-but-unstattable lock must fail closed (age 0), got %d", a)
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
		if strings.Contains(cmd, "cat '/var/lib/ob/monk/lock'") {
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
	e.lockVal = "owned"
	if err := e.WriteFence(context.Background(), "R9", 7); err != nil {
		t.Fatal(err)
	}
	res, err := e.mutate(context.Background(), "docker stop OLD1")
	if err != nil || res.ExitCode != 0 {
		t.Fatal(err)
	}
	last := f.Commands[len(f.Commands)-1]
	if !strings.Contains(last, `[ "$(cat '/var/lib/ob/monk/fence' 2>/dev/null)" = 'R9 7' ]`) {
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
	e.lockVal = "owned"
	_ = e.WriteFence(context.Background(), "D1", 1)
	stop := e.StartHeartbeat(context.Background())
	time.Sleep(90 * time.Millisecond) // > 2 intervals at TTL/10
	stop()
	seq := strings.Join(f.Commands, "\n")
	// `touch -c` refreshes the mtime but never creates the file — a lock another
	// runner deleted on takeover must not be resurrected.
	if !strings.Contains(seq, "touch -c '/var/lib/ob/monk/lock'") {
		t.Fatalf("heartbeat never touched lock:\n%s", seq)
	}
	// and it only refreshes while the fence still names this runner.
	if !strings.Contains(seq, "cat '/var/lib/ob/monk/fence'") {
		t.Fatalf("heartbeat must be fence-guarded:\n%s", seq)
	}
}
