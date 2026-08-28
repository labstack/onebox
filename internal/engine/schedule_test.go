package engine

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

func TestSyncSchedulesRetainsManualScheduledJob(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "list-unit-files"):
			return transport.Result{}, true
		case strings.Contains(cmd, "systemd-analyze calendar"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "command -v flock"):
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
	artifacts := strings.Join(f.Inputs, "\n")
	for _, want := range []string{"TimeoutStartSec=1h", "Persistent=true", "schedule.lock", "run --rm --no-deps", "nightly"} {
		if !strings.Contains(artifacts, want) {
			t.Errorf("installed schedule artifacts are missing %q:\n%s", want, artifacts)
		}
	}
}

// A package upgrade cannot mutate an agentless target. ScheduleApply is the
// explicit bridge from a unit written by an older runner to the current unit
// contract, without requiring an unrelated release deploy.
func TestScheduleApplyUpgradesLegacyUnitsUnderRegime(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "45m", CatchUp: false},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "current/compose.yaml"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "list-unit-files"):
			// v2026.8.5 installed this timer, but its service had no bounded
			// runner or failure notifier. Presence must not make apply skip it.
			return transport.Result{Stdout: "ob-sample-nightly.timer\n"}, true
		case strings.Contains(cmd, "systemd-analyze calendar"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{
		Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production",
	})
	if err := e.ScheduleApply(context.Background(), "R9-schedule-apply"); err != nil {
		t.Fatalf("schedule apply: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	for _, want := range []string{
		`"phase":"schedule-apply","event":"start"`,
		"systemctl enable --now ob-sample-nightly.timer",
		`"phase":"schedule-apply","event":"finish","status":"ok"`,
		"rm -f '/var/lib/ob/sample/lock'",
	} {
		if !strings.Contains(seq, want) {
			t.Errorf("schedule apply is missing %q:\n%s", want, seq)
		}
	}
	artifacts := strings.Join(f.Inputs, "\n")
	for _, want := range []string{
		"ExecStart=/bin/sh /etc/systemd/system/ob-sample-nightly.run",
		"ExecStopPost=/bin/sh /etc/systemd/system/ob-sample-nightly.notify",
		"TimeoutStartSec=45m",
		"flock --exclusive --nonblock",
		"Persistent=false",
	} {
		if !strings.Contains(artifacts, want) {
			t.Errorf("upgraded artifacts are missing %q:\n%s", want, artifacts)
		}
	}
	for _, command := range f.Commands {
		if strings.Contains(command, "/etc/systemd/system/ob-sample-nightly") &&
			strings.Contains(command, ".ob-tmp") && !strings.Contains(command, "ob-fenced") {
			t.Errorf("schedule artifact write escaped the fence: %s", command)
		}
	}
}

func TestScheduleApplyRefusesBeforeFirstRelease(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := happyFake() // its current release has no Compose runtime
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "command -v flock") {
			return transport.Result{Stdout: "ok\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	err := e.ScheduleApply(context.Background(), "R9-schedule-apply")
	if err == nil || !strings.Contains(err.Error(), "deploy the application first") {
		t.Fatalf("error = %v, want first-release refusal", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if len(f.Inputs) != 0 || !strings.Contains(seq, "rm -f '/var/lib/ob/sample/lock'") {
		t.Fatalf("schedule apply wrote units or leaked its lock before refusing:\n%s", seq)
	}
}

func TestScheduleApplyStopsBeforeUnitWritesWhenJournalStartFails(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "current/compose.yaml"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, `"phase":"schedule-apply","event":"start"`):
			return transport.Result{ExitCode: 74, Stderr: "journal is read-only"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	err := e.ScheduleApply(context.Background(), "R9-schedule-apply")
	if err == nil || !strings.Contains(err.Error(), "journal schedule apply start") {
		t.Fatalf("error = %v, want journal refusal", err)
	}
	for _, command := range f.Commands {
		if strings.Contains(command, "/etc/systemd/system/ob-sample-nightly") {
			t.Fatalf("unit mutation followed failed journal start: %s", command)
		}
	}
}

func TestScheduledJobUnitContract(t *testing.T) {
	job := app.ScheduledJob{
		Name: "nightly", Cron: "0 2 * * *", Timezone: "UTC",
		Calendar: "*-*-* 02:00:00", Timeout: "45m", CatchUp: false, DeployLock: "exclusive",
	}
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil)
	service := scheduleServiceUnit("sample", job,
		"/etc/systemd/system/ob-sample-nightly.run",
		"/etc/systemd/system/ob-sample-nightly.notify")
	timer := scheduleTimerUnit("sample", job)

	for _, want := range []string{
		"exec 9>'/var/lib/ob/sample/schedule/nightly.lock'",
		"flock --exclusive --nonblock --conflict-exit-code 75 9",
		"exec 8>'/var/lib/ob/sample/schedule.lock'",
		"flock --exclusive --nonblock --conflict-exit-code 75 8",
		"/var/lib/ob/sample/lock",
		"application operation holds the deploy lock",
		"docker compose",
		"--project-directory",
		"/var/lib/ob/sample/current",
		"compose.yaml",
		"run --rm --no-deps --name 'sample-nightly-1'",
		"docker rm -f 'sample-nightly-1'",
		"nightly",
	} {
		if !strings.Contains(runner, want) {
			t.Errorf("runner is missing %q:\n%s", want, runner)
		}
	}
	command := exec.CommandContext(context.Background(), "sh", "-n")
	command.Stdin = strings.NewReader(runner)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("exclusive runner is not valid POSIX shell: %v: %s\n%s", err, output, runner)
	}
	for _, want := range []string{
		"Type=oneshot",
		"ExecStart=/bin/sh /etc/systemd/system/ob-sample-nightly.run",
		"ExecStopPost=/bin/sh /etc/systemd/system/ob-sample-nightly.notify",
		"TimeoutStartSec=45m",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service is missing %q:\n%s", want, service)
		}
	}
	for _, want := range []string{
		"OnCalendar=*-*-* 02:00:00 UTC",
		"Persistent=false",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer is missing %q:\n%s", want, timer)
		}
	}
	for _, artifact := range []string{runner, service, timer} {
		if strings.Contains(artifact, "Restart=") {
			t.Errorf("cron-shaped scheduled jobs must not retry implicitly:\n%s", artifact)
		}
	}
}

