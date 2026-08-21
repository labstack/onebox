package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestNextEpochFailsClosed(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name   string
		result transport.Result
		want   int
		err    bool
	}{
		{name: "absent", result: transport.Result{ExitCode: app.ProbeAbsent}, want: 1},
		{name: "valid", result: transport.Result{Stdout: "41\n"}, want: 42},
		{name: "empty", result: transport.Result{}, err: true},
		{name: "malformed", result: transport.Result{Stdout: "not-an-epoch\n"}, err: true},
		{name: "negative", result: transport.Result{Stdout: "-1\n"}, err: true},
		{name: "overflow", result: transport.Result{Stdout: strconv.FormatUint(uint64(maxInt)+1, 10)}, err: true},
		{name: "unreadable", result: transport.Result{ExitCode: app.ProbeUnreadable}, err: true},
		{name: "not-regular", result: transport.Result{ExitCode: app.ProbeNotRegular}, err: true},
		{name: "undetermined", result: transport.Result{ExitCode: app.ProbeUndetermined}, err: true},
		{name: "broken-parent", result: transport.Result{ExitCode: app.ProbeStatePathNotDirectory}, err: true},
		{name: "probe-failure", result: transport.Result{ExitCode: 23, Stderr: "probe failed"}, err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if command == epochProbeCmd("/state/epoch") {
					return test.result, true
				}
				return transport.Result{}, false
			}}
			engine := &Engine{T: fake}
			got, err := engine.nextEpoch(context.Background(), "/state/epoch")
			if test.err {
				if err == nil || !strings.Contains(err.Error(), "epoch") {
					t.Fatalf("next epoch error = %v", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("next epoch = %d, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestEpochProbeBehavior(t *testing.T) {
	dir := t.TempDir()
	run := func(epochPath string) transport.Result {
		t.Helper()
		result, err := transport.NewLocal().Run(context.Background(), epochProbeCmd(epochPath))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	epochPath := filepath.Join(dir, "epoch")
	if result := run(epochPath); result.ExitCode != app.ProbeAbsent {
		t.Fatalf("absent epoch exit = %d, want %d", result.ExitCode, app.ProbeAbsent)
	}
	if err := os.WriteFile(epochPath, []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := run(epochPath); result.ExitCode != 0 || result.Stdout != "7\n" {
		t.Fatalf("regular epoch = %#v", result)
	}
	if err := os.Remove(epochPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(epochPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if result := run(epochPath); result.ExitCode != app.ProbeNotRegular {
		t.Fatalf("directory epoch exit = %d, want %d", result.ExitCode, app.ProbeNotRegular)
	}
	if err := os.Remove(epochPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), epochPath); err != nil {
		t.Fatal(err)
	}
	if result := run(epochPath); result.ExitCode != app.ProbeNotRegular {
		t.Fatalf("symlink epoch exit = %d, want %d", result.ExitCode, app.ProbeNotRegular)
	}
}

func TestAtomicEpochWriteReplacesThroughSiblingTemp(t *testing.T) {
	dir := t.TempDir()
	epochPath := filepath.Join(dir, "epoch")
	if err := os.WriteFile(epochPath, []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := atomicEpochWriteCmd(epochPath, 8)
	for _, want := range []string{"umask 077", "mktemp", `trap 'rm -f "$tmp"'`, `> "$tmp"`, "chmod 600", `mv -f "$tmp"`} {
		if !strings.Contains(command, want) {
			t.Fatalf("atomic epoch command missing %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "> "+q(epochPath)) {
		t.Fatalf("epoch target is truncated directly:\n%s", command)
	}
	result, err := transport.NewLocal().Run(context.Background(), command)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("atomic epoch write: exit=%d err=%v stderr=%s", result.ExitCode, err, result.Stderr)
	}
	body, err := os.ReadFile(epochPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, []byte("8\n")) {
		t.Fatalf("epoch body = %q", body)
	}
	info, err := os.Stat(epochPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("epoch mode = %o", info.Mode().Perm())
	}
	leftovers, err := filepath.Glob(epochPath + ".tmp.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("epoch temp files remain: %v", leftovers)
	}
}

func TestAtomicEpochWriteFailurePreservesPreviousValue(t *testing.T) {
	dir := t.TempDir()
	epochPath := filepath.Join(dir, "epoch")
	if err := os.WriteFile(epochPath, []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "mv"), []byte("#!/bin/sh\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := "PATH=" + q(binDir) + ":$PATH; export PATH; " + atomicEpochWriteCmd(epochPath, 8)
	result, err := transport.NewLocal().Run(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatal("simulated interrupted rename unexpectedly succeeded")
	}
	body, err := os.ReadFile(epochPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, []byte("7\n")) {
		t.Fatalf("failed atomic write changed previous epoch to %q", body)
	}
}

type epochAcquisitionCase struct {
	name   string
	result transport.Result
	want   int
	err    bool
}

func epochAcquisitionCases() []epochAcquisitionCase {
	maxInt := int(^uint(0) >> 1)
	return []epochAcquisitionCase{
		{name: "absent", result: transport.Result{ExitCode: app.ProbeAbsent}, want: 1},
		{name: "valid", result: transport.Result{Stdout: "41\n"}, want: 42},
		{name: "unreadable", result: transport.Result{ExitCode: app.ProbeUnreadable}, err: true},
		{name: "empty", result: transport.Result{}, err: true},
		{name: "malformed", result: transport.Result{Stdout: "broken\n"}, err: true},
		{name: "negative", result: transport.Result{Stdout: "-1\n"}, err: true},
		{name: "max-value", result: transport.Result{Stdout: strconv.Itoa(maxInt)}, err: true},
		{name: "overflowing", result: transport.Result{Stdout: strconv.FormatUint(uint64(maxInt)+1, 10)}, err: true},
	}
}

func TestApplicationLockEpochMatrix(t *testing.T) {
	for _, test := range epochAcquisitionCases() {
		t.Run(test.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if command == epochProbeCmd("/var/lib/ob/sample/epoch") {
					return test.result, true
				}
				return transport.Result{}, false
			}}
			engine := lockEngine(t, fake)
			got, err := engine.AcquireLock(context.Background(), "same-operation", false)
			assertEpochAcquisition(t, fake, got, err, test)
		})
	}
}

func TestBackupLockEpochMatrix(t *testing.T) {
	for _, test := range epochAcquisitionCases() {
		t.Run(test.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if command == epochProbeCmd("/var/lib/ob/example/backup/locks/database.epoch") {
					return test.result, true
				}
				return transport.Result{}, false
			}}
			engine := backupLockTestEngine(fake)
			got, err := engine.AcquireBackupLock(context.Background(), "database", "same-operation", 0)
			assertEpochAcquisition(t, fake, got, err, test)
		})
	}
}

func assertEpochAcquisition(t *testing.T, fake *transport.Fake, got int, err error, test epochAcquisitionCase) {
	t.Helper()
	if test.err {
		if err == nil {
			t.Fatal("invalid epoch was accepted")
		}
		if strings.Contains(strings.Join(fake.Commands, "\n"), "set -C") {
			t.Fatalf("lock was created after epoch validation failed:\n%s", strings.Join(fake.Commands, "\n"))
		}
		return
	}
	if err != nil || got != test.want {
		t.Fatalf("acquire epoch = %d, %v; want %d", got, err, test.want)
	}
}

func TestApplicationEpochPersistenceFailureReleasesLock(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "mktemp '/var/lib/ob/sample/epoch.tmp.XXXXXX'") {
			return transport.Result{ExitCode: 23, Stderr: "rename interrupted"}, true
		}
		return transport.Result{}, false
	}}
	engine := lockEngine(t, fake)
	if _, err := engine.AcquireLock(context.Background(), "operation", false); err == nil {
		t.Fatal("acquisition succeeded after epoch persistence failed")
	}
	if engine.lockVal != "" || !strings.Contains(strings.Join(fake.Commands, "\n"), "then rm -f '/var/lib/ob/sample/lock'") {
		t.Fatalf("failed acquisition left its lock published:\n%s", strings.Join(fake.Commands, "\n"))
	}
}

