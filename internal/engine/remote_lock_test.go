package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fmt"
	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
	"sync"
)

// remoteLockCase drives one of the three remote locks against a fake host.
type remoteLockCase struct {
	name    string
	path    string
	acquire func(t *testing.T, f *transport.Fake, force bool) error
}

func remoteLockCases() []remoteLockCase {
	return []remoteLockCase{
		{name: "application", path: "/var/lib/onebox/app/lock", acquire: func(t *testing.T, f *transport.Fake, force bool) error {
			_, err := lockEngine(t, f).AcquireLock(context.Background(), "R9", force)
			return err
		}},
		{name: "host", path: "/var/lib/onebox/_host/lock", acquire: func(t *testing.T, f *transport.Fake, force bool) error {
			return lockEngine(t, f).acquireHostLock(context.Background(), force)
		}},
		{name: "backup", path: "/var/lib/onebox/app/backup/locks/database.lock", acquire: func(_ *testing.T, f *transport.Fake, _ bool) error {
			_, err := backupLockTestEngine(f).AcquireBackupLock(context.Background(), "database", "backup-1", 0)
			return err
		}},
	}
}

// heldLockFake reports the lock as held by holder (exact file bytes) with the
// given age output, and records whether the holder was ever removed.
func heldLockFake(path, holder, age string, removed *bool) *transport.Fake {
	return &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, path+".candidate.XXXXXX"):
			if *removed {
				return transport.Result{}, true
			}
			return transport.Result{ExitCode: lockHeldExitCode}, true
		case strings.Contains(cmd, "then rm -f "+q(path)+"; else exit"):
			*removed = true
			return transport.Result{}, true
		case strings.Contains(cmd, "then cat "+q(path)):
			return transport.Result{Stdout: holder}, true
		case strings.HasPrefix(cmd, "if [ -L "+q(path)):
			return transport.Result{Stdout: age}, true
		}
		return transport.Result{}, false
	}}
}

// Every lock is published by the same no-replace hard link. A noclobber
// redirect creates the lock before writing it, so a full disk left an EMPTY
// host or backup lock that every later runner read as contention.
func TestRemoteLocksPublishByAtomicLink(t *testing.T) {
	for _, tc := range remoteLockCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := &transport.Fake{}
			if err := tc.acquire(t, f, false); err != nil {
				t.Fatalf("acquire: %v\n%s", err, strings.Join(f.Commands, "\n"))
			}
			seq := strings.Join(f.Commands, "\n")
			if !strings.Contains(seq, q(tc.path+".candidate.XXXXXX")) || !strings.Contains(seq, `link "$tmp" `+q(tc.path)) {
				t.Fatalf("lock not published by an atomic hard link:\n%s", seq)
			}
			if strings.Contains(seq, "set -C") {
				t.Fatalf("lock still created with noclobber:\n%s", seq)
			}
		})
	}
}

// A create that fails for any reason other than "already held" (exit 80: the
// metadata write hit ENOSPC) is an infrastructure error, never contention.
func TestRemoteLocksReportCreateFailureAsInfrastructure(t *testing.T) {
	for _, tc := range remoteLockCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, tc.path+".candidate.XXXXXX") {
					return transport.Result{ExitCode: lockCreateFailedExitCode, Stderr: "No space left on device"}, true
				}
				return transport.Result{}, false
			}}
			err := tc.acquire(t, f, false)
			if err == nil || !strings.Contains(err.Error(), "No space left on device") || errors.Is(err, ErrBackupConflict) {
				t.Fatalf("error = %v, want the create failure surfaced", err)
			}
			if strings.Contains(strings.Join(f.Commands, "\n"), "then cat "+q(tc.path)) {
				t.Fatal("a failed create was inspected as if the lock were held")
			}
		})
	}
}