func TestPinnedScheduledJobRunnerLeasesImmutableRelease(t *testing.T) {
	job := app.ScheduledJob{Name: "refresh", DeployLock: "pinned"}
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", []app.EnvFile{
		{File: "config/runtime.env"},
		{File: "secrets/runtime.env", Provider: "sops"},
	})

	for _, want := range []string{
		"exec 9>'/var/lib/ob/sample/schedule/refresh.lock'",
		"flock --exclusive --nonblock --conflict-exit-code 75 9",
		"exec 8>'/var/lib/ob/sample/schedule.lock'",
		"release_dir=$(readlink -f '/var/lib/ob/sample/current')",
		"exec 7>>\"$release_dir/.ob-schedule.lease\"",
		"flock --shared 7",
		"flock --unlock 8",
		"trap cleanup 0",
		"pinned release has no compose.yaml",
		"/var/lib/ob/sample/schedule/refresh.state",
		"--project-directory \"$release_dir\"",
		"-f \"$release_dir\"/'compose.yaml'",
		"--env-file \"$release_dir\"/'config/runtime.env'",
		"run --rm --no-deps --name 'sample-refresh-1' 'refresh'",
		"docker rm -f 'sample-refresh-1'",
	} {
		if !strings.Contains(runner, want) {
			t.Errorf("pinned runner is missing %q:\n%s", want, runner)
		}
	}
	if strings.Contains(runner, "current/compose.yaml") {
		t.Fatalf("pinned runner executes through moving current link:\n%s", runner)
	}
	if strings.Contains(runner, "secrets/runtime.env") {
		t.Fatalf("encrypted env file was passed as a Compose interpolation input:\n%s", runner)
	}
	command := exec.CommandContext(context.Background(), "sh", "-n")
	command.Stdin = strings.NewReader(runner)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pinned runner is not valid POSIX shell: %v: %s\n%s", err, output, runner)
	}
}

