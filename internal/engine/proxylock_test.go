package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestHostLockHappyPath(t *testing.T) {
	f := &transport.Fake{}
	e := lockEngine(t, f)
	if err := e.acquireHostLock(context.Background(), false); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "set -C") || !strings.Contains(seq, "/var/lib/ob/_host/lock") {
		t.Fatalf("noclobber host lock creation missing:\n%s", seq)
	}
	e.releaseHostLock(context.Background())
	if !strings.Contains(strings.Join(f.Commands, "\n"), "rm -f '/var/lib/ob/_host/lock'") {
		t.Fatalf("release must remove the host lock:\n%s", strings.Join(f.Commands, "\n"))
	}
}

func TestHostLockHeldFreshRefuses(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "set -C") {
			return transport.Result{ExitCode: 1, Stderr: "cannot overwrite"}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/ob/_host/lock'") {
			return transport.Result{Stdout: `{"owner":"alice@laptop","deploy_id":"unlock","epoch":0}`}, true
		}
		if strings.Contains(cmd, "date +%s") {
			return transport.Result{Stdout: "30\n"}, true // fresh
		}
		return transport.Result{}, false
	}}
	e := lockEngine(t, f)
	err := e.acquireHostLock(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "alice@laptop") {
		t.Fatalf("fresh host lock must refuse naming the holder, got %v", err)
	}
}

func TestHostLockExpiredTakesOver(t *testing.T) {
	broke := false
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "set -C") && !broke {
			return transport.Result{ExitCode: 1, Stderr: "cannot overwrite"}, true
		}
		if strings.Contains(cmd, "rm -f '/var/lib/ob/_host/lock'") {
			broke = true
			return transport.Result{}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/ob/_host/lock'") {
			return transport.Result{Stdout: `{"owner":"bob@ci","deploy_id":"other","ttl_s":600}`}, true
		}
		if strings.Contains(cmd, "date +%s") {
			return transport.Result{Stdout: "99999\n"}, true // long expired
		}
		return transport.Result{}, false
	}
	e := lockEngine(t, f)
	if err := e.acquireHostLock(context.Background(), false); err != nil {
		t.Fatalf("expired host lock must be taken over: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
}

func TestHostLockForceBreaks(t *testing.T) {
	broke := false
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "set -C") && !broke {
			return transport.Result{ExitCode: 1, Stderr: "cannot overwrite"}, true
		}
		if strings.Contains(cmd, "rm -f '/var/lib/ob/_host/lock'") {
			broke = true
			return transport.Result{}, true
		}
		if strings.Contains(cmd, "cat '/var/lib/ob/_host/lock'") {
			return transport.Result{Stdout: `{"owner":"bob@ci","deploy_id":"other","ttl_s":600}`}, true
		}
		if strings.Contains(cmd, "date +%s") {
			return transport.Result{Stdout: "30\n"}, true // fresh — only force may break
		}
		return transport.Result{}, false
	}
	e := lockEngine(t, f)
	if err := e.acquireHostLock(context.Background(), true); err != nil {
		t.Fatalf("force must break the host lock: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
}
