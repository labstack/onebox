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
		switch {
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "@@paused:"):
			return transport.Result{Stdout: "@@paused:nightly\nexists=1\noperator=v@volt\n"}, true
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
			return transport.Result{Stdout: "@@paused:nightly\nexists=1\noperator=v@volt\npaused_at=2026-09-06T10:00:00Z\nreason=upstream is returning garbage\n"}, true
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
exists=1
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

// The marker's existence is the statement. A marker whose fields never landed
// — a truncated write, a `touch` by hand — still means paused, because the
// alternative is a deploy quietly starting a timer somebody stopped.
func TestPausedJobsCountsAMarkerWithNoFields(t *testing.T) {
	cfg, _ := pausableFixture(t)
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "@@paused:") {
			return transport.Result{Stdout: "@@paused:nightly\nexists=1\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	paused, err := e.pausedJobs(context.Background(), []string{"nightly"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := paused["nightly"]; !ok {
		t.Fatalf("a marker with no fields was read as not paused: %#v", paused)
	}
	// Reading the fields is not enough: the host has to be asked whether the
	// file is there, because a marker with no readable fields is still a pause.
	if !strings.Contains(strings.Join(f.Commands, "\n"), "[ -e '/var/lib/ob/sample/schedule/nightly.paused' ]") {
		t.Fatalf("the read does not test whether the marker exists:\n%s", strings.Join(f.Commands, "\n"))
	}
}

// Resume clears the marker before it starts the timer. The other order leaves
// a running job described on disk as paused, which status believes and the
// next deploy acts on by stopping it again.
func TestScheduleResumeClearsTheMarkerBeforeStartingTheTimer(t *testing.T) {
	cfg, f := pausableFixture(t)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "@@paused:"):
			return transport.Result{Stdout: "@@paused:nightly\nexists=1\noperator=v@volt\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.ScheduleResume(context.Background(), "op-2", "nightly"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	removed, enabled := -1, -1
	for i, cmd := range f.Commands {
		if removed < 0 && strings.Contains(cmd, "rm -f '/var/lib/ob/sample/schedule/nightly.paused'") {
			removed = i
		}
		if enabled < 0 && strings.Contains(cmd, "systemctl enable --now 'ob-sample-nightly.timer'") {
			enabled = i
		}
	}
	if removed < 0 || enabled < 0 {
		t.Fatalf("resume did not both clear the marker and start the timer:\n%s", strings.Join(f.Commands, "\n"))
	}
	if removed > enabled {
		t.Fatalf("the timer was started while the marker still said paused:\n%s", strings.Join(f.Commands, "\n"))
	}
}

// Pausing twice must not throw away who stopped the job the first time, or why.
func TestSchedulePauseRefusesToOverwriteAnExistingPause(t *testing.T) {
	cfg, f := pausableFixture(t)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "@@paused:"):
			return transport.Result{Stdout: "@@paused:nightly\nexists=1\noperator=first@volt\nreason=the original reason\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.SchedulePause(context.Background(), "op-3", "nightly", "a second reason")
	if err == nil {
		t.Fatal("a second pause silently replaced the first")
	}
	if !strings.Contains(err.Error(), "the original reason") {
		t.Fatalf("the refusal does not say what the standing pause is: %v", err)
	}
	for _, cmd := range f.Commands {
		if strings.Contains(cmd, "cat > '/var/lib/ob/sample/schedule/nightly.paused'") {
			t.Fatalf("the standing marker was overwritten: %s", cmd)
		}
	}
}

// Resuming a job nobody paused is not a state change, so it must not be
// recorded as one in the audit trail.
func TestScheduleResumeRefusesAJobThatIsNotPaused(t *testing.T) {
	cfg, f := pausableFixture(t)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "@@paused:"):
			return transport.Result{Stdout: "@@paused:nightly\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.ScheduleResume(context.Background(), "op-4", "nightly"); err == nil {
		t.Fatal("resuming a job that was never paused was accepted")
	}
	for _, cmd := range f.Commands {
		if strings.Contains(cmd, `"phase":"schedule-resume"`) {
			t.Fatalf("a resume that did nothing was journaled: %s", cmd)
		}
	}
}

// A marker outlives the job it names unless something removes it, and the job
// name is reusable. Reconciliation owns the schedule directory, so it is where
// the orphan goes.
func TestSyncSchedulesRemovesThePauseMarkerOfAnUndeclaredJob(t *testing.T) {
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
		case strings.Contains(cmd, ".paused") && strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "nightly.paused\nretired.paused\n"}, true
		case strings.Contains(cmd, "@@paused:"):
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.SyncSchedules(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "'/var/lib/ob/sample/schedule/retired.paused'") {
		t.Fatalf("the orphaned marker was left behind:\n%s", seq)
	}
	if strings.Contains(seq, "rm -f '/var/lib/ob/sample/schedule/nightly.paused'") {
		t.Fatalf("reconciliation deleted a declared job's pause:\n%s", seq)
	}
}

// list is the survey command. A deliberately stopped job that renders exactly
// like a broken one is the ambiguity the pause marker exists to remove.
func TestScheduleListSaysWhenAJobIsPaused(t *testing.T) {
	cfg, _ := pausableFixture(t)
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: "@@nightly\nActiveState=inactive\n" +
				"@@paused:nightly\nexists=1\noperator=v@volt\nreason=upstream is returning garbage\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	listing, err := e.ScheduleList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing) != 1 {
		t.Fatalf("want one job, got %#v", listing)
	}
	if listing[0].Paused == nil || listing[0].Paused.Reason != "upstream is returning garbage" {
		t.Fatalf("list does not report the pause: %#v", listing[0])
	}
	if calls := len(f.Commands); calls != 1 {
		t.Fatalf("list must stay one round trip, made %d:\n%s", calls, strings.Join(f.Commands, "\n"))
	}
}

// A pause explains a stopped timer. It does not explain a missing unit or a
// failed run, and it must not stop status reporting them: an operator who
// pauses a failing job would otherwise get a green report over a broken host.
func TestStatusKeepsABrokenPausedJobVisible(t *testing.T) {
	cfg, _ := pausableFixture(t)
	f := statusFake("R2", "R2")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: "@@journal\npersistent\n" +
				"@@nightly:service\nLoadState=not-found\nActiveState=inactive\n" +
				"@@nightly:timer\nLoadState=loaded\nActiveState=inactive\n" +
				"@@nightly:run\n@@nightly:history\n" +
				"@@nightly:paused\nexists=1\noperator=v@volt\nreason=upstream is returning garbage\n"}, true
		}
		return base(cmd)
	}
	var out bytes.Buffer
	e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("a paused job with a unit that will not load was called in sync:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "PAUSED") {
		t.Fatalf("the pause was not reported:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "service unit is not loaded") {
		t.Fatalf("the pause hid a broken unit:\n%s", out.String())
	}
}
