package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func pausableFixture(t *testing.T) (*app.Resolved, *transport.Fake) {
	t.Helper()
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := happyFake()
	return cfg, f
}

func TestSchedulePauseStopsTheTimerAndRecordsWhy(t *testing.T) {
	cfg, f := pausableFixture(t)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "command -v flock") {
			return transport.Result{Stdout: "ok\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.SchedulePause(context.Background(), "op-1", "nightly", "upstream is returning garbage"); err != nil {
		t.Fatalf("pause: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "systemctl disable --now 'ob-sample-nightly.timer'") {
		t.Fatalf("the timer was not stopped:\n%s", seq)
	}
	if !strings.Contains(seq, "nightly.paused") {
		t.Fatalf("no pause marker was written:\n%s", seq)
	}
	written := strings.Join(f.Inputs, "\n")
	if !strings.Contains(written, "reason=upstream is returning garbage") || !strings.Contains(written, "operator=") {
		t.Fatalf("the marker does not say who paused it or why:\n%s", written)
	}
	if !strings.Contains(seq, `"phase":"schedule-pause"`) {
		t.Fatalf("the pause was not journaled:\n%s", seq)
	}
}

func TestSchedulePauseRequiresAReason(t *testing.T) {
	cfg, f := pausableFixture(t)
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.SchedulePause(context.Background(), "op-1", "nightly", "   "); err == nil {
		t.Fatal("a pause with no reason was accepted")
	}
	for _, command := range f.Commands {
		if strings.Contains(command, "systemctl disable") || strings.Contains(command, ".paused") {
			t.Fatalf("a refused pause reached the host: %s", command)
		}
	}
	if err := e.SchedulePause(context.Background(), "op-1", "web", "because"); err == nil {
		t.Fatal("a workload with no schedule was accepted")
	}
}

func TestScheduleResumeClearsTheMarkerAndStartsTheTimer(t *testing.T) {
	cfg, f := pausableFixture(t)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "command -v flock") {
			return transport.Result{Stdout: "ok\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.ScheduleResume(context.Background(), "op-2", "nightly"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "rm -f '/var/lib/ob/sample/schedule/nightly.paused'") {
		t.Fatalf("the marker was not removed:\n%s", seq)
	}
	if !strings.Contains(seq, "systemctl enable --now 'ob-sample-nightly.timer'") {
		t.Fatalf("the timer was not started:\n%s", seq)
	}
	if !strings.Contains(seq, `"phase":"schedule-resume"`) {
		t.Fatalf("the resume was not journaled:\n%s", seq)
	}
}

// A pause that a deploy quietly undoes is not a pause.
func TestSyncSchedulesLeavesAPausedTimerStopped(t *testing.T) {
	cfg, f := pausableFixture(t)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "list-unit-files"):
			return transport.Result{}, true
		case strings.Contains(cmd, "systemd-analyze calendar"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "@@paused:"):
			return transport.Result{Stdout: "@@paused:nightly\noperator=v@volt\npaused_at=2026-09-06T10:00:00Z\nreason=upstream is returning garbage\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.SyncSchedules(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "systemctl enable --now ob-sample-nightly.timer") {
		t.Fatalf("reconciliation re-enabled a paused timer:\n%s", seq)
	}
	if !strings.Contains(seq, "systemctl disable --now ob-sample-nightly.timer") {
		t.Fatalf("a paused job's timer was left running:\n%s", seq)
	}
	// The units are still written, so a fix lands even while paused.
	if artifacts := strings.Join(f.Inputs, "\n"); !strings.Contains(artifacts, "ob-sample-nightly.run") &&
		!strings.Contains(artifacts, "TimeoutStartSec") {
		t.Fatalf("a paused job stopped receiving unit updates:\n%s", artifacts)
	}
}

// A paused job is not running. Status says so on its own line, and does not
// call the stopped timer a fault.
func TestScheduleStatusReportsAPauseWithoutCallingItDivergence(t *testing.T) {
	cfg, _ := pausableFixture(t)
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@journal
persistent
@@nightly:service
LoadState=loaded
ActiveState=inactive
Result=success
ExecMainStatus=0
@@nightly:timer
LoadState=loaded
ActiveState=inactive
@@nightly:run
@@nightly:history
@@nightly:paused
operator=v@volt
paused_at=2026-09-06T10:00:00Z
reason=upstream is returning garbage
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
	if got.Paused == nil || got.Paused.Reason != "upstream is returning garbage" || got.Paused.Operator != "v@volt" {
		t.Fatalf("the pause was not surfaced: %#v", got.Paused)
	}
	if got.Diverged {
		t.Fatalf("a deliberate pause was reported as divergence: %#v", got.Issues)
	}
	if strings.Contains(strings.Join(got.Issues, "; "), "timer is not active") {
		t.Fatalf("a paused timer was called a fault: %#v", got.Issues)
	}
}

// Without a marker, a stopped timer is still divergence.
func TestScheduleStatusStillFlagsAStoppedTimerThatNobodyPaused(t *testing.T) {
	cfg, _ := pausableFixture(t)
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@journal
persistent
@@nightly:service
LoadState=loaded
ActiveState=inactive
@@nightly:timer
LoadState=loaded
ActiveState=inactive
@@nightly:run
@@nightly:history
@@nightly:paused
`}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses[0]; got.Paused != nil || !got.Diverged {
		t.Fatalf("an unexplained stopped timer must still be divergence: %#v", got)
	}
}
