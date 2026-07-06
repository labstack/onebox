package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// mirrors journal.journalMarker (unexported) — this test simulates the remote
// bulk-read output that Journals parses.
const journalMarkerLine = "@@ob-journal@@"

// Bounding FindIncomplete to one round trip must NOT change its answer: an
// incomplete deploy can sit behind a newer, finished one (a crash left R1
// half-done, then R2 deployed cleanly), so the scan still has to look past the
// newest journal. This is the correctness guard on the perf change.
func TestFindIncompleteScansPastNewerFinished(t *testing.T) {
	out := journalMarkerLine + "R1.jsonl\n" +
		`{"deploy_id":"R1","event":"start","ts":"t"}` + "\n" +
		journalMarkerLine + "R2.jsonl\n" +
		`{"deploy_id":"R2","event":"start","ts":"t"}` + "\n" +
		`{"deploy_id":"R2","event":"finish","status":"ok","ts":"t"}` + "\n"

	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") {
			return transport.Result{Stdout: out}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	s, err := e.FindIncomplete(context.Background())
	if err != nil {
		t.Fatalf("must find the older incomplete deploy: %v", err)
	}
	if s.DeployID != "R1" {
		t.Fatalf("want incomplete R1, got %q", s.DeployID)
	}
	if len(f.Commands) != 1 {
		t.Fatalf("want a single round trip, got %d: %v", len(f.Commands), f.Commands)
	}
}