func TestPinnedScheduleDeployConflictClassifiesLifecycleEffects(t *testing.T) {
	for _, tc := range []struct {
		effect app.DataEffect
		want   bool
	}{
		{effect: app.DataEffectNone, want: false},
		{effect: app.DataEffectMigration, want: true},
		{effect: app.DataEffectDestructive, want: true},
		{effect: app.DataEffectUnknown, want: true},
	} {
		t.Run(string(tc.effect), func(t *testing.T) {
			cfg := testConfig()
			migrate := cfg.Workloads["migrate"]
			migrate.DataEffect = tc.effect
			cfg.Workloads["migrate"] = migrate
			e := New(cfg, testProject(t), &transport.Fake{}, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			conflict := e.pinnedScheduleDeployConflict()
			if tc.want && !strings.Contains(conflict, "job migrate ("+string(tc.effect)+")") {
				t.Fatalf("effect %q was not classified: %q", tc.effect, conflict)
			}
			if !tc.want && conflict != "" {
				t.Fatalf("data-effect-free deployment was classified as conflicting: %q", conflict)
			}
		})
	}
}

func TestPinnedScheduledJobLockProtocol(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the installed runner targets Linux systemd hosts")
	}
	if _, err := os.Stat("/usr/bin/flock"); err != nil {
		t.Skip("util-linux flock is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	root := t.TempDir()
	names := app.Names{App: "sample", BasePath: root}
	releaseID := "20260828-120000-abc1234"
	releaseDir := names.ReleaseDir(releaseID)
	if err := os.MkdirAll(releaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", releaseID), names.CurrentLink()); err != nil {
		t.Fatal(err)
	}

	started := filepath.Join(root, "started")
	stop := filepath.Join(root, "stop")
	removed := filepath.Join(root, "removed")
	stub := filepath.Join(root, "docker-stub")
	stubBody := "#!/bin/sh\nset -eu\n" +
		"if [ \"${1:-}\" = rm ]; then touch " + q(removed) + "; exit 0; fi\n" +
		"touch " + q(started) + "\n" +
		"while [ ! -e " + q(stop) + " ]; do sleep 0.01; done\n"
	if err := os.WriteFile(stub, []byte(stubBody), 0o700); err != nil {
		t.Fatal(err)
	}

	job := app.ScheduledJob{Name: "refresh", DeployLock: "pinned"}
	runner := scheduleRunnerScript("sample", job, names, filepath.Join(names.AppDir(), "lock"), nil)
	runner = strings.ReplaceAll(runner, "/usr/bin/docker", q(stub))
	command := exec.CommandContext(ctx, "sh")
	command.Stdin = strings.NewReader(runner)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if finished {
			return
		}
		_ = os.WriteFile(stop, nil, 0o600)
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pinned runner did not reach the container command")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(removed); err != nil {
		t.Fatalf("initial stale-container cleanup did not run: %v", err)
	}

	assertLock := func(path string, available bool) {
		t.Helper()
		err := exec.CommandContext(ctx, "/usr/bin/flock", "--exclusive", "--nonblock", "--conflict-exit-code", "75", path, "true").Run()
		if available && err != nil {
			t.Fatalf("lock %s remained unavailable: %v", path, err)
		}
		if !available && err == nil {
			t.Fatalf("lock %s was not held", path)
		}
	}
	assertLock(names.ScheduleRunLock(), true)
	assertLock(names.ScheduledJobRunLock(job.Name), false)
	assertLock(filepath.Join(releaseDir, ".ob-schedule.lease"), false)
	leases, err := release.ActiveScheduleLeases(ctx, transport.NewLocal(), names)
	if err != nil || len(leases) != 1 || leases[0] != releaseID {
		t.Fatalf("active release lease was not observable: leases=%v err=%v", leases, err)
	}
	cfg := testConfig()
	cfg.BasePath = root
	cfg.Workloads[job.Name] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "6h", CatchUp: true, DeployLock: "pinned"},
	}
	deploy := New(cfg, testProject(t), transport.NewLocal(), Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if _, err := deploy.acquireLock(ctx, "concurrent-deploy", false, pinnedScheduleLeasePolicy{allow: true}); err != nil {
		t.Fatalf("deployment could not acquire its lock while the pinned job ran: %v", err)
	}
	deploy.ReleaseLock(ctx)
	if _, err := deploy.AcquireLock(ctx, "concurrent-operation", false); err == nil || !strings.Contains(err.Error(), "pinned scheduled jobs") {
		t.Fatalf("ordinary application operation was not blocked by the pinned job: %v", err)
	}
	cfg.Hooks = map[string]app.Command{"pre_release": {Run: "change-shared-data"}}
	riskyDeploy := New(cfg, testProject(t), transport.NewLocal(), Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	conflict := riskyDeploy.pinnedScheduleDeployConflict()
	if conflict == "" || !strings.Contains(conflict, "hook pre_release") {
		t.Fatalf("risky deployment conflict was not classified: %q", conflict)
	}
	if _, err := riskyDeploy.acquireLock(ctx, "risky-deploy", false, pinnedScheduleLeasePolicy{conflict: conflict}); err == nil || !strings.Contains(err.Error(), "hook pre_release") {
		t.Fatalf("risky deployment was not blocked by the pinned job: %v", err)
	}

	if err := os.WriteFile(stop, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	finished = true
	if _, err := os.Stat(removed); err != nil {
		t.Fatalf("completed run did not clean its named container: %v", err)
	}
	assertLock(filepath.Join(releaseDir, ".ob-schedule.lease"), true)
	leases, err = release.ActiveScheduleLeases(ctx, transport.NewLocal(), names)
	if err != nil || len(leases) != 0 {
		t.Fatalf("completed release remained leased: leases=%v err=%v", leases, err)
	}
	if _, err := os.Stat(names.ScheduledJobRunState(job.Name)); !os.IsNotExist(err) {
		t.Fatalf("runner state survived completion: %v", err)
	}
}

func TestScheduledJobFailureNotifierUsesConfiguredWebhooks(t *testing.T) {
	cfg := testConfig()
	cfg.Notifications = map[string]app.Notification{
		"ops": {
			Webhook: "https://hooks.example.com/secret-path",
			On:      []string{"failure"}, Format: "json",
		},
		"success-only": {
			Webhook: "https://hooks.example.com/success",
			On:      []string{"success"}, Format: "text",
		},
	}
	f := &transport.Fake{TargetName: "root@example.internal"}
	e := New(cfg, testProject(t), f, Options{Environment: "production", Out: &bytes.Buffer{}, Sleep: noSleep})
	script, err := e.scheduleFailureNotifier("nightly")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`${SERVICE_RESULT:-success}`,
		"exec 9>'/var/lib/ob/sample/schedule/nightly.lock'",
		"flock --exclusive --nonblock 9",
		"docker rm -f 'sample-nightly-1'",
		`ts=$(date -u`,
		`"$ts"`,
		"curl --fail --silent --show-error --max-time 5 --request POST",
		"Content-Type: application/json",
		"X-Title: sample scheduled job nightly",
		"https://hooks.example.com/secret-path",
		`"host":"root@example.internal"`,
		`"status":"fail"`,
		"wait || true",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("failure notifier is missing %q:\n%s", want, script)
		}
	}
	for _, forbidden := range []string{
		"https://hooks.example.com/success",
		scheduleNotificationTimestamp,
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("failure notifier contains %q:\n%s", forbidden, script)
		}
	}
}

