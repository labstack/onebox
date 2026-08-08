package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func protectionLockTestEngine(fake *transport.Fake) *Engine {
	engine := New(
		&app.Resolved{Spec: &app.Spec{Name: "example", BasePath: "/var/lib/ob"}, Env: "production"},
		nil,
		fake,
		Options{Out: io.Discard, LockTTL: 10 * time.Second, Sleep: func(time.Duration) {}, Now: func() time.Time {
			return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
		}},
	)
	engine.lockVal = "app-lock"
	return engine
}

func TestProtectionLockRequiresApplicationLock(t *testing.T) {
	fake := &transport.Fake{}
	engine := protectionLockTestEngine(fake)
	engine.lockVal = ""
	if _, err := engine.AcquireProtectionLock(context.Background(), "database", "backup-1", 0); err == nil {
		t.Fatal("protection lock acquired without application lock")
	}
	if len(fake.Commands) != 0 {
		t.Fatal("protection lock touched the host before checking lock order")
	}
}

func TestProtectionLockReturnsBoundedRetryableBackupConflict(t *testing.T) {
	holder := `{"owner":"operator","operation_id":"restore-7","service":"database","epoch":4,"ttl_s":10,"acquired_at":"2026-08-07T12:00:00Z"}`
	createAttempts := 0
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		switch {
		case strings.HasPrefix(command, "set -C; echo ") && strings.Contains(command, "database.lock"):
			createAttempts++
			return transport.Result{ExitCode: 1}, true
		case strings.HasPrefix(command, "cat ") && strings.Contains(command, "database.lock"):
			return transport.Result{Stdout: holder + "\n"}, true
		case strings.HasPrefix(command, "if [ -L ") && strings.Contains(command, "database.lock"):
			return transport.Result{Stdout: "2\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := protectionLockTestEngine(fake)

	_, err := engine.AcquireProtectionLock(context.Background(), "database", "backup-8", 200*time.Millisecond)
	if !errors.Is(err, ErrProtectionConflict) {
		t.Fatalf("acquire error = %v, want protection conflict", err)
	}
	var conflict *ProtectionConflictError
	if !errors.As(err, &conflict) || conflict.Code() != "backup_conflict" || !conflict.Retryable() {
		t.Fatalf("conflict classification = %#v", err)
	}
	if got, want := createAttempts, 3; got != want {
		t.Fatalf("create attempts = %d, want bounded %d", got, want)
	}
}

func TestProtectionLockHonorsCancellation(t *testing.T) {
	fake := &transport.Fake{}
	engine := protectionLockTestEngine(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.AcquireProtectionLock(ctx, "database", "backup-1", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire error = %v, want context cancellation", err)
	}
	if len(fake.Commands) != 0 {
		t.Fatal("cancelled acquisition touched the host")
	}
}

func TestProtectionLockReclaimsStaleHolderWithNewFence(t *testing.T) {
	holder := `{"owner":"operator","operation_id":"backup-old","service":"database","epoch":4,"ttl_s":10,"acquired_at":"2026-08-07T11:00:00Z"}`
	createAttempts := 0
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		switch {
		case strings.HasPrefix(command, "set -C; echo ") && strings.Contains(command, "database.lock"):
			createAttempts++
			if createAttempts == 1 {
				return transport.Result{ExitCode: 1}, true
			}
			return transport.Result{}, true
		case strings.HasPrefix(command, "cat ") && strings.Contains(command, "database.epoch"):
			return transport.Result{Stdout: "4\n"}, true
		case strings.HasPrefix(command, "cat ") && strings.Contains(command, "database.lock"):
			return transport.Result{Stdout: holder + "\n"}, true
		case strings.HasPrefix(command, "if [ -L ") && strings.Contains(command, "database.lock"):
			return transport.Result{Stdout: "11\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := protectionLockTestEngine(fake)

	epoch, err := engine.AcquireProtectionLock(context.Background(), "database", "backup-new", 0)
	if err != nil {
		t.Fatalf("reclaim stale protection lock: %v", err)
	}
	if epoch != 5 || createAttempts != 2 {
		t.Fatalf("reclaimed epoch/attempts = %d/%d, want 5/2", epoch, createAttempts)
	}
	if got := engine.protectionFenceVals["database"]; got != "backup-new 5" {
		t.Fatalf("protection fence = %q", got)
	}
}

func TestProtectionMutationRejectsStaleFence(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "write-database-data") {
			return transport.Result{ExitCode: 98, Stderr: "ob-protection-fenced\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := protectionLockTestEngine(fake)
	engine.fenceVal = "deploy-1 1"
	engine.protectionLockVals = map[string]string{"database": "old-lock"}
	engine.protectionFenceVals = map[string]string{"database": "backup-old 4"}

	if _, err := engine.ProtectionMutate(context.Background(), "database", "write-database-data"); !errors.Is(err, ErrProtectionFenced) {
		t.Fatalf("protection mutation error = %v, want stale fence", err)
	}
}