// An empty lock (what a failed noclobber write by an older runner leaves), a
// corrupt holder, or an unparsable age must be refused with an explanation —
// never reclaimed, however old the file looks.
func TestRemoteLocksRefuseUntrustworthyHolders(t *testing.T) {
	for _, holder := range []struct{ name, raw, age, want string }{
		{name: "empty", raw: "", age: "999999\n", want: "empty"},
		{name: "corrupt", raw: `{"owner":`, age: "999999\n", want: "not valid JSON"},
		{name: "unaged", raw: `{"owner":"x","deploy_id":"R1","operation_id":"backup-0"}`, age: "stat: weird\n", want: "not a number"},
	} {
		for _, tc := range remoteLockCases() {
			t.Run(holder.name+"/"+tc.name, func(t *testing.T) {
				removed := false
				f := heldLockFake(tc.path, holder.raw, holder.age, &removed)
				err := tc.acquire(t, f, false)
				if err == nil || !strings.Contains(err.Error(), "cannot be trusted") || !strings.Contains(err.Error(), holder.want) {
					t.Fatalf("error = %v, want an explanatory refusal mentioning %q", err, holder.want)
				}
				if errors.Is(err, ErrBackupConflict) {
					t.Fatalf("untrustworthy holder reported as retryable contention: %v", err)
				}
				if removed {
					t.Fatal("untrustworthy holder was reclaimed")
				}
			})
		}
	}
}

// --break-lock remains the operator's explicit escape hatch for a holder that
// cannot be read, on the locks that expose it.
func TestBreakLockRemovesUntrustworthyHolder(t *testing.T) {
	for _, tc := range remoteLockCases()[:2] {
		t.Run(tc.name, func(t *testing.T) {
			removed := false
			f := heldLockFake(tc.path, "", "0\n", &removed)
			if err := tc.acquire(t, f, true); err != nil {
				t.Fatalf("force acquire: %v\n%s", err, strings.Join(f.Commands, "\n"))
			}
			if !removed {
				t.Fatal("--break-lock did not remove the empty lock")
			}
		})
	}
}

// Over a real shell: an expired-looking empty backup lock — what a noclobber
// create left on a full disk — is refused and left in place; once an operator
// removes it, the lock is published with complete metadata.
func TestRealShellBackupLockRefusesEmptyLeftover(t *testing.T) {
	engine := New(
		&app.Resolved{Spec: &app.Spec{Name: "example", BasePath: t.TempDir()}, Env: "production"},
		nil, transport.NewLocal(),
		Options{Out: io.Discard, LockTTL: 10 * time.Second, Sleep: func(time.Duration) {}},
	)
	engine.lockVal = "app-lock"
	lock := engine.backupLockPath("database")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := engine.AcquireBackupLock(ctx, "database", "backup-1", 0); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty leftover lock: err = %v, want an explanatory refusal", err)
	}
	if info, err := os.Stat(lock); err != nil || info.Size() != 0 {
		t.Fatalf("empty leftover lock was touched: %v %v", info, err)
	}

	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	epoch, err := engine.AcquireBackupLock(ctx, "database", "backup-1", 0)
	if err != nil || epoch != 1 {
		t.Fatalf("acquire after cleanup: epoch %d err %v", epoch, err)
	}
	content, err := os.ReadFile(lock)
	if err != nil || !strings.Contains(string(content), `"operation_id":"backup-1"`) {
		t.Fatalf("published lock = %q, %v", content, err)
	}
	if candidates, _ := filepath.Glob(lock + ".candidate.*"); len(candidates) != 0 {
		t.Fatalf("temporary lock candidates leaked: %v", candidates)
	}
	engine.ReleaseBackupLock("database")
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release left the lock: %v", err)
	}
}

// Over a real shell, many runners racing for one backup lock: exactly one may
// win, every other one must see contention, and no candidate file may leak.
// This is the property the link-based create exists for.
func TestRealShellBackupLockAdmitsExactlyOneConcurrentRunner(t *testing.T) {
	base := t.TempDir()
	const runners = 8
	engines := make([]*Engine, runners)
	for i := range engines {
		engines[i] = New(
			&app.Resolved{Spec: &app.Spec{Name: "example", BasePath: base}, Env: "production"},
			nil, transport.NewLocal(),
			Options{Out: io.Discard, LockTTL: time.Minute, Sleep: func(time.Duration) {}},
		)
		engines[i].lockVal = "app-lock"
	}
	lock := engines[0].backupLockPath("database")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}

	errs := make([]error, runners)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range engines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = engines[i].AcquireBackupLock(context.Background(), "database", fmt.Sprintf("op-%d", i), 0)
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range errs {
		var conflict *BackupConflictError
		switch {
		case err == nil:
			winners++
		case errors.As(err, &conflict):
		default:
			t.Errorf("runner %d: %v, want success or backup contention", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d runners hold the backup lock at once, want exactly 1: %v", winners, errs)
	}
	if candidates, _ := filepath.Glob(lock + ".candidate.*"); len(candidates) != 0 {
		t.Fatalf("temporary lock candidates leaked: %v", candidates)
	}
}
