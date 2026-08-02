package journal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// Journals must read every deploy's records in ONE round trip, split them back
// out by the per-file marker, and preserve the parsing Summarize relies on.
func TestJournalsOneRoundTrip(t *testing.T) {
	marshal := func(recs ...Record) string {
		var ls []string
		for _, r := range recs {
			b, _ := json.Marshal(r)
			ls = append(ls, string(b))
		}
		return strings.Join(ls, "\n")
	}
	// Mimic the remote `for f in ...; do echo MARKER$f; cat $f; done` output.
	out := journalMarker + "R1.jsonl\n" +
		marshal(
			Record{DeployID: "R1", Event: "start"},
			Record{DeployID: "R1", Phase: "deploy", Event: "finish", Status: "ok"},
		) + "\n" +
		journalMarker + "R2.jsonl\n" +
		marshal(Record{DeployID: "R2", Event: "start", Detail: "prev=R1"}) + "\n" +
		"garbage-not-json\n" // torn line tolerated

	var got string
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") {
			got = cmd
			return transport.Result{Stdout: out}, true
		}
		return transport.Result{}, false
	}}

	ids, byID, err := Journals(context.Background(), f, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Commands) != 1 {
		t.Fatalf("want exactly 1 round trip, got %d: %v", len(f.Commands), f.Commands)
	}
	if !strings.Contains(got, "'/var/lib/ob/sample/journal'") {
		t.Fatalf("command must target the app's journal dir: %s", got)
	}
	if len(ids) != 2 || ids[0] != "R1" || ids[1] != "R2" {
		t.Fatalf("ids oldest-first: %v", ids)
	}
	if len(byID["R1"]) != 2 || len(byID["R2"]) != 1 {
		t.Fatalf("records grouped by id wrong: R1=%d R2=%d", len(byID["R1"]), len(byID["R2"]))
	}
	if s := Summarize(byID["R2"]); !s.Started || s.Finished || s.PrevRelease != "R1" {
		t.Fatalf("R2 summary from bulk read: %+v", s)
	}
}

// A crash can leave a journal's final record without its trailing newline. The
// bulk command's `echo` after each `cat` must still separate it from the next
// file's marker, so no records leak across deploys and every file enters ids.
func TestJournalsTornLastRecordDoesNotSwallowNextFile(t *testing.T) {
	// R1's last line has no trailing "\n" (torn write); the command's echo adds
	// one before R2's marker. R1's torn line is dropped, R2 stays intact.
	out := journalMarker + "R1.jsonl\n" +
		`{"deploy_id":"R1","event":"start"}` + "\n" +
		`{"deploy_id":"R1","event":"result","status":"ok"` + // <- torn, no closing brace/newline
		"\n" + // the trailing `echo`
		journalMarker + "R2.jsonl\n" +
		`{"deploy_id":"R2","event":"start"}` + "\n\n"

	var gotCmd string
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") {
			gotCmd = cmd
			return transport.Result{Stdout: out}, true
		}
		return transport.Result{}, false
	}}
	ids, byID, err := Journals(context.Background(), f, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCmd, `cat "$f" || exit; echo;`) {
		t.Fatalf("command must fail on unreadable journals and fence torn writes with a newline: %s", gotCmd)
	}
	if len(ids) != 2 || ids[1] != "R2" {
		t.Fatalf("R2 must not be swallowed by R1's torn line: ids=%v", ids)
	}
	if len(byID["R2"]) != 1 {
		t.Fatalf("R2's record misattributed or lost: %v", byID["R2"])
	}
}

// A never-deployed host has no journal dir: the command exits clean with no
// output, and Journals returns nothing rather than erroring.
func TestJournalsNoJournalDir(t *testing.T) {
	f := &transport.Fake{} // default: empty stdout, exit 0
	ids, byID, err := Journals(context.Background(), f, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 || len(byID) != 0 {
		t.Fatalf("want empty, got ids=%v byID=%v", ids, byID)
	}
}
