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

func TestRemoveSchedulesRemovesFilesAndReloadsAfterFailedDisable(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "disable schedule ob-sample-nightly failed (exit 5): unit is busy") {
		t.Fatalf("remove schedules error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "rm -f /etc/systemd/system/ob-sample-nightly.timer /etc/systemd/system/ob-sample-nightly.service") {
		t.Fatalf("disable failure stranded the unit files:\n%s", seq)
	}
	if !strings.Contains(seq, "systemctl daemon-reload") {
		t.Fatalf("systemd was not reloaded after removing the unit files:\n%s", seq)
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

// Teardown has to take both namespaces with it.
//
// Backup timers are named outside the job scheduler's namespace on purpose —
// a deploy used to treat them as "no longer declared" and delete every
// scheduled backup. Teardown is the opposite case: matching only the job
// prefix left `ob destroy` with ob-backup-<app>-… timers still loaded, firing
// against a release directory the same command had just deleted.
func TestRemoveSchedulesTakesBackupTimersToo(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "list-unit-files") {
			return transport.Result{Stdout: strings.Join([]string{
				"ob-sample-nightly.timer",
				"ob-backup-sample-production-postgres-backup.timer",
				"ob-backup-sample-production-postgres-verify.timer",
				// Another application's, and a stranger's. Neither is ours.
				"ob-backup-other-production-postgres-backup.timer",
				"logrotate.timer",
				"",
			}, "\n")}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RemoveSchedules(context.Background()); err != nil {
		t.Fatalf("remove schedules: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	for _, want := range []string{
		"ob-sample-nightly",
		"ob-backup-sample-production-postgres-backup",
		"ob-backup-sample-production-postgres-verify",
	} {
		if !strings.Contains(seq, "rm -f /etc/systemd/system/"+want+".timer") {
			t.Errorf("teardown left %s installed:\n%s", want, seq)
		}
	}
	for _, never := range []string{"ob-backup-other-production", "logrotate"} {
		if strings.Contains(seq, never) {
			t.Errorf("teardown removed a unit that is not this application's (%s):\n%s", never, seq)
		}
	}
}

func TestLegacyScheduleOwnershipComesFromServiceBody(t *testing.T) {
	tests := []struct {
		name   string
		backup bool
		body   string
		want   bool
	}{
		{"owned job", false, "Description=Onebox scheduled job nightly for help-desk\n", true},
		{"other job", false, "Description=Onebox scheduled job nightly for help\n", false},
		{"owned backup", true, "Description=Onebox backup verify for database (help-desk)\n", true},
		{"other backup", true, "Description=Onebox backup verify for database (help)\n", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.HasPrefix(cmd, "cat ") {
					return transport.Result{Stdout: test.body}, true
				}
				return transport.Result{}, false
			}}
			cfg := testConfig()
			cfg.Spec.Name = "help-desk"
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			got, err := e.legacyScheduleUnitBelongsToApplication(context.Background(), "legacy", test.backup)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("ownership = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAppNamedBackupDoesNotOwnEveryBackupTimer(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "list-unit-files") {
			return transport.Result{Stdout: "ob-backup-other-production-postgres-backup.timer\n"}, true
		}
		return transport.Result{}, false
	}}
	cfg := testConfig()
	cfg.Spec.Name = "backup"
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RemoveSchedules(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "rm -f") {
		t.Fatalf("app named backup removed another application's timer:\n%s", seq)
	}
}
