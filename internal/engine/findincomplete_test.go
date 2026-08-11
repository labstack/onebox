package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// mirrors journal.journalMarker (unexported) — this test simulates the remote
// bulk-read output that Journals parses.
const journalMarkerLine = "@@ob-journal@@"

// A crash left R1 half-done, then R2 deployed cleanly. R2 rolled every role and
// activated its own release, so nothing about R1 is still completable: resuming
// it would re-activate a superseded release, and aborting it would revert to a
// predecessor two releases stale. R1 stays in `ob audit` as history; it is not
// reported as work waiting to be done.
//
// The single round trip is the other half of this test: the scan is bounded, and
// bounding it must not change the answer.
func TestFindIncompleteIgnoresADeploySupersededByANewerOne(t *testing.T) {
	out := journalMarkerLine + "R1.jsonl\n" +
		`{"deploy_id":"R1","phase":"deploy","event":"start","ts":"t"}` + "\n" +
		journalMarkerLine + "R2.jsonl\n" +
		`{"deploy_id":"R2","phase":"deploy","event":"start","ts":"t"}` + "\n" +
		`{"deploy_id":"R2","phase":"deploy","event":"finish","status":"ok","ts":"t"}` + "\n"

	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") {
			return transport.Result{Stdout: out}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	if _, err := e.FindIncomplete(context.Background()); !errors.Is(err, ErrNoIncomplete) {
		t.Fatalf("err = %v, want ErrNoIncomplete", err)
	}
	if len(f.Commands) != 1 {
		t.Fatalf("want a single round trip, got %d: %v", len(f.Commands), f.Commands)
	}
}

// A journal that is not a deploy at all — a manual job, a service apply — must
// not be mistaken for the newest deploy and hide the incomplete one behind it.
func TestFindIncompleteLooksPastNonDeployJournals(t *testing.T) {
	out := journalMarkerLine + "R1.jsonl\n" +
		`{"deploy_id":"R1","phase":"deploy","event":"start","ts":"t"}` + "\n" +
		journalMarkerLine + "R2.jsonl\n" +
		`{"deploy_id":"R2","phase":"job","event":"start","ts":"t"}` + "\n" +
		`{"deploy_id":"R2","phase":"job","event":"finish","status":"ok","ts":"t"}` + "\n"

	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") {
			return transport.Result{Stdout: out}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	s, err := e.FindIncomplete(context.Background())
	if err != nil {
		t.Fatalf("must find the incomplete deploy: %v", err)
	}
	if s.DeployID != "R1" {
		t.Fatalf("want incomplete R1, got %q", s.DeployID)
	}
}

func TestFindIncompleteReturnsFailedRecoveryAttempt(t *testing.T) {
	out := journalMarkerLine + "R1.jsonl\n" +
		`{"deploy_id":"R1","phase":"deploy","event":"start","ts":"t"}` + "\n" +
		`{"deploy_id":"R1","phase":"deploy","event":"finish","status":"fail","ts":"t"}` + "\n" +
		`{"deploy_id":"R1","phase":"abort","event":"abort","status":"fail","ts":"t"}` + "\n"

	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") {
			return transport.Result{Stdout: out}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	s, err := e.FindIncomplete(context.Background())
	if err != nil {
		t.Fatalf("failed deploy and abort attempts must remain recoverable: %v", err)
	}
	if s.DeployID != "R1" || !s.Failed {
		t.Fatalf("unexpected incomplete summary: %+v", s)
	}
}
