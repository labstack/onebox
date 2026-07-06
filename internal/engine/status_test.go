package engine

import (
	"bytes"
	"context"
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
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "compose.project"):
			return transport.Result{Stdout: "S1 server\nW1 worker\nPG1 postgres\n"}, true
		// merged inspect: "<ob.release>|<health>"
		case strings.Contains(cmd, "ob.release") && strings.Contains(cmd, "S1"):
			return transport.Result{Stdout: webRelease + "|healthy\n"}, true
		case strings.Contains(cmd, "ob.release") && strings.Contains(cmd, "W1"):
			return transport.Result{Stdout: recorded + "|healthy\n"}, true
		case strings.Contains(cmd, "Health"): // accessory health-only inspect
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "ls -1"): // no journals
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}
	return f
}

// The perf contract: status must not fan out a docker ps per role/accessory,
// and must not inspect a container twice for label+health. For the 2-role +
// 1-accessory fixture that means exactly one project-wide ps and one inspect
// per container — a regression here is what made status slow on a high-latency
// host.
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
	if inspect != 3 { // S1 + W1 (roles) + PG1 (accessory)
		t.Fatalf("want 3 docker inspect (one per container), got %d:\n%s", inspect, strings.Join(f.Commands, "\n"))
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
			return transport.Result{Stdout: "S1 server\n"}, true
		case strings.Contains(cmd, "ob.release") && strings.Contains(cmd, "S1"):
			return transport.Result{Stdout: "R2|healthy\n"}, true
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
