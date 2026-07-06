package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/transport"
)

func journalLines(recs ...journal.Record) string {
	var lines []string
	for _, r := range recs {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	return strings.Join(lines, "\n") + "\n"
}

// interruptedFake: R1 crashed after web rolled; worker pending. R0 is live.
func interruptedFake(gateDetail string) *transport.Fake {
	f := happyFake()
	jr := journalLines(
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "deploy", Event: "start", Detail: "prev=R0", Operator: "v@mac", TS: "2026-07-03T00:00:00Z"},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "transfer", Event: "result", Status: "ok"},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "pre-release", SubStep: "migrate", Event: "result", Status: "ok", Detail: gateDetail},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "release", Role: "web", Event: "result", Status: "ok"},
	)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/monk/journal"):
			return transport.Result{Stdout: journalMarkerLine + "R1.jsonl\n" + jr}, true
		case strings.Contains(cmd, "test -d"):
			return transport.Result{ExitCode: 0}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R0\n"}, true
		case strings.Contains(cmd, "ls -1 '/var/lib/ob/monk/releases'"):
			return transport.Result{Stdout: "R0\nR1\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestResumeSkipsCompletedStepsAndFinishes(t *testing.T) {
	f := interruptedFake("changed=false")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "--scale server=2") {
		t.Fatalf("web already rolled — resume must skip it:\n%s", seq)
	}
	if strings.Contains(seq, "OB_RESULT_FILE") {
		t.Fatalf("migrate already ran — resume must not re-run it:\n%s", seq)
	}
	if !strings.Contains(seq, "--force-recreate worker") {
		t.Fatalf("pending worker must be released:\n%s", seq)
	}
	if !strings.Contains(seq, "ln -sfn 'releases/R1'") {
		t.Fatalf("resumed deploy must activate:\n%s", seq)
	}
	if !strings.Contains(seq, `"event":"finish","status":"ok"`) {
		t.Fatalf("resumed deploy must journal finish:\n%s", seq)
	}
}

func TestResumeWithNothingIncomplete(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/monk/journal") {
			return transport.Result{Stdout: journalMarkerLine + "R1.jsonl\n" + journalLines(
				journal.Record{DeployID: "R1", Phase: "deploy", Event: "start"},
				journal.Record{DeployID: "R1", Phase: "deploy", Event: "finish", Status: "ok"},
			)}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err == nil || !strings.Contains(err.Error(), "no incomplete") {
		t.Fatalf("want no-incomplete error, got %v", err)
	}
}

func TestAbortRefusesClosedGate(t *testing.T) {
	f := interruptedFake("changed=unknown (no result declared — gate closed, fail-safe)")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Abort(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("closed gate must refuse abort: %v", err)
	}
}

func TestAbortReplaysPreviousRelease(t *testing.T) {
	f := interruptedFake("changed=false")
	// abort path: web rolled to R1 — its container carries ob.release='R1';
	// replaying R0 must drain it. The fake: newcomer query for R0 returns the
	// R0 container only after R0's up --scale ran.
	base := f.Dynamic
	r0Scaled := func() bool {
		for _, c := range f.Commands {
			if strings.Contains(c, "releases/R0/compose.yaml' up -d --no-deps --no-recreate --scale server=2") {
				return true
			}
		}
		return false
	}
	oldGone := func() bool {
		for _, c := range f.Commands {
			if strings.Contains(c, "docker rm OLD1") {
				return true
			}
		}
		return false
	}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "ob.release='R0'") && strings.Contains(cmd, "service='server'") {
			if r0Scaled() {
				return transport.Result{Stdout: "PREV1\n"}, true
			}
			return transport.Result{Stdout: ""}, true
		}
		// live server set: OLD1 (the R1 container being replaced) until removed,
		// plus the R0 newcomer PREV1 once the R0 scale ran.
		if strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service='server'") && !strings.Contains(cmd, "ob.release=") {
			var ids []string
			if !oldGone() {
				ids = append(ids, "OLD1")
			}
			if r0Scaled() {
				ids = append(ids, "PREV1")
			}
			return transport.Result{Stdout: strings.Join(ids, "\n") + "\n"}, true
		}
		if strings.Contains(cmd, "ob.release='R0'") && strings.Contains(cmd, "service='worker'") {
			return transport.Result{Stdout: ""}, true // worker never completed → recreate from R0
		}
		if strings.Contains(cmd, "ob.release='R1'") {
			return transport.Result{Stdout: ""}, true // straggler sweep finds none
		}
		if strings.Contains(cmd, "inspect") && strings.Contains(cmd, "PREV1") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		return base(cmd)
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Abort(context.Background(), false); err != nil {
		t.Fatalf("abort: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "releases/R0/compose.yaml' up -d --no-deps --no-recreate --scale server=2") {
		t.Fatalf("abort must roll web back to R0:\n%s", seq)
	}
	if !strings.Contains(seq, `"event":"abort","status":"ok"`) {
		t.Fatalf("abort must journal:\n%s", seq)
	}
}
