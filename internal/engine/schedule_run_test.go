package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestScheduleRunWritesInputsJournalsThenStartsAfterReleasingTheLock(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Inputs:   map[string]app.JobInput{"SOURCE": {Enum: []string{"catalog", "prices"}, Default: "catalog"}},
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "systemctl is-active"):
			return transport.Result{Stdout: "inactive\n"}, true
		case strings.Contains(cmd, "systemctl start"):
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	result, err := e.ScheduleRun(context.Background(), "20260905-151200-schedule_run-7c1e", "sync", map[string]string{"SOURCE": "prices"}, false)
	if err != nil {
		t.Fatalf("schedule run: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	if !result.Started || result.Unit != "ob-sample-sync" || result.Inputs["SOURCE"] != "prices" || result.Operation != "20260905-151200-schedule_run-7c1e" {
		t.Fatalf("result = %#v", result)
	}
	seq := strings.Join(f.Commands, "\n")
	inputs := strings.Index(seq, "sync.inputs")
	start := strings.Index(seq, "systemctl start --no-block 'ob-sample-sync.service'")
	release := strings.LastIndex(seq, "rm -f '/var/lib/ob/sample/lock'")
	if inputs < 0 || start < 0 || release < 0 || !(inputs < release && release < start) {
		t.Fatalf("expected inputs write, lock release, then start:\n%s", seq)
	}
	if !strings.Contains(seq, "set -C") {
		t.Fatalf("inputs file was not created with noclobber:\n%s", seq)
	}
	if written := strings.Join(f.Inputs, "\n"); !strings.Contains(written, "ONEBOX_OPERATION=20260905-151200-schedule_run-7c1e\nSOURCE=prices\n") {
		t.Fatalf("inputs file content is wrong:\n%s", written)
	}
	for _, want := range []string{
		`"phase":"schedule-run","event":"start"`,
		`"phase":"schedule-run","event":"finish","status":"ok"`,
		`"target":"sync"`,
		`inputs: SOURCE=prices`,
	} {
		if !strings.Contains(seq, want) {
			t.Fatalf("journal is missing %q:\n%s", want, seq)
		}
	}
	// The start is claimed under the lock; the finish records how the request
	// ended, so it comes after the unit was actually started. A journal is per
	// operation id, so no other operation appends to this file meanwhile.
	if started := strings.Index(seq, `"phase":"schedule-run","event":"start"`); started < 0 || started > release {
		t.Fatalf("journal start was not written under the lock:\n%s", seq)
	}
	if finish := strings.LastIndex(seq, `"phase":"schedule-run","event":"finish","status":"ok"`); finish < start {
		t.Fatalf("journal finish was written before the unit was started:\n%s", seq)
	}
}

func TestScheduleRunRefusals(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Inputs:   map[string]app.JobInput{"SOURCE": {Enum: []string{"catalog"}, Default: "catalog"}},
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	cfg.Workloads["prune"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "destructive",
		Schedule: &app.JobSchedule{Cron: "0 3 * * *", Timezone: "UTC", Timeout: "1h"},
	}
	active := false
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "command -v flock") {
			return transport.Result{Stdout: "ok\n"}, true
		}
		if strings.Contains(cmd, "systemctl is-active") {
			if active {
				return transport.Result{Stdout: "activating\n"}, true
			}
			return transport.Result{Stdout: "inactive\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	ctx := context.Background()
	if _, err := e.ScheduleRun(ctx, "op", "prune", nil, false); err == nil || !strings.Contains(err.Error(), "ob job run") {
		t.Fatalf("destructive job accepted: %v", err)
	}
	if _, err := e.ScheduleRun(ctx, "op", "sync", map[string]string{"SOURCE": "reviews"}, false); err == nil {
		t.Fatal("undeclared value accepted")
	}
	if _, err := e.ScheduleRun(ctx, "op", "web", nil, false); err == nil {
		t.Fatal("non-scheduled workload accepted")
	}
	for _, command := range f.Commands {
		if strings.Contains(command, "systemctl start") || strings.Contains(command, ".inputs") {
			t.Fatalf("a refused run reached the host: %s", command)
		}
	}
	active = true
	if _, err := e.ScheduleRun(ctx, "op", "sync", nil, false); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("active unit not refused: %v", err)
	}
}

func TestScheduleRunWaitReportsTheRecordAndFailsOnAnyOtherOutcome(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	for outcome, wantErr := range map[string]bool{"success": false, "skipped": true, "failure": true} {
		f := happyFake()
		base := f.Dynamic
		f.Dynamic = func(cmd string) (transport.Result, bool) {
			switch {
			case strings.Contains(cmd, "command -v flock"):
				return transport.Result{Stdout: "ok\n"}, true
			case strings.Contains(cmd, "systemctl is-active"):
				return transport.Result{Stdout: "inactive\n"}, true
			case strings.Contains(cmd, "systemctl start 'ob-sample-sync.service'"):
				return transport.Result{}, true
			case strings.Contains(cmd, "SYSLOG_IDENTIFIER=ob-run"):
				// Newest first: a stale record from an earlier run precedes ours,
				// and must not be mistaken for it.
				return transport.Result{Stdout: `{"run":"ffffffffffffffffffffffffffffffff","job":"sync","trigger":"timer","operation":"","started_at":"2026-09-05T14:00:01Z","finished_at":"2026-09-05T14:00:02Z","duration_s":1,"attempts":1,"exit_status":0,"outcome":"success","inputs":{}}` + "\n" +
					`{"run":"a1b2c3d4e5f60718293a4b5c6d7e8f90","job":"sync","trigger":"manual","operation":"op-` + outcome + `","started_at":"2026-09-05T15:00:01Z","finished_at":"2026-09-05T15:00:02Z","duration_s":1,"attempts":1,"exit_status":0,"outcome":"` + outcome + `","inputs":{}}` + "\n"}, true
			}
			return base(cmd)
		}
		e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
		result, err := e.ScheduleRun(context.Background(), "op-"+outcome, "sync", nil, true)
		if result.Record == nil || result.Record.Outcome != outcome || result.Record.Run != "a1b2c3d4e5f60718293a4b5c6d7e8f90" {
			t.Fatalf("%s: this operation's record not returned: %#v", outcome, result)
		}
		if (err != nil) != wantErr {
			t.Fatalf("%s: err = %v, wantErr %v", outcome, err, wantErr)
		}
		if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "--no-block") {
			t.Fatalf("%s: --wait must block on systemctl start:\n%s", outcome, seq)
		}
	}
}

