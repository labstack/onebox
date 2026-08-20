package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestSyncSchedulesRetainsManualScheduledJob(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.Schedule{Cron: "0 2 * * *", Timezone: "UTC"},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "list-unit-files"):
			return transport.Result{}, true
		case strings.Contains(cmd, "systemd-analyze calendar"):
			return transport.Result{Stdout: "ok\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.SyncSchedules(context.Background()); err != nil {
		t.Fatalf("sync schedules: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	for _, want := range []string{"ob-sample-nightly.service", "ob-sample-nightly.timer", "systemctl enable --now ob-sample-nightly.timer"} {
		if !strings.Contains(seq, want) {
			t.Fatalf("manual scheduled job omitted %q:\n%s", want, seq)
		}
	}
}

func TestRemoveSchedulesRejectsFailedDisable(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "list-unit-files"):
			return transport.Result{Stdout: "ob-sample-nightly.timer\n"}, true
		case strings.Contains(cmd, "systemctl disable --now"):
			return transport.Result{ExitCode: 5, Stderr: "unit is busy"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.RemoveSchedules(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remove schedule ob-sample-nightly failed (exit 5): unit is busy") {
		t.Fatalf("remove schedules error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "timer >/dev/null 2>&1 && rm -f") {
		t.Fatalf("disable failure must prevent unit-file removal:\n%s", seq)
	}
	if strings.Contains(seq, "systemctl daemon-reload") {
		t.Fatalf("schedule removal continued after disable failure:\n%s", seq)
	}
}

// A deploy must not delete the backup timers.
//
// SyncSchedules owns "ob-<app>-*" and removes what the project no longer
// declares. Backup timers were named inside that namespace, so every deploy
// reclaimed them as stale and silently stopped all scheduled backups — the only
// trace being a line saying the schedule was "no longer declared".
func TestSyncSchedulesLeavesBackupTimersAlone(t *testing.T) {
	if !strings.HasPrefix(app.BackupUnitPrefix, "ob-") {
		t.Fatalf("backup prefix %q is expected to sit under the ob- namespace", app.BackupUnitPrefix)
	}
	backupTimer := app.Names{App: "example", BasePath: "/var/lib/ob"}.
		BackupTimerForEnvironment("production", "database", "backup")
	if strings.HasPrefix(backupTimer, "ob-example-") {
		t.Fatalf("backup timer %q is inside the job scheduler's namespace and a deploy would delete it", backupTimer)
	}
}
