package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/transport"
)

// reconcileFake serves one journal listing and one running-container listing.
// `running` is `<id> <operation>` lines, exactly as the label probe formats.
func reconcileFake(journals string, running []string) *transport.Fake {
	return &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in"):
			return transport.Result{Stdout: journals}, true
		case strings.Contains(cmd, "label='ob.operation'"):
			return transport.Result{Stdout: strings.Join(running, "\n") + "\n"}, true
		}
		return transport.Result{}, false
	}}
}

func reconcileEngine(t *testing.T, f *transport.Fake) *Engine {
	t.Helper()
	return New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
}

// closeAll reads the journals the way a caller does, then closes what it finds.
func closeAll(t *testing.T, e *Engine) error {
	t.Helper()
	ids, byID, err := journal.Journals(context.Background(), e.T, e.names())
	if err != nil {
		t.Fatalf("read journals: %v", err)
	}
	return e.closeInterruptedJobRuns(context.Background(), ids, byID)
}

const startedJobJournal = journalMarkerLine + "J1.jsonl\n" +
	`{"deploy_id":"J1","epoch":4,"phase":"job","event":"start","status":"ok","operation_kind":"job_run","service":"catalog-refresh","ts":"t1"}` + "\n"

// The safety crux, and the reason this asks Docker rather than the journal: a
// container still changing data with no process owning it must stop the next
// operation.
func TestRefuseWhileAnotherOperationsJobContainerRuns(t *testing.T) {
	f := reconcileFake("", []string{"abc123def456 J1"})
	err := reconcileEngine(t, f).refuseForeignJobContainers(context.Background(), "J2")
	if err == nil {
		t.Fatal("a live job container from another operation must refuse")
	}
	for _, want := range []string{"J1", "abc123def456", "still running"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q: %v", want, err)
		}
	}
}

// This operation's own container is not a reason to refuse itself — a deploy
// runs gate jobs under its own id.
func TestRefuseAllowsThisOperationsOwnContainer(t *testing.T) {
	f := reconcileFake("", []string{"abc123def456 J1"})
	if err := reconcileEngine(t, f).refuseForeignJobContainers(context.Background(), "J1"); err != nil {
		t.Fatalf("own container refused: %v", err)
	}
}

// A run that recorded its own interruption still has a live container, and a
// journal reduction would call it finished. That is the Ctrl-C case.
func TestRefuseCatchesAnInterruptedRunThatRecordedItself(t *testing.T) {
	journals := startedJobJournal +
		`{"deploy_id":"J1","epoch":4,"phase":"job","event":"finish","status":"fail","error_code":"interrupted","operation_kind":"job_run","service":"catalog-refresh","ts":"t2"}` + "\n"
	f := reconcileFake(journals, []string{"abc123def456 J1"})
	if err := reconcileEngine(t, f).refuseForeignJobContainers(context.Background(), "J2"); err == nil {
		t.Fatal("a recorded interruption must not hide a live container")
	}
}

// Gone, and the client never journaled a result: the outcome is unknown.
func TestCloseRecordsAnUnknownOutcomeAsInterrupted(t *testing.T) {
	f := reconcileFake(startedJobJournal, nil)
	if err := closeAll(t, reconcileEngine(t, f)); err != nil {
		t.Fatalf("close: %v", err)
	}
	appended := strings.Join(f.Commands, "\n")
	if !strings.Contains(appended, `"error_code":"interrupted"`) || !strings.Contains(appended, `"event":"finish"`) {
		t.Fatalf("interrupted run was not recorded honestly:\n%s", appended)
	}
	// The epoch groups a journal into invocations. Written without it, the
	// record lands in an invocation of its own and leaves this one open.
	if !strings.Contains(appended, `"epoch":4`) {
		t.Fatalf("terminal record did not join the invocation it closes:\n%s", appended)
	}
}