func TestSyncSchedulesRefusesMissingFlockBeforeInstallingUnits(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "list-unit-files"):
			return transport.Result{}, true
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.SyncSchedules(context.Background())
	if err == nil || !strings.Contains(err.Error(), "install util-linux") {
		t.Fatalf("error = %v, want actionable flock refusal", err)
	}
	if len(f.Inputs) != 0 {
		t.Fatalf("unit files were written before the capability refusal: %d", len(f.Inputs))
	}
}

func TestScheduleStatusSurfacesTheLastSystemdFailure(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@nightly:service
LoadState=loaded
ActiveState=failed
Result=timeout
ExecMainStatus=15
@@nightly:timer
LoadState=loaded
ActiveState=active
`}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || !statuses[0].Diverged || statuses[0].LastResult != "timeout" || statuses[0].LastExitStatus != 15 {
		t.Fatalf("failed systemd result was not surfaced: %#v", statuses)
	}
	if !strings.Contains(strings.Join(statuses[0].Issues, "\n"), "last run failed") {
		t.Fatalf("failure has no actionable issue: %#v", statuses[0])
	}
}

func TestScheduleStatusReportsRunningPinnedRelease(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["refresh"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "6h", CatchUp: true, DeployLock: "pinned"},
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@refresh:service
LoadState=loaded
ActiveState=activating
Result=success
ExecMainStatus=0
@@refresh:timer
LoadState=loaded
ActiveState=active
@@refresh:run
release=20260828-120000-abc1234
started_at=2026-08-28T12:01:02Z
`}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %#v", statuses)
	}
	got := statuses[0]
	if !got.Running || got.DeployLock != "pinned" || got.Timeout != "6h" ||
		got.PinnedRelease != "20260828-120000-abc1234" || got.StartedAt != "2026-08-28T12:01:02Z" || got.Diverged {
		t.Fatalf("running pinned status was not surfaced: %#v", got)
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
	if !strings.Contains(seq, "rm -f /etc/systemd/system/ob-sample-nightly.timer /etc/systemd/system/ob-sample-nightly.service /etc/systemd/system/ob-sample-nightly.run /etc/systemd/system/ob-sample-nightly.notify") {
		t.Fatalf("disable failure stranded the unit files:\n%s", seq)
	}
	if !strings.Contains(seq, "systemctl daemon-reload") {
		t.Fatalf("systemd was not reloaded after removing the unit files:\n%s", seq)
	}
	if strings.Contains(seq, "systemctl disable --now ob-sample-nightly.timer >/dev/null 2>&1") {
		t.Fatalf("disable stderr was discarded instead of captured:\n%s", seq)
	}
}