func TestScheduleRunDiscardsItsInputsWhenTheStartFails(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Inputs:   map[string]app.JobInput{"SOURCE": {Enum: []string{"catalog", "prices"}, Default: "catalog"}},
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "systemctl is-active"):
			return transport.Result{Stdout: "inactive\n"}, true
		case strings.Contains(cmd, "systemctl start"):
			return transport.Result{ExitCode: 5, Stderr: "Unit ob-sample-sync.service not found."}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	_, err := e.ScheduleRun(context.Background(), "op-1", "sync", map[string]string{"SOURCE": "prices"}, false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("start failure was not reported: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "rm -f '/var/lib/ob/sample/schedule/sync.inputs'") {
		t.Fatalf("a failed start left the inputs file pending:\n%s", seq)
	}
}

func TestScheduleRunTellsAPendingFileFromAWriteFailure(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	for name, tc := range map[string]struct {
		exit   int
		stderr string
		want   string
		reject string
	}{
		"pending":   {73, "", "already pending", "cannot write"},
		"read-only": {1, "cat: cannot create: Read-only file system", "Read-only file system", "already pending"},
	} {
		t.Run(name, func(t *testing.T) {
			f := happyFake()
			base := f.Dynamic
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch {
				case strings.Contains(cmd, "command -v flock"):
					return transport.Result{Stdout: "ok\n"}, true
				case strings.Contains(cmd, "systemctl is-active"):
					return transport.Result{Stdout: "inactive\n"}, true
				case strings.Contains(cmd, "sync.inputs"):
					return transport.Result{ExitCode: tc.exit, Stderr: tc.stderr}, true
				}
				return base(cmd)
			}
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			_, err := e.ScheduleRun(context.Background(), "op", "sync", nil, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.reject) {
				t.Fatalf("err = %v, want %q and not %q", err, tc.want, tc.reject)
			}
			for _, command := range f.Commands {
				if strings.Contains(command, "systemctl start") {
					t.Fatalf("a failed write still started the unit: %s", command)
				}
			}
		})
	}
}

// Without TRIGGER_UNIT the next timer firing would read the file this run
// left, so the manual path is refused rather than being made ambiguous.
func TestScheduleRunRefusesAHostThatCannotTellTheTriggerApart(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "systemctl is-active"):
			return transport.Result{Stdout: "inactive\n"}, true
		case strings.Contains(cmd, "systemctl --version"):
			return transport.Result{Stdout: "systemd 249 (249.11-0ubuntu3)\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	_, err := e.ScheduleRun(context.Background(), "op", "sync", nil, false)
	if err == nil || !strings.Contains(err.Error(), "systemd 252") {
		t.Fatalf("err = %v, want a refusal naming the requirement", err)
	}
	for _, command := range f.Commands {
		if strings.Contains(command, ".inputs") || strings.Contains(command, "systemctl start") {
			t.Fatalf("the refused run still touched the host: %s", command)
		}
	}
}

// The journal has to be able to say the request failed. Writing the finish
// as ok before the unit is even started left ob audit reporting "started" for
// a run that never began.
func TestScheduleRunJournalsAFailedRequestAsFailed(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "systemctl --version"):
			return transport.Result{Stdout: "systemd 255 (255.4-1ubuntu8)\n"}, true
		case strings.Contains(cmd, "systemctl is-active"):
			return transport.Result{Stdout: "inactive\n"}, true
		case strings.Contains(cmd, "systemctl start"):
			return transport.Result{ExitCode: 5, Stderr: "Unit ob-sample-sync.service not found."}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if _, err := e.ScheduleRun(context.Background(), "op-1", "sync", nil, false); err == nil {
		t.Fatal("a failed start was reported as success")
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, `"phase":"schedule-run","event":"finish","status":"fail"`) {
		t.Fatalf("the journal calls a failed request started:\n%s", seq)
	}
	// The journal redacts a failure's detail on purpose, so the record says
	// that the request failed and where to look, not what the host said.
	if !strings.Contains(seq, `"error_code":"execution_failed"`) {
		t.Fatalf("the failed finish carries no error code:\n%s", seq)
	}
	if strings.Contains(seq, `"phase":"schedule-run","event":"finish","status":"ok"`) {
		t.Fatalf("a failed request also journaled a success:\n%s", seq)
	}
}
