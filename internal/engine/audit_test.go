package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/transport"
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

func TestAuditExposesSafeExecInvocationEvidence(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "exec-1.jsonl\n"}, true
		case strings.Contains(cmd, "exec-1.jsonl"):
			return transport.Result{Stdout: journalLines(
				journal.Record{DeployID: "exec-1", Phase: "exec", Event: "start", Operator: "v@mac", TS: "t1", Target: "web", TargetKind: "workload", CommandDigest: "abc123", Reason: "inspect incident 42"},
				journal.Record{DeployID: "exec-1", Phase: "exec", Event: "finish", Status: "ok"},
			)}, true
		}
		return transport.Result{}, false
	}}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	records, err := e.AuditSnapshot(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Action != "exec" || records[0].Target != "web" || records[0].TargetKind != "workload" || records[0].CommandDigest != "abc123" || records[0].Reason != "inspect incident 42" || records[0].Outcome != "succeeded" {
		t.Fatalf("exec audit = %+v", records)
	}
	if err := e.Audit(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "command_digest=abc123") || !strings.Contains(out.String(), "reason=inspect incident 42") {
		t.Fatalf("human exec audit = %s", out.String())
	}
}
