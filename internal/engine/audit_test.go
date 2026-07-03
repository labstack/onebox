package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/transport"
)

func TestAuditListsOutcomesNewestFirst(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "R1.jsonl\nR2.jsonl\n"}, true
		case strings.Contains(cmd, "R1.jsonl"):
			return transport.Result{Stdout: journalLines(
				journal.Record{DeployID: "R1", Phase: "deploy", Event: "start", Operator: "v@mac", GitSHA: "abc1234", TS: "t1"},
				journal.Record{DeployID: "R1", Phase: "deploy", Event: "finish", Status: "ok"},
			)}, true
		case strings.Contains(cmd, "R2.jsonl"):
			return transport.Result{Stdout: journalLines(
				journal.Record{DeployID: "R2", Phase: "deploy", Event: "start", Operator: "ci@runner", GitSHA: "def5678", TS: "t2"},
			)}, true
		}
		return transport.Result{}, false
	}}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Audit(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "deployed") || !strings.Contains(s, "INCOMPLETE") {
		t.Fatalf("outcomes missing:\n%s", s)
	}
	if strings.Index(s, "R2") > strings.Index(s, "R1") {
		t.Fatalf("newest first expected:\n%s", s)
	}
	if !strings.Contains(s, "ci@runner") || !strings.Contains(s, "abc1234") {
		t.Fatalf("operator/sha missing:\n%s", s)
	}
}
