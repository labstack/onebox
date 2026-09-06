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

func TestRunJournalIncludesExecutionOnlyWhenSet(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", 32)} {
		t.Run("execution="+id, func(t *testing.T) {
			record, _, _ := runNotifier(t, app.ScheduledJob{Name: "nightly"}, nil,
				"attempt=1\nexecution="+id+"\n", nil)
			value, present := record["execution"]
			if id == "" && present {
				t.Fatalf("ordinary run contains execution field: %v", record)
			}
			if id != "" && value != id {
				t.Fatalf("durable run execution = %v, want %s", value, id)
			}
		})
	}
}

func TestDurableRunnerPublishesCheckpointBeforeReleasingRetentionRendezvous(t *testing.T) {
	e := New(testConfig(), testProject(t), &transport.Fake{}, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	runner, err := e.durableScheduleRunner(app.ScheduledJob{Name: "refresh", Timeout: "1h", DeployLock: "pinned", Execution: &app.JobExecution{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease := strings.Index(runner, "/usr/bin/flock --shared 7")
	prepare := strings.Index(runner, " prepare ")
	unlock := strings.Index(runner, "/usr/bin/flock --unlock 8")
	run := strings.LastIndex(runner, " run ")
	if lease < 0 || prepare <= lease || unlock <= prepare || run <= unlock {
		t.Fatalf("expected live lease, checkpoint, mutex unlock, then execution:\n%s", runner)
	}
}

func TestDurableCleanupCannotRemoveAnotherInvocationsContainer(t *testing.T) {
	for _, tc := range []struct {
		name, label, invocation string
		remove                  bool
	}{
		{"owned", "current", "current", true},
		{"other activation", "previous", "current", false},
		{"unlabelled", "", "current", false},
		{"missing invocation", "current", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			removed := filepath.Join(dir, "removed")
			stub := filepath.Join(dir, "docker")
			body := "#!/bin/sh\ncase \"$1\" in\ninspect) printf '%s\\n' \"$TEST_LABEL\";;\nrm) touch " + q(removed) + ";;\n*) exit 2;;\nesac\n"
			if err := os.WriteFile(stub, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "sh", "-c", strings.ReplaceAll(durableContainerCleanup("job"), "/usr/bin/docker", q(stub)))
			command.Env = append(os.Environ(), "INVOCATION_ID="+tc.invocation, "TEST_LABEL="+tc.label)
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("cleanup: %v: %s", err, out)
			}
			_, err := os.Stat(removed)
			if (err == nil) != tc.remove {
				t.Fatalf("removed=%t, want %t", err == nil, tc.remove)
			}
		})
	}
}

func TestDurablePythonRequirementDoesNotAffectOrdinarySchedules(t *testing.T) {
	for _, durable := range []bool{false, true} {
		f := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
			switch {
			case strings.Contains(command, "command -v flock"):
				return transport.Result{Stdout: "ok\n"}, true
			case strings.Contains(command, "/usr/bin/python3"):
				return transport.Result{ExitCode: 127}, true
			default:
				return transport.Result{}, false
			}
		}}
		e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}})
		job := app.ScheduledJob{Name: "refresh"}
		if durable {
			job.Execution = &app.JobExecution{}
		}
		err := e.requireScheduleHost(context.Background(), []app.ScheduledJob{job})
		if durable && (err == nil || !strings.Contains(err.Error(), "Python 3.8")) {
			t.Fatalf("missing Python accepted: %v", err)
		}
		if !durable && err != nil {
			t.Fatalf("ordinary schedule refused: %v", err)
		}
		if !durable && strings.Contains(strings.Join(f.Commands, "\n"), "python3") {
			t.Fatal("ordinary schedule probes Python")
		}
	}
}

func TestDurableResumeRefusesLegacyRunnerBeforePublishingRequest(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["refresh"] = app.Workload{Role: app.RoleJob, When: "manual", DataEffect: app.DataEffectNone,
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"}, Execution: &app.JobExecution{}}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(command, "systemctl is-active"):
			return transport.Result{Stdout: "inactive\n"}, true
		case strings.Contains(command, "Durable execution protocol v1."):
			return transport.Result{ExitCode: 1}, true
		}
		return base(command)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	_, err := e.scheduleRun(context.Background(), "resume-op", "refresh", nil, false, strings.Repeat("a", 32))
	if err == nil || !strings.Contains(err.Error(), "ob schedule apply") {
		t.Fatalf("legacy runner accepted: %v", err)
	}
	for _, command := range f.Commands {
		if strings.Contains(command, "refresh.inputs") || strings.Contains(command, "systemctl start") {
			t.Fatalf("resume published before runner qualification: %s", command)
		}
	}
}

func TestDurableCompatibilityInvalidationMustSucceedBeforeDataChangingJob(t *testing.T) {
	for _, effect := range []app.DataEffect{app.DataEffectMigration, app.DataEffectDestructive, app.DataEffectUnknown} {
		t.Run(string(effect), func(t *testing.T) {
			cfg := testConfig()
			cfg.Workloads["change"] = app.Workload{Role: app.RoleJob, DataEffect: effect}
			f := happyFake()
			base := f.Dynamic
			invalidated := false
			f.Dynamic = func(command string) (transport.Result, bool) {
				if strings.Contains(command, " invalidate ") {
					invalidated = true
					return transport.Result{ExitCode: 1, Stderr: "checkpoint store is read-only"}, true
				}
				return base(command)
			}
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			_, _, err := e.runOneJob(context.Background(), "change", "/release", "/release/compose.yaml")
			if !invalidated || err == nil || !strings.Contains(err.Error(), "invalidate durable execution compatibility") {
				t.Fatalf("failed invalidation did not stop data-changing job: invalidated=%t err=%v", invalidated, err)
			}
			if strings.Contains(strings.Join(f.Commands, "\n"), "ONEBOX_RESULT_FILE") {
				t.Fatal("job started before compatibility invalidation succeeded")
			}
		})
	}
}