// The one window where success is provable after the fact. The result record is
// written by the shared job phase and carries no operation kind, so matching it
// on that would silently never fire.
func TestCloseRecordsAJournaledResultAsSuccess(t *testing.T) {
	journals := startedJobJournal +
		`{"deploy_id":"J1","epoch":4,"phase":"job","sub_step":"job:catalog-refresh","event":"result","status":"ok","ts":"t2"}` + "\n"
	f := reconcileFake(journals, nil)
	if err := closeAll(t, reconcileEngine(t, f)); err != nil {
		t.Fatalf("close: %v", err)
	}
	appended := strings.Join(f.Commands, "\n")
	if !strings.Contains(appended, `"status":"ok"`) || strings.Contains(appended, "interrupted") {
		t.Fatalf("a proven success was not closed as one:\n%s", appended)
	}
}

// A plan may be run more than once, appending a second invocation to the same
// journal. A finish in an earlier epoch says nothing about a later one.
func TestCloseGroupsAJournalByInvocation(t *testing.T) {
	journals := startedJobJournal +
		`{"deploy_id":"J1","epoch":4,"phase":"job","event":"finish","status":"ok","operation_kind":"job_run","service":"catalog-refresh","ts":"t2"}` + "\n" +
		`{"deploy_id":"J1","epoch":5,"phase":"job","event":"start","status":"ok","operation_kind":"job_run","service":"catalog-refresh","ts":"t3"}` + "\n"
	f := reconcileFake(journals, nil)
	if err := closeAll(t, reconcileEngine(t, f)); err != nil {
		t.Fatalf("close: %v", err)
	}
	appended := strings.Join(f.Commands, "\n")
	if !strings.Contains(appended, `"epoch":5`) {
		t.Fatalf("the unfinished second invocation was not closed:\n%s", appended)
	}
	if strings.Count(appended, `"event":"finish"`) != 1 {
		t.Fatalf("the finished invocation must not be closed again:\n%s", appended)
	}
}

// Deploy journals and completed runs are not orphans.
func TestCloseIgnoresDeploysAndFinishedRuns(t *testing.T) {
	journals := journalMarkerLine + "R1.jsonl\n" +
		`{"deploy_id":"R1","epoch":1,"phase":"deploy","event":"start","ts":"t"}` + "\n" +
		journalMarkerLine + "J2.jsonl\n" +
		`{"deploy_id":"J2","epoch":1,"phase":"job","event":"start","status":"ok","operation_kind":"job_run","service":"chore","ts":"t"}` + "\n" +
		`{"deploy_id":"J2","epoch":1,"phase":"job","event":"finish","status":"ok","operation_kind":"job_run","service":"chore","ts":"t"}` + "\n"
	f := reconcileFake(journals, nil)
	if err := closeAll(t, reconcileEngine(t, f)); err != nil {
		t.Fatalf("close: %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), `"event":"finish"`) {
		t.Fatalf("nothing was unfinished, so nothing should be written:\n%s", strings.Join(f.Commands, "\n"))
	}
}

// A recorded failure is evidence of the outcome exactly as much as a recorded
// success. Calling it interrupted would hide that the job ran and failed on its
// own terms.
func TestCloseKeepsARecordedFailureAsAFailure(t *testing.T) {
	journals := startedJobJournal +
		`{"deploy_id":"J1","epoch":4,"phase":"job","sub_step":"job:catalog-refresh","event":"result","status":"fail","ts":"t2"}` + "\n"
	f := reconcileFake(journals, nil)
	if err := closeAll(t, reconcileEngine(t, f)); err != nil {
		t.Fatalf("close: %v", err)
	}
	appended := strings.Join(f.Commands, "\n")
	if !strings.Contains(appended, `"status":"fail"`) {
		t.Fatalf("a recorded failure was not closed as one:\n%s", appended)
	}
	if strings.Contains(appended, "interrupted") {
		t.Fatalf("a known failure must not be reported as an unknown outcome:\n%s", appended)
	}
}