func TestBackupEpochPersistenceFailureReleasesLock(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "mktemp '/var/lib/ob/example/backup/locks/database.epoch.tmp.XXXXXX'") {
			return transport.Result{ExitCode: 23, Stderr: "rename interrupted"}, true
		}
		return transport.Result{}, false
	}}
	engine := backupLockTestEngine(fake)
	if _, err := engine.AcquireBackupLock(context.Background(), "database", "operation", 0); err == nil {
		t.Fatal("backup acquisition succeeded after epoch persistence failed")
	}
	if engine.backupLockVals["database"] != "" || engine.backupFenceVals["database"] != "" ||
		!strings.Contains(strings.Join(fake.Commands, "\n"), "then rm -f '/var/lib/ob/example/backup/locks/database.lock'") {
		t.Fatalf("failed backup acquisition left its lock or fence published:\n%s", strings.Join(fake.Commands, "\n"))
	}
}

func TestRealShellApplicationEpochLifecycle(t *testing.T) {
	cfg := testConfig()
	cfg.BasePath = t.TempDir()
	engine := New(cfg, testProject(t), transport.NewLocal(), Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	ctx := context.Background()

	first, err := engine.AcquireLock(ctx, "same-operation", false)
	if err != nil || first != 1 {
		t.Fatalf("first acquisition = %d, %v; want 1", first, err)
	}
	engine.ReleaseLock(ctx)
	second, err := engine.AcquireLock(ctx, "same-operation", false)
	if err != nil || second != 2 {
		t.Fatalf("same-operation reacquisition = %d, %v; want 2", second, err)
	}
	engine.ReleaseLock(ctx)

	if err := os.WriteFile(engine.epochPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.AcquireLock(ctx, "same-operation", false); err == nil {
		t.Fatal("real shell accepted an empty application epoch")
	}
	if _, err := os.Stat(engine.lockPath()); !os.IsNotExist(err) {
		t.Fatalf("application lock exists after epoch refusal: %v", err)
	}
}

func TestRealShellBackupEpochLifecycle(t *testing.T) {
	cfg := testConfig()
	cfg.BasePath = t.TempDir()
	engine := New(cfg, testProject(t), transport.NewLocal(), Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	ctx := context.Background()
	if _, err := engine.AcquireLock(ctx, "deploy-operation", false); err != nil {
		t.Fatal(err)
	}
	defer engine.ReleaseLock(ctx)

	first, err := engine.AcquireBackupLock(ctx, "database", "same-operation", 0)
	if err != nil || first != 1 {
		t.Fatalf("first backup acquisition = %d, %v; want 1", first, err)
	}
	engine.ReleaseBackupLock("database")
	second, err := engine.AcquireBackupLock(ctx, "database", "same-operation", 0)
	if err != nil || second != 2 {
		t.Fatalf("same-operation backup reacquisition = %d, %v; want 2", second, err)
	}
	engine.ReleaseBackupLock("database")

	if err := os.WriteFile(engine.backupEpochPath("database"), []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.AcquireBackupLock(ctx, "database", "same-operation", 0); err == nil {
		t.Fatal("real shell accepted a malformed backup epoch")
	}
	if _, err := os.Stat(engine.backupLockPath("database")); !os.IsNotExist(err) {
		t.Fatalf("backup lock exists after epoch refusal: %v", err)
	}
}
