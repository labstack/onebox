package engine

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestScheduledJobsNeedNoPythonRunner(t *testing.T) {
	f := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "command -v flock") {
			return transport.Result{Stdout: "ok\n"}, true
		}
		if strings.Contains(command, "python") {
			t.Fatalf("scheduled job probes Python: %s", command)
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}})
	if err := e.requireScheduleHost(t.Context(), []app.ScheduledJob{{Name: "refresh"}}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyJobExecutionsBlockScheduleReplacementAndJobStart(t *testing.T) {
	for _, action := range []string{"apply", "preflight without jobs", "manual run", "migration"} {
		t.Run(action, func(t *testing.T) {
			f := happyFake()
			base := f.Dynamic
			f.Dynamic = func(command string) (transport.Result, bool) {
				if strings.Contains(command, "/schedule/executions") {
					return transport.Result{ExitCode: 78}, true
				}
				return base(command)
			}
			cfg := testConfig()
			cfg.Workloads["refresh"] = app.Workload{Role: app.RoleJob, DataEffect: app.DataEffectNone,
				Schedule: &app.JobSchedule{Cron: "0 * * * *", Timeout: "1h"}}
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			var err error
			switch action {
			case "apply":
				err = e.SyncSchedules(t.Context())
			case "preflight without jobs":
				err = e.requireScheduleHost(t.Context(), nil)
			case "manual run":
				_, err = e.ScheduleRun(t.Context(), "op-1", "refresh", nil, false)
			case "migration":
				_, _, err = e.runOneJob(t.Context(), "op-1", 1, "migrate", "/release", "/release/compose.yaml")
			}
			if err == nil || !strings.Contains(err.Error(), "archive") {
				t.Fatalf("legacy checkpoint ignored: %v", err)
			}
			if len(f.Inputs) > 0 || len(f.Uploads) > 0 {
				t.Fatalf("legacy state allowed schedule/request writes: %+v", f)
			}
			for _, command := range f.Commands {
				if strings.Contains(command, "systemctl start") || strings.Contains(command, "ONEBOX_RESULT_FILE") {
					t.Fatalf("job started despite legacy checkpoint: %s", command)
				}
			}
		})
	}
}

func TestOldExecutionMetadataDoesNotHideJobHistory(t *testing.T) {
	record := `{"run":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","job":"nightly","outcome":"failure","execution":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`
	if got := parseScheduleRunRecords(record, "nightly"); len(got) != 1 || got[0].Outcome != "failure" {
		t.Fatalf("old run record lost after workflow removal: %+v", got)
	}
}

func TestScheduleHostRejectsIncompatibleFlockBeforeInstall(t *testing.T) {
	f := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "command -v flock") {
			for _, option := range []string{"--conflict-exit-code", "--exclusive", "--nonblock", "--shared", "--timeout", "--unlock"} {
				if !strings.Contains(command, option) {
					t.Fatalf("flock compatibility probe does not require %s: %s", option, command)
				}
			}
			return transport.Result{ExitCode: 1}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}})
	err := e.requireScheduleHost(context.Background(), []app.ScheduledJob{{Name: "refresh"}})
	if err == nil || !strings.Contains(err.Error(), "compatible util-linux flock") {
		t.Fatalf("incompatible flock accepted: %v", err)
	}
}

func TestScheduleFlockProbeExecutesCapabilityChecks(t *testing.T) {
	root := t.TempDir()
	flock := filepath.Join(root, "flock")
	writeHelp := func(options string) {
		t.Helper()
		if err := os.WriteFile(flock, []byte("#!/bin/sh\nprintf '%s\\n' "+q(options)+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func() ([]byte, error) {
		command := exec.CommandContext(t.Context(), "sh", "-c", scheduleFlockProbe(flock))
		command.Env = append(os.Environ(), "PATH="+root+":/usr/bin:/bin")
		return command.CombinedOutput()
	}

	writeHelp("--conflict-exit-code --exclusive --nonblock --shared --timeout --unlock")
	if output, err := run(); err != nil || string(output) != "ok\n" {
		t.Fatalf("compatible flock rejected: err=%v output=%q", err, output)
	}
	writeHelp("--conflict-exit-code --exclusive --nonblock --shared --timeout")
	if output, err := run(); err == nil || strings.Contains(string(output), "ok") {
		t.Fatalf("flock missing --unlock accepted: err=%v output=%q", err, output)
	}
}
