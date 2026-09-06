package engine

import (
	"bytes"
	"context"
	"encoding/json"
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
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	service := scheduleServiceUnit("sample", job,
		"/etc/systemd/system/ob-sample-nightly.run",
		"/etc/systemd/system/ob-sample-nightly.notify")
	timer := scheduleTimerUnit("sample", job)

	for _, want := range []string{
		"exec 9>'/var/lib/ob/sample/schedule/nightly.lock'",
		"flock --exclusive --nonblock 9 || stand_aside",
		"exec 8>'/var/lib/ob/sample/schedule.lock'",
		"flock --exclusive --nonblock 8 || skip",
		"/var/lib/ob/sample/lock",
		"application operation holds the deploy lock",
		"docker compose",
		"--project-directory",
		"/var/lib/ob/sample/current",
		"compose.yaml",
		`run --rm --no-deps "$@" --name 'sample-nightly-1'`,
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
	}, 10*time.Minute, true)

	for _, want := range []string{
		"exec 9>'/var/lib/ob/sample/schedule/refresh.lock'",
		"flock --exclusive --nonblock 9 || stand_aside",
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
		`run --rm --no-deps "$@" --name 'sample-refresh-1' 'refresh'`,
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
	runner := scheduleRunnerScript("sample", job, names, filepath.Join(names.AppDir(), "lock"), nil, 10*time.Minute, true)
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
	// The notifier, not the runner, removes the state: it is the evidence
	// ExecStopPost turns into the run record.
	if _, err := os.Stat(names.ScheduledJobRunState(job.Name)); err != nil {
		t.Fatalf("runner removed the state the notifier finalises: %v", err)
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
	script, err := e.scheduleNotifier(app.ScheduledJob{Name: "nightly", Notify: []string{"failure", "timeout"}})
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

func TestScheduledJobRunnersRecordRunStateForTheNotifier(t *testing.T) {
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	for _, tc := range []struct {
		name string
		job  app.ScheduledJob
	}{
		{"exclusive", app.ScheduledJob{Name: "nightly", Cron: "0 2 * * *", Timezone: "UTC", Calendar: "*-*-* 02:00:00", Timeout: "45m", DeployLock: "exclusive"}},
		{"pinned", app.ScheduledJob{Name: "nightly", Cron: "0 2 * * *", Timezone: "UTC", Calendar: "*-*-* 02:00:00", Timeout: "45m", DeployLock: "pinned"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := scheduleRunnerScript("sample", tc.job, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
			for _, want := range []string{
				"state='/var/lib/ob/sample/schedule/nightly.state'",
				"write_state() {",
				"started_epoch=%s",
				"trigger=%s",
				"write_state 1",
				"mv -f \"$tmp\" \"$state\"",
			} {
				if !strings.Contains(runner, want) {
					t.Errorf("%s runner is missing %q:\n%s", tc.name, want, runner)
				}
			}
			if strings.Contains(runner, `rm -f "$state"`) {
				t.Errorf("%s runner removes the state the notifier finalises:\n%s", tc.name, runner)
			}
			command := exec.CommandContext(context.Background(), "sh", "-n")
			command.Stdin = strings.NewReader(runner)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("%s runner is not valid POSIX shell: %v: %s\n%s", tc.name, err, output, runner)
			}
		})
	}
	service := scheduleServiceUnit("sample", app.ScheduledJob{Name: "nightly", Timeout: "45m"},
		"/etc/systemd/system/ob-sample-nightly.run", "/etc/systemd/system/ob-sample-nightly.notify")
	if strings.Contains(service, "SuccessExitStatus") {
		t.Errorf("a skip is recorded by the runner and exits 0; the unit needs no exit-status remap:\n%s", service)
	}
}

func TestScheduledJobNotifierWritesOneRunRecordToTheJournal(t *testing.T) {
	cfg := testConfig()
	f := &transport.Fake{TargetName: "root@example.internal"}
	e := New(cfg, testProject(t), f, Options{Environment: "production", Out: &bytes.Buffer{}, Sleep: noSleep})
	script, err := e.scheduleNotifier(app.ScheduledJob{Name: "nightly", Notify: []string{"failure", "timeout"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"state='/var/lib/ob/sample/schedule/nightly.state'",
		`started_epoch=*) started_epoch=${line#started_epoch=}`,
		`rm -f "$state"`,
		`result=${SERVICE_RESULT:-success}`,
		`status=${EXIT_STATUS:-0}`,
		`outcome=timeout`,
		`outcome=skipped`,
		`outcome=success`,
		`outcome=failure`,
		`"run":"%s","job":"%s","trigger":"%s","operation":"%s","release":"%s"`,
		`"duration_s":%s,"attempts":%s,"exit_status":%s,"outcome":"%s","reason":"%s","inputs":{%s}`,
		`"${INVOCATION_ID:-}" 'nightly'`,
		`SYSLOG_IDENTIFIER=ob-run\nONEBOX_APP=%s\nONEBOX_UNIT=%s\nONEBOX_JOB=%s`,
		`"$record" 'sample' 'ob-sample-nightly' 'nightly' | logger --journald`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("notifier is missing %q:\n%s", want, script)
		}
	}
	command := exec.CommandContext(context.Background(), "sh", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("notifier is not valid POSIX shell: %v: %s\n%s", err, output, script)
	}
}

// runNotifier executes the generated ExecStopPost script with stub logger and
// curl binaries, the way systemd would after a run. It returns the record the
// script wrote, whether the state file survived, and every curl invocation's
// arguments, one per element.
func runNotifier(t *testing.T, job app.ScheduledJob, notifications map[string]app.Notification, state string, env map[string]string) (map[string]any, bool, []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	base := t.TempDir()
	cfg := testConfig()
	cfg.BasePath = base
	cfg.Notifications = notifications
	e := New(cfg, testProject(t), &transport.Fake{TargetName: "root@example.internal"}, Options{Environment: "production", Out: &bytes.Buffer{}, Sleep: noSleep})
	script, err := e.scheduleNotifier(job)
	if err != nil {
		t.Fatal(err)
	}
	scheduleDir := filepath.Join(base, "sample", "schedule")
	if err := os.MkdirAll(scheduleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(scheduleDir, "nightly.state")
	if state != "" {
		if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	record := filepath.Join(bin, "record.jsonl")
	// The stub checks the structured fields the history query relies on and
	// keeps only the MESSAGE line, as `journalctl -o cat` would show it.
	stub := "#!/bin/sh\n[ \"$1\" = --journald ] || exit 9\n" +
		"fields=$(cat)\n" +
		"printf '%s\\n' \"$fields\" | grep -q '^SYSLOG_IDENTIFIER=ob-run$' || exit 8\n" +
		"printf '%s\\n' \"$fields\" | grep -q '^ONEBOX_UNIT=ob-sample-nightly$' || exit 7\n" +
		"printf '%s\\n' \"$fields\" | grep -q '^ONEBOX_JOB=nightly$' || exit 6\n" +
		"printf '%s\\n' \"$fields\" | sed -n 's/^MESSAGE=//p' >>" + record + "\n"
	if err := os.WriteFile(filepath.Join(bin, "logger"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	sent := filepath.Join(bin, "curl.args")
	// Sends run in the background concurrently, so each stub call writes its
	// whole argument list in one printf, keeping calls from interleaving.
	curl := "#!/bin/sh\nprintf '%s\\n' \"$@\" -- >>" + sent + "\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(context.Background(), "sh", "-s")
	command.Stdin = strings.NewReader(script)
	command.Env = append([]string{"PATH=" + bin + ":" + os.Getenv("PATH")}, "HOME="+base)
	for k, v := range env {
		command.Env = append(command.Env, k+"="+v)
	}
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("notifier exited non-zero: %v\n%s\n%s", err, out, script)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("notifier wrote no record: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 1 {
		t.Fatalf("notifier wrote %d records, want 1:\n%s", len(lines), body)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("record is not JSON: %v\n%s", err, lines[0])
	}
	_, stateErr := os.Stat(statePath)
	var sends []string
	if args, err := os.ReadFile(sent); err == nil {
		sends = strings.Split(strings.TrimSpace(string(args)), "\n")
	}
	return decoded, stateErr == nil, sends
}

func TestScheduledJobNotifierRecordsEachOutcomeAndRemovesState(t *testing.T) {
	state := "release=20260905-140000-ab12cd\nstarted_at=2026-09-05T15:00:01Z\nstarted_epoch=1\ntrigger=timer\noperation=\nattempt=2\ninputs=\n"
	for name, tc := range map[string]struct {
		state   string
		env     map[string]string
		outcome string
		exit    any
		attempt float64
	}{
		"success":      {state, map[string]string{"SERVICE_RESULT": "success", "EXIT_STATUS": "0", "INVOCATION_ID": "a1b2"}, "success", float64(0), 2},
		"failure":      {state, map[string]string{"SERVICE_RESULT": "exit-code", "EXIT_STATUS": "1"}, "failure", float64(1), 2},
		"timeout":      {state, map[string]string{"SERVICE_RESULT": "timeout", "EXIT_STATUS": "TERM"}, "timeout", nil, 2},
		"skipped":      {"skipped=another run of this job is still in progress\noperation=\ninputs=\n", map[string]string{"SERVICE_RESULT": "success", "EXIT_STATUS": "0", "TRIGGER_UNIT": "ob-sample-nightly.timer"}, "skipped", float64(0), 0},
		"job exits 75": {state, map[string]string{"SERVICE_RESULT": "exit-code", "EXIT_STATUS": "75"}, "failure", float64(75), 2},
		"no state":     {"", map[string]string{"SERVICE_RESULT": "exit-code", "EXIT_STATUS": "3"}, "failure", float64(3), 0},
	} {
		t.Run(name, func(t *testing.T) {
			record, stateLeft, sends := runNotifier(t, app.ScheduledJob{Name: "nightly", Notify: []string{"failure", "timeout"}}, nil, tc.state, tc.env)
			if len(sends) != 0 {
				t.Fatalf("no webhook is configured, yet curl ran: %v", sends)
			}
			if record["outcome"] != tc.outcome || record["exit_status"] != tc.exit || record["attempts"] != tc.attempt {
				t.Fatalf("record = %#v", record)
			}
			if record["job"] != "nightly" || record["run"] != tc.env["INVOCATION_ID"] {
				t.Fatalf("identity fields wrong: %#v", record)
			}
			if tc.state == state && (record["release"] != "20260905-140000-ab12cd" || record["trigger"] != "timer" || record["duration_s"].(float64) < 1) {
				t.Fatalf("state fields not carried: %#v", record)
			}
			if name == "skipped" && (record["trigger"] != "timer" || record["reason"] != "another run of this job is still in progress") {
				t.Fatalf("skip was not recorded with its trigger and reason: %#v", record)
			}
			if stateLeft {
				t.Fatal("state file survived the notifier")
			}
		})
	}
}

func TestScheduleStatusPrefersTheRunRecordOverSystemdResult(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@journal
persistent
@@nightly:service
LoadState=loaded
ActiveState=inactive
Result=success
ExecMainStatus=75
@@nightly:timer
LoadState=loaded
ActiveState=active
NextElapseUSecRealtime=Sat 2026-09-06 02:00:00 UTC
@@nightly:run
@@nightly:history
{"run":"a1b2c3d4e5f60718293a4b5c6d7e8f90","job":"nightly","trigger":"timer","release":"r1","started_at":"2026-09-05T02:00:01Z","finished_at":"2026-09-05T02:00:02Z","duration_s":1,"attempts":0,"exit_status":75,"outcome":"skipped","inputs":{}}
{"run":"b2c3d4e5f60718293a4b5c6d7e8f9012","job":"nightly","trigger":"timer","release":"r1","started_at":"2026-09-04T02:00:01Z","finished_at":"2026-09-04T02:05:02Z","duration_s":301,"attempts":3,"exit_status":1,"outcome":"failure","inputs":{}}
{"run":"c3d4e5f60718293a4b5c6d7e8f901234","job":"nightly","trigger":"timer","release":"r1","started_at":"2026-09-03T02:00:01Z","finished_at":"2026-09-03T02:01:02Z","duration_s":61,"attempts":1,"exit_status":0,"outcome":"success","inputs":{}}
`}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := statuses[0]
	if got.LastOutcome != "skipped" || got.NextRun != "Sat 2026-09-06 02:00:00 UTC" || !got.JournalPersistent {
		t.Fatalf("record fields not surfaced: %#v", got)
	}
	// The newest record is a skip: it is not itself a failure, and it does not
	// clear the failure before it, which is still the job's standing verdict.
	if got.ConsecutiveFailures != 1 || !got.Diverged {
		t.Fatalf("a skip cleared the failure behind it: %#v", got)
	}
	if got.LastAttempts != 0 || got.LastDurationSeconds != 1 {
		t.Fatalf("last run detail not surfaced: %#v", got)
	}
}

func TestScheduleStatusCountsConsecutiveFailuresFromRecords(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@journal
volatile
@@nightly:service
LoadState=loaded
ActiveState=failed
Result=exit-code
ExecMainStatus=1
@@nightly:timer
LoadState=loaded
ActiveState=active
@@nightly:run
@@nightly:history
{"run":"a1b2c3d4e5f60718293a4b5c6d7e8f90","job":"nightly","trigger":"timer","started_at":"2026-09-05T02:00:01Z","finished_at":"2026-09-05T02:00:02Z","duration_s":1,"attempts":2,"exit_status":1,"outcome":"failure","inputs":{}}
{"run":"b2c3d4e5f60718293a4b5c6d7e8f9012","job":"nightly","trigger":"timer","started_at":"2026-09-04T02:00:01Z","finished_at":"2026-09-04T02:05:02Z","duration_s":301,"attempts":2,"exit_status":null,"outcome":"timeout","inputs":{}}
{"run":"c3d4e5f60718293a4b5c6d7e8f901234","job":"nightly","trigger":"timer","started_at":"2026-09-03T02:00:01Z","finished_at":"2026-09-03T02:01:02Z","duration_s":61,"attempts":1,"exit_status":0,"outcome":"success","inputs":{}}
`}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := statuses[0]
	if got.ConsecutiveFailures != 2 || !got.Diverged || got.JournalPersistent {
		t.Fatalf("failures not counted: %#v", got)
	}
	if !strings.Contains(strings.Join(got.Issues, "; "), "last run failed: failure (exit 1)") {
		t.Fatalf("issue does not name the outcome: %#v", got.Issues)
	}
}

func TestScheduledJobRunnerRetriesWithCappedDoublingBackoff(t *testing.T) {
	job := app.ScheduledJob{Name: "nightly", Timeout: "45m", DeployLock: "exclusive",
		RetryAttempts: 3, RetryBackoff: 30 * time.Second, RetryMaxBackoff: 10 * time.Minute}
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	for _, want := range []string{
		"max_attempts=3", "backoff=30", "max_backoff=600",
		"attempt=1", "while :; do", "write_state \"$attempt\"",
		"status=0", "|| status=$?", "[ \"$status\" -eq 0 ] && exit 0",
		"if [ \"$attempt\" -ge \"$max_attempts\" ]; then exit \"$status\"; fi",
		"sleep \"$backoff\"", "backoff=$((backoff * 2))", "attempt=$((attempt + 1))",
	} {
		if !strings.Contains(runner, want) {
			t.Errorf("runner is missing %q:\n%s", want, runner)
		}
	}
	single := scheduleRunnerScript("sample", app.ScheduledJob{Name: "nightly", Timeout: "1h", DeployLock: "exclusive", RetryAttempts: 1}, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	if strings.Contains(single, "max_attempts=") {
		t.Errorf("a single-attempt job must not carry a retry loop:\n%s", single)
	}
	pinned := scheduleRunnerScript("sample", app.ScheduledJob{Name: "nightly", Timeout: "1h", DeployLock: "pinned", RetryAttempts: 2, RetryBackoff: time.Second, RetryMaxBackoff: time.Minute}, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	if !strings.Contains(pinned, "max_attempts=") || strings.Index(pinned, "flock --unlock 8") > strings.Index(pinned, "max_attempts=") {
		t.Errorf("pinned runner must release the schedule mutex before its attempt loop:\n%s", pinned)
	}
	for _, script := range []string{runner, single, pinned} {
		command := exec.CommandContext(context.Background(), "sh", "-n")
		command.Stdin = strings.NewReader(script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("runner is not valid POSIX shell: %v: %s\n%s", err, output, script)
		}
	}
}

func TestScheduledJobNotifierSelectsBodiesByOutcome(t *testing.T) {
	cfg := testConfig()
	cfg.Notifications = map[string]app.Notification{
		"ops": {Webhook: "https://hooks.example.com/ops", On: []string{"success", "failure"}, Format: "json"},
	}
	f := &transport.Fake{TargetName: "root@example.internal"}
	e := New(cfg, testProject(t), f, Options{Environment: "production", Out: &bytes.Buffer{}, Sleep: noSleep})
	script, err := e.scheduleNotifier(app.ScheduledJob{Name: "nightly", Notify: []string{"success", "failure", "skipped"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`case " success failure skipped " in *" $outcome "*) ;; *) exit 0 ;; esac`,
		`"status":"ok"`,
		`"status":"fail"`,
		`if [ "$outcome" = success ]; then`,
		`"deploy_id":"'"${INVOCATION_ID:-}"'"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("notifier is missing %q:\n%s", want, script)
		}
	}
	command := exec.CommandContext(context.Background(), "sh", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("notifier is not valid POSIX shell: %v: %s\n%s", err, output, script)
	}
}

func TestScheduledJobNotifierSendsOnlySelectedOutcomesWithTheRunID(t *testing.T) {
	state := "release=r1\nstarted_at=2026-09-05T15:00:01Z\nstarted_epoch=1\ntrigger=timer\noperation=\nattempt=2\ninputs=\n"
	webhooks := map[string]app.Notification{
		"ops":  {Webhook: "https://hooks.example.com/ops", On: []string{"success", "failure"}, Format: "json"},
		"chat": {Webhook: "https://hooks.example.com/chat", On: []string{"failure"}, Format: "text"},
	}
	for name, tc := range map[string]struct {
		notify []string
		env    map[string]string
		want   int
		status string
	}{
		"failure selected":     {[]string{"failure", "timeout"}, map[string]string{"SERVICE_RESULT": "exit-code", "EXIT_STATUS": "1", "INVOCATION_ID": "abc123"}, 2, "fail"},
		"success not selected": {[]string{"failure", "timeout"}, map[string]string{"SERVICE_RESULT": "success", "EXIT_STATUS": "0", "INVOCATION_ID": "abc123"}, 0, ""},
		"success selected":     {[]string{"success"}, map[string]string{"SERVICE_RESULT": "success", "EXIT_STATUS": "0", "INVOCATION_ID": "abc123"}, 1, "ok"},
		"skipped selected":     {[]string{"skipped"}, map[string]string{"SERVICE_RESULT": "success", "EXIT_STATUS": "0", "INVOCATION_ID": "abc123"}, 2, "fail"},
		"timeout not selected": {[]string{"failure"}, map[string]string{"SERVICE_RESULT": "timeout", "EXIT_STATUS": "TERM", "INVOCATION_ID": "abc123"}, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			runState := state
			if name == "skipped selected" {
				runState = "skipped=an application operation holds the deploy lock\noperation=\ninputs=\n"
			}
			_, _, sends := runNotifier(t, app.ScheduledJob{Name: "nightly", Notify: tc.notify}, webhooks, runState, tc.env)
			calls := 0
			for _, arg := range sends {
				if arg == "--" {
					calls++
				}
			}
			if calls != tc.want {
				t.Fatalf("curl ran %d time(s), want %d:\n%s", calls, tc.want, strings.Join(sends, "\n"))
			}
			if tc.want == 0 {
				return
			}
			joined := strings.Join(sends, "\n")
			var jsonBody string
			for _, arg := range sends {
				if strings.HasPrefix(arg, "{") {
					jsonBody = arg
				}
			}
			if jsonBody == "" {
				t.Fatalf("no JSON body was sent:\n%s", joined)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(jsonBody), &body); err != nil {
				t.Fatalf("sent body is not JSON: %v\n%s", err, jsonBody)
			}
			if body["status"] != tc.status || body["deploy_id"] != "abc123" || body["verb"] != "scheduled job nightly" {
				t.Fatalf("body = %#v", body)
			}
			if ts, _ := body["ts"].(string); !strings.HasSuffix(ts, "Z") || strings.Contains(ts, "ONEBOX") {
				t.Fatalf("timestamp was not filled at send time: %#v", body)
			}
			if strings.Contains(joined, "ONEBOX_SCHEDULE") {
				t.Fatalf("a placeholder leaked into a send:\n%s", joined)
			}
		})
	}
}

func TestScheduledJobRunnerConsumesManualInputsWithoutShellInterpolation(t *testing.T) {
	job := app.ScheduledJob{Name: "sync", Timeout: "45m", DeployLock: "pinned", RetryAttempts: 1,
		Inputs: map[string]app.JobInput{"SOURCE": {Enum: []string{"catalog"}, Default: "catalog"}}}
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	for _, want := range []string{
		"inputs_file='/var/lib/ob/sample/schedule/sync.inputs'",
		`if [ -z "${TRIGGER_UNIT:-}" ] && [ -f "$inputs_file" ]; then`,
		`while IFS= read -r line || [ -n "$line" ]; do`,
		`ONEBOX_OPERATION=*) operation=${line#ONEBOX_OPERATION=} ;;`,
		`[A-Z]*=*) set -- "$@" -e "$line"`,
		`rm -f "$inputs_file"`,
		`run --rm --no-deps "$@" --name 'sample-sync-1' 'sync'`,
	} {
		if !strings.Contains(runner, want) {
			t.Errorf("runner is missing %q:\n%s", want, runner)
		}
	}
	if strings.Contains(runner, ". \"$inputs_file\"") || strings.Contains(runner, "eval") {
		t.Fatalf("runner evaluates the inputs file as shell:\n%s", runner)
	}
	command := exec.CommandContext(context.Background(), "sh", "-n")
	command.Stdin = strings.NewReader(runner)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("runner is not valid POSIX shell: %v: %s\n%s", err, output, runner)
	}
	// The consume block precedes the locks so a skipped manual run cannot
	// leave its inputs for the next timer firing.
	if strings.Index(runner, "inputs_file=") > strings.Index(runner, "flock --exclusive --nonblock 9") {
		t.Fatalf("inputs are consumed after the lock:\n%s", runner)
	}
	exclusive := scheduleRunnerScript("sample", app.ScheduledJob{Name: "sync", Timeout: "1h", DeployLock: "exclusive", RetryAttempts: 1}, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	if !strings.Contains(exclusive, "inputs_file=") || !strings.Contains(exclusive, `run --rm --no-deps "$@" --name`) {
		t.Fatalf("exclusive runner does not consume inputs:\n%s", exclusive)
	}
}

func TestSyncSchedulesRequireSystemd252ForEveryScheduledJob(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Inputs:   map[string]app.JobInput{"SOURCE": {Enum: []string{"a"}, Default: "a"}},
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	for version, wantErr := range map[string]bool{"systemd 249 (249.11-0ubuntu3)\n": true, "systemd 255 (255.4-1ubuntu8)\n": false, "": true} {
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
			case strings.Contains(cmd, "systemctl --version"):
				return transport.Result{Stdout: version}, true
			}
			return base(cmd)
		}
		e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
		err := e.SyncSchedules(context.Background())
		if wantErr && (err == nil || !strings.Contains(err.Error(), "systemd 252")) {
			t.Fatalf("version %q was accepted for a scheduled job: %v", version, err)
		}
		if !wantErr && err != nil {
			t.Fatalf("version %q was refused: %v", version, err)
		}
	}
}

// The inputs block is the one place operator text meets the runner, so it is
// executed rather than only inspected: values with spaces and equals signs
// must arrive as single -e arguments, the metadata line must never become an
// argument, and the file must be gone afterwards.
func TestScheduleInputsLinesParseTheFileIntoArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	dir := t.TempDir()
	inputs := filepath.Join(dir, "sync.inputs")
	body := "ONEBOX_OPERATION=20260905-151200-schedule_run-7c1e\nSOURCE=prices and more\nSINCE=2026-09-01=ish\n"
	if err := os.WriteFile(inputs, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.Join(append(append([]string{"#!/bin/sh", "set -eu"}, scheduleInputsLines(inputs)...),
		`printf 'argc=%s\n' "$#"`,
		`for arg in "$@"; do printf 'arg=%s\n' "$arg"; done`,
		`printf 'operation=%s\n' "$operation"`,
		`printf 'json=%s\n' "$inputs_json"`,
	), "\n")
	for _, trigger := range []string{"", "ob-sample-sync.timer"} {
		command := exec.CommandContext(context.Background(), "sh", "-s")
		command.Stdin = strings.NewReader(script)
		command.Env = []string{"PATH=" + os.Getenv("PATH")}
		if trigger != "" {
			command.Env = append(command.Env, "TRIGGER_UNIT="+trigger)
		}
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("trigger %q: %v\n%s", trigger, err, out)
		}
		got := string(out)
		if trigger != "" {
			if !strings.Contains(got, "argc=0\n") || !strings.Contains(got, "operation=\n") {
				t.Fatalf("a timer activation read the inputs file:\n%s", got)
			}
			if _, err := os.Stat(inputs); err != nil {
				t.Fatalf("a timer activation removed the inputs file: %v", err)
			}
			continue
		}
		for _, want := range []string{
			"argc=4\n", "arg=-e\narg=SOURCE=prices and more\n", "arg=-e\narg=SINCE=2026-09-01=ish\n",
			"operation=20260905-151200-schedule_run-7c1e\n",
			`json="SOURCE":"prices and more","SINCE":"2026-09-01=ish"` + "\n",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("manual activation output is missing %q:\n%s", want, got)
			}
		}
		if _, err := os.Stat(inputs); err == nil {
			t.Fatal("the inputs file survived a manual activation")
		}
		if err := os.WriteFile(inputs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScheduleStatusRaisesAnIssueForASkipStreak(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	skip := `{"run":"a1b2c3d4e5f60718293a4b5c6d7e8f90","job":"nightly","trigger":"timer","started_at":"2026-09-05T02:00:01Z","finished_at":"2026-09-05T02:00:01Z","duration_s":0,"attempts":0,"exit_status":0,"outcome":"skipped","reason":"an application operation holds the deploy lock","inputs":{}}`
	success := `{"run":"c3d4e5f60718293a4b5c6d7e8f901234","job":"nightly","trigger":"timer","started_at":"2026-09-03T02:00:01Z","finished_at":"2026-09-03T02:01:02Z","duration_s":61,"attempts":1,"exit_status":0,"outcome":"success","inputs":{}}`
	for name, tc := range map[string]struct {
		history string
		skips   int
		issue   bool
	}{
		"two skips":   {skip + "\n" + skip + "\n" + success, 2, false},
		"three skips": {skip + "\n" + skip + "\n" + skip + "\n" + success, 3, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, "systemctl show") {
					return transport.Result{Stdout: "@@journal\npersistent\n@@nightly:service\nLoadState=loaded\nActiveState=inactive\n@@nightly:timer\nLoadState=loaded\nActiveState=active\n@@nightly:run\n@@nightly:history\n" + tc.history + "\n"}, true
				}
				return transport.Result{}, false
			}}
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			statuses, err := e.scheduleStatuses(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := statuses[0]
			if got.ConsecutiveSkips != tc.skips || got.LastOutcome != "skipped" || got.LastReason == "" {
				t.Fatalf("skips not counted: %#v", got)
			}
			if got.Diverged != tc.issue {
				t.Fatalf("issue = %v, want %v: %#v", got.Diverged, tc.issue, got.Issues)
			}
			if tc.issue && !strings.Contains(strings.Join(got.Issues, "; "), "skipped 3 firings in a row: an application operation holds the deploy lock") {
				t.Fatalf("issue does not name the streak and reason: %#v", got.Issues)
			}
		})
	}
}

// A skip is news about timing. It must not clear a failure that nothing has
// fixed: the newest record that actually ran is the standing verdict.
func TestScheduleStatusKeepsAFailureVisibleBehindASkip(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	history := `{"run":"a1b2c3d4e5f60718293a4b5c6d7e8f90","job":"nightly","trigger":"timer","started_at":"2026-09-05T02:00:01Z","finished_at":"2026-09-05T02:00:01Z","duration_s":0,"attempts":0,"exit_status":0,"outcome":"skipped","reason":"an application operation holds the deploy lock","inputs":{}}
{"run":"b2c3d4e5f60718293a4b5c6d7e8f9012","job":"nightly","trigger":"timer","started_at":"2026-09-04T02:00:01Z","finished_at":"2026-09-04T02:05:02Z","duration_s":301,"attempts":2,"exit_status":9,"outcome":"failure","inputs":{}}
{"run":"c3d4e5f60718293a4b5c6d7e8f901234","job":"nightly","trigger":"timer","started_at":"2026-09-03T02:00:01Z","finished_at":"2026-09-03T02:01:02Z","duration_s":61,"attempts":1,"exit_status":0,"outcome":"success","inputs":{}}`
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: "@@journal\npersistent\n@@nightly:service\nLoadState=loaded\nActiveState=inactive\n@@nightly:timer\nLoadState=loaded\nActiveState=active\n@@nightly:run\n@@nightly:history\n" + history + "\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := statuses[0]
	if !got.Diverged || got.ConsecutiveFailures != 1 || got.LastOutcome != "skipped" {
		t.Fatalf("a skip cleared the failure behind it: %#v", got)
	}
	if issues := strings.Join(got.Issues, "; "); !strings.Contains(issues, "last run failed: failure (exit 9)") ||
		!strings.Contains(issues, "nothing has run since") {
		t.Fatalf("issue does not name the failure the skip hid: %#v", got.Issues)
	}
}

// An unreadable journal costs status its records, not the whole report.
func TestScheduleStatusDegradesWhenTheJournalCannotBeRead(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			if !strings.Contains(cmd, "2>/dev/null || true") {
				return transport.Result{ExitCode: 1, Stderr: "Failed to open journal"}, true
			}
			return transport.Result{Stdout: "@@journal\npersistent\n@@nightly:service\nLoadState=loaded\nActiveState=inactive\n@@nightly:timer\nLoadState=loaded\nActiveState=active\n@@nightly:run\n@@nightly:history\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatalf("an unreadable journal broke the whole status read: %v", err)
	}
	if got := statuses[0]; got.LastOutcome != "" || got.Diverged {
		t.Fatalf("missing records must read as unknown, not as failure: %#v", got)
	}
}

// A firing that cannot take the job lock must leave the running job's state
// alone: that file is the evidence its own notifier turns into the record.
func TestScheduledJobRunnerDoesNotClobberARunningJobsState(t *testing.T) {
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	job := app.ScheduledJob{Name: "nightly", Timeout: "1h", DeployLock: "exclusive", RetryAttempts: 1}
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	if !strings.Contains(runner, `stand_aside() { echo "onebox: skipped: $1" >&2; exit 0; }`) {
		t.Fatalf("runner has no lock-less skip:\n%s", runner)
	}
	if !strings.Contains(runner, "flock --exclusive --nonblock 9 || stand_aside 'another run of this job is still in progress'") {
		t.Fatalf("a job-lock conflict still writes state:\n%s", runner)
	}
	// The other two skips hold the job lock, so the state is theirs to write.
	for _, want := range []string{
		"flock --exclusive --nonblock 8 || skip 'an application operation is taking its lock'",
		"skip 'an application operation holds the deploy lock'",
	} {
		if !strings.Contains(runner, want) {
			t.Fatalf("runner is missing %q:\n%s", want, runner)
		}
	}
}

// The container name is fixed, so a corpse from one attempt would fail every
// attempt after it.
func TestScheduledJobRunnerClearsTheContainerBetweenAttempts(t *testing.T) {
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	job := app.ScheduledJob{Name: "nightly", Timeout: "45m", DeployLock: "exclusive",
		RetryAttempts: 3, RetryBackoff: time.Second, RetryMaxBackoff: time.Minute}
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	loop := runner[strings.Index(runner, "while :; do"):]
	if !strings.Contains(loop, "docker rm -f 'sample-nightly-1'") {
		t.Fatalf("no cleanup inside the attempt loop:\n%s", loop)
	}
}

// On a systemd without TRIGGER_UNIT the runner cannot see the trigger, and
// says so rather than calling every timer firing an operator's run.
func TestScheduledJobRunnerRecordsAnUnknownTriggerOnAnOlderSystemd(t *testing.T) {
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	job := app.ScheduledJob{Name: "nightly", Timeout: "1h", DeployLock: "exclusive", RetryAttempts: 1}
	modern := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, true)
	older := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil, 10*time.Minute, false)
	if !strings.Contains(modern, "else trigger=manual; fi") {
		t.Fatalf("a host that sets TRIGGER_UNIT must name the operator:\n%s", modern)
	}
	if !strings.Contains(older, "else trigger=unknown; fi") {
		t.Fatalf("a host without TRIGGER_UNIT must not invent a trigger:\n%s", older)
	}
}

// The systemd floor is scoped to what actually needs it. A project that has
// been running scheduled jobs on an older LTS keeps running them.
func TestSyncSchedulesRequiresSystemd252OnlyForInputs(t *testing.T) {
	for name, tc := range map[string]struct {
		inputs  map[string]app.JobInput
		wantErr bool
	}{
		"plain job":       {nil, false},
		"declares inputs": {map[string]app.JobInput{"SOURCE": {Enum: []string{"a"}, Default: "a"}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Workloads["sync"] = app.Workload{
				Role: app.RoleJob, When: "manual", DataEffect: "none", Inputs: tc.inputs,
				Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
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
				case strings.Contains(cmd, "systemctl --version"):
					return transport.Result{Stdout: "systemd 249 (249.11-0ubuntu3)\n"}, true
				}
				return base(cmd)
			}
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			err := e.SyncSchedules(context.Background())
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "systemd 252") {
					t.Fatalf("inputs were accepted on systemd 249: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a job without inputs was refused on systemd 249: %v", err)
			}
			if artifacts := strings.Join(f.Inputs, "\n"); !strings.Contains(artifacts, "else trigger=unknown; fi") {
				t.Fatalf("the runner claims a trigger the host cannot report:\n%s", artifacts)
			}
		})
	}
}
