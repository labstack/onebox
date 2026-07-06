package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func statusFake(webRelease, recorded string) *transport.Fake {
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/" + recorded + "\n"}, true
		// one project-wide docker ps → every container as "id service"
		// one docker ps carries id|service|ob.release|status for every container
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "compose.project"):
			return transport.Result{Stdout: "S1|server|" + webRelease + "|Up (healthy)\n" +
				"W1|worker|" + recorded + "|Up (healthy)\nPG1|postgres|" + recorded + "|Up (healthy)\n"}, true
		case strings.Contains(cmd, "ls -1"): // no journals
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}
	return f
}

// The perf contract: status must not fan a docker ps or inspect out per
// container. For any number of roles/accessories it issues exactly one
// project-wide ps and one batched inspect covering every container — the whole
// reason status went from ~35 round trips to a handful on a high-latency host.
func TestStatusRoundTripBudget(t *testing.T) {
	f := statusFake("R2", "R2")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	var ps, inspect int
	for _, c := range f.Commands {
		switch {
		case strings.Contains(c, "docker ps"):
			ps++
		case strings.Contains(c, "docker inspect"):
			inspect++
		}
	}
	if ps != 1 {
		t.Fatalf("want exactly 1 docker ps (project-wide), got %d:\n%s", ps, strings.Join(f.Commands, "\n"))
	}
	if inspect != 0 { // health rides in docker ps .Status; no per-container inspect
		t.Fatalf("want 0 docker inspect (health from ps), got %d:\n%s", inspect, strings.Join(f.Commands, "\n"))
	}
}

func TestStatusInSync(t *testing.T) {
	f := statusFake("R2", "R2")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("in-sync status must not error: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "all in sync") {
		t.Fatalf("expected in-sync:\n%s", out.String())
	}
}

// After collapsing the per-service queries into one project-wide ps, a role or
// accessory simply absent from the map must still render NOT RUNNING and force
// divergence — the crashed-service signal must survive the refactor.
func TestStatusFlagsNotRunning(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R2\n"}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "compose.project"):
			// only the web role's container is up: worker + postgres are gone
			return transport.Result{Stdout: "S1|server|R2|Up (healthy)\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("a missing role/accessory must be divergence:\n%s", out.String())
	}
	s := out.String()
	if c := strings.Count(s, "NOT RUNNING"); c != 2 { // worker role + postgres accessory
		t.Fatalf("want NOT RUNNING for worker and postgres, got %d:\n%s", c, s)
	}
}

// Health now rides in docker ps .Status; an unhealthy role must still force
// divergence. Release matches recorded, so only health drives the result.
func TestStatusFlagsUnhealthyRole(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R2\n"}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "compose.project"):
			return transport.Result{Stdout: "S1|server|R2|Up (unhealthy)\n" +
				"W1|worker|R2|Up (healthy)\nPG1|postgres|R2|Up (healthy)\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("an unhealthy role must be a divergence:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "(unhealthy)") {
		t.Fatalf("unhealthy health must be shown:\n%s", out.String())
	}
}

// A crash-looping (Restarting) role container is on the recorded release yet is
// not serving. It must force divergence and show its "down" health — before, its
// status parsed to "none" and read as a healthy no-healthcheck container.
func TestStatusFlagsCrashLoopingRole(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R2\n"}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "compose.project"):
			return transport.Result{Stdout: "S1|server|R2|Restarting (1) 3 seconds ago\n" +
				"W1|worker|R2|Up (healthy)\nPG1|postgres|R2|Up (healthy)\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("a crash-looping role must be a divergence:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "(down)") {
		t.Fatalf("crash-looping role must show 'down' health:\n%s", out.String())
	}
}

// A read that errors inside the concurrent wave must fail status — not yield a
// partial table that still ends in "all in sync". A suspicious id from the ps
// makes projectContainers error; gather must propagate it.
func TestStatusSurfacesReadError(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R2\n"}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "compose.project"):
			return transport.Result{Stdout: "S1;reboot|server|R2|Up (healthy)\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err == nil {
		t.Fatal("a read error in the wave must fail status")
	}
	if strings.Contains(out.String(), "all in sync") || strings.Contains(out.String(), "ROLE") {
		t.Fatalf("status must not render a table on a failed read:\n%s", out.String())
	}
}

// A transport-level journal read failure must surface as an error — never be
// mistaken for a clean journal and reported "all in sync". This is the whole
// reason FindIncomplete returns the ErrNoIncomplete sentinel.
func TestStatusSurfacesJournalReadError(t *testing.T) {
	f := statusFake("R2", "R2") // everything else in sync
	f.Err = func(cmd string) error {
		if strings.Contains(cmd, "for f in") { // the journal.Journals command
			return errors.New("connection reset")
		}
		return nil
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	err := e.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("a journal read failure must surface, got %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "all in sync") {
		t.Fatalf("must not report clean on a failed journal read:\n%s", out.String())
	}
}

func TestStatusFlagsDivergence(t *testing.T) {
	f := statusFake("R1", "R2") // web still runs old release
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	err := e.Status(context.Background())
	if err == nil {
		t.Fatalf("divergence must be an error:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "DIVERGED") {
		t.Fatalf("expected DIVERGED marker:\n%s", out.String())
	}
}