func TestRuntimePrefixStopsAtEscapedComponentBoundary(t *testing.T) {
	tests := []struct {
		name   string
		unit   string
		prefix string
		want   bool
	}{
		{"job owned", "ob-acme-nightly", "ob-acme-", true},
		{"hyphenated job owner", "ob-acme--web-nightly", "ob-acme--web-", true},
		{"job belongs to hyphen extension", "ob-acme--web-nightly", "ob-acme-", false},
		{"backup environment owned", "ob-backup-acme-prod-postgres-backup", "ob-backup-acme-prod-", true},
		{"hyphenated backup environment owned", "ob-backup-acme-prod--eu-postgres-backup", "ob-backup-acme-prod--eu-", true},
		{"backup belongs to hyphen extension", "ob-backup-acme-prod--eu-postgres-backup", "ob-backup-acme-prod-", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesRuntimePrefix(test.unit, test.prefix); got != test.want {
				t.Fatalf("matchesRuntimePrefix(%q, %q) = %t, want %t", test.unit, test.prefix, got, test.want)
			}
		})
	}
}

func TestScheduleReconciliationDoesNotCrossEscapedApplicationBoundary(t *testing.T) {
	listed := strings.Join([]string{
		"ob-acme--web-nightly.timer",
		"ob-backup-acme--web-production-postgres-backup.timer",
		"",
	}, "\n")
	for _, test := range []struct {
		name string
		run  func(*Engine) error
	}{
		{"sync", func(e *Engine) error { return e.SyncSchedules(context.Background()) }},
		{"remove", func(e *Engine) error { return e.RemoveSchedules(context.Background()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, "list-unit-files") {
					return transport.Result{Stdout: listed}, true
				}
				return transport.Result{}, false
			}}
			cfg := testConfig()
			cfg.Spec.Name = "acme"
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			if err := test.run(e); err != nil {
				t.Fatal(err)
			}
			if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "rm -f") {
				t.Fatalf("%s removed a hyphen-extension application's schedule:\n%s", test.name, seq)
			}
		})
	}
}

func TestBackupScheduleSyncDoesNotCrossEscapedEnvironmentBoundary(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "list-unit-files") {
			return transport.Result{Stdout: "ob-backup-acme-prod--eu-postgres-backup.timer\n"}, true
		}
		return transport.Result{}, false
	}}
	cfg := testConfig()
	cfg.Spec.Name = "acme"
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "prod"})
	if err := e.SyncBackupSchedules(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "rm -f") {
		t.Fatalf("backup sync removed a hyphen-extension environment's schedule:\n%s", seq)
	}
}

func TestScheduleSyncIgnoresInvalidHostListedUnitNames(t *testing.T) {
	for _, test := range []struct {
		name   string
		listed string
		run    func(*Engine) error
	}{
		{"job", "ob-sample-nightly;touch.timer\n", func(e *Engine) error { return e.SyncSchedules(context.Background()) }},
		{"backup", "ob-backup-sample-production-postgres;touch.timer\n", func(e *Engine) error { return e.SyncBackupSchedules(context.Background()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, "list-unit-files") {
					return transport.Result{Stdout: test.listed}, true
				}
				return transport.Result{}, false
			}}
			e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
			if err := test.run(e); err != nil {
				t.Fatal(err)
			}
			if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "rm -f") {
				t.Fatalf("sync interpolated an invalid host-listed unit name:\n%s", seq)
			}
		})
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

func TestScheduleOwnershipComesFromServiceBody(t *testing.T) {
	tests := []struct {
		name   string
		backup bool
		body   string
		want   bool
	}{
		{"owned job", false, "Description=Onebox scheduled job nightly for help-desk\n", true},
		{"other job", false, "Description=Onebox scheduled job nightly for help\n", false},
		{"owned backup current", true, "Description=Onebox backup verify for database (help-desk/production)\n", true},
		{"owned backup legacy", true, "Description=Onebox backup verify for database (help-desk)\n", true},
		{"other environment backup", true, "Description=Onebox backup verify for database (help-desk/staging)\n", false},
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
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
			got, err := e.scheduleUnitBelongsToOwner(context.Background(), "legacy", test.backup)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("ownership = %t, want %t", got, test.want)
			}
		})
	}
}

func TestBackupServiceUnitRecordsEnvironmentOwnership(t *testing.T) {
	body := backupServiceUnit("sample", "production", "postgres", "backup", "/tmp/lock", []string{"true"})
	if !strings.Contains(body, "Description=Onebox backup backup for postgres (sample/production)") {
		t.Fatalf("backup service unit has no exact environment owner:\n%s", body)
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
