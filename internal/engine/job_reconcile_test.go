package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// reconcileFake serves one journal listing and answers the operation-label
// container lookup with whatever `running` names.
func reconcileFake(journals string, running []string) *transport.Fake {
	return &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in"):
			return transport.Result{Stdout: journals}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "ob.operation="):
			return transport.Result{Stdout: strings.Join(running, "\n") + "\n"}, true
		}
		return transport.Result{}, false
	}}
}

func reconcileEngine(t *testing.T, f *transport.Fake) *Engine {
	t.Helper()
	return New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
}

const startedJobJournal = journalMarkerLine + "J1.jsonl\n" +
	`{"deploy_id":"J1","phase":"job","event":"start","status":"ok","operation_kind":"job_run","service":"catalog-refresh","ts":"t"}` + "\n"

// The safety crux: a container still changing data with no process owning it
// must stop the next operation, whatever the lock's TTL says about the client
// that started it.
func TestReconcileRefusesWhileAnOrphanedJobRuns(t *testing.T) {
	f := reconcileFake(startedJobJournal, []string{"CID0123456789ab"})
	err := reconcileEngine(t, f).reconcileOrphanedJobRuns(context.Background())
	if err == nil {
		t.Fatal("a live orphaned job must refuse the operation")
	}
	for _, want := range []string{"catalog-refresh", "J1", "still running", "CID012345678"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q: %v", want, err)
		}
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), `"event":"finish"`) {
		t.Fatalf("a running job must not be closed:\n%s", strings.Join(f.Commands, "\n"))
	}
}

// Gone, and the client never journaled a result: the outcome is unknown and
// must be recorded as such. Writing `ok` here would erase the rollback debt an
// unresolved data-changing job carries.
func TestReconcileClosesAGoneOrphanAsInterrupted(t *testing.T) {
	f := reconcileFake(startedJobJournal, nil)
	if err := reconcileEngine(t, f).reconcileOrphanedJobRuns(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	appended := strings.Join(f.Commands, "\n")
	if !strings.Contains(appended, `"error_code":"interrupted"`) || !strings.Contains(appended, `"status":"fail"`) {
		t.Fatalf("interrupted run was not recorded honestly:\n%s", appended)
	}
}

// The one window where success is provable after the fact: the client observed
// the exit and journaled the result, then died before the finish record.
func TestReconcileClosesAProvenSuccessAsSucceeded(t *testing.T) {
	journals := startedJobJournal +
		`{"deploy_id":"J1","phase":"job","sub_step":"job:catalog-refresh","event":"result","status":"ok","operation_kind":"job_run","ts":"t"}` + "\n"
	f := reconcileFake(journals, nil)
	if err := reconcileEngine(t, f).reconcileOrphanedJobRuns(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	appended := strings.Join(f.Commands, "\n")
	if !strings.Contains(appended, `"event":"finish"`) || !strings.Contains(appended, `"status":"ok"`) {
		t.Fatalf("a proven success was not closed as one:\n%s", appended)
	}
	if strings.Contains(appended, "interrupted") {
		t.Fatalf("a proven success must not be recorded as interrupted:\n%s", appended)
	}
}

// Deploy journals and completed job runs are not orphans.
func TestReconcileIgnoresDeploysAndFinishedRuns(t *testing.T) {
	journals := journalMarkerLine + "R1.jsonl\n" +
		`{"deploy_id":"R1","phase":"deploy","event":"start","ts":"t"}` + "\n" +
		journalMarkerLine + "J2.jsonl\n" +
		`{"deploy_id":"J2","phase":"job","event":"start","status":"ok","operation_kind":"job_run","service":"chore","ts":"t"}` + "\n" +
		`{"deploy_id":"J2","phase":"job","event":"finish","status":"ok","operation_kind":"job_run","service":"chore","ts":"t"}` + "\n"
	f := reconcileFake(journals, nil)
	if err := reconcileEngine(t, f).reconcileOrphanedJobRuns(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	joined := strings.Join(f.Commands, "\n")
	if strings.Contains(joined, "ob.operation=") {
		t.Fatalf("nothing was orphaned, so no container lookup should happen:\n%s", joined)
	}
}
