package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func containerEngine(t *testing.T, psOut string) (*Engine, *transport.Fake) {
	t.Helper()
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps") && strings.Contains(cmd, "--format") {
			return transport.Result{Stdout: psOut}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	return e, f
}

// The injection guard: a container id from `docker ps` that isn't a plain
// hex/alnum token must abort, not be interpolated into the next docker command.
func TestProjectContainersRejectsSuspiciousID(t *testing.T) {
	// exactly two fields, but the id carries shell metacharacters
	e, _ := containerEngine(t, "S1;reboot server\n")
	if _, err := e.projectContainers(context.Background()); err == nil {
		t.Fatal("a non-alnum container id must be rejected, not reused in a command")
	}
	// a 65-char id (validID caps at 64) is likewise refused
	e2, _ := containerEngine(t, strings.Repeat("a", 65)+" server\n")
	if _, err := e2.projectContainers(context.Background()); err == nil {
		t.Fatal("an over-length container id must be rejected")
	}
}

// Blank lines and containers with no compose-service label (a single field)
// are dropped without error; real services still map.
func TestProjectContainersDropsUnlabeled(t *testing.T) {
	e, _ := containerEngine(t, "S1 server\nORPHAN\n\nPG1 postgres\n")
	byService, err := e.projectContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(byService) != 2 || len(byService["server"]) != 1 || len(byService["postgres"]) != 1 {
		t.Fatalf("want only server+postgres, got %v", byService)
	}
}

// Multiple containers of one service keep docker's newest-first order.
func TestProjectContainersGroupsByService(t *testing.T) {
	e, _ := containerEngine(t, "A1 server\nA2 server\nP1 postgres\n")
	byService, err := e.projectContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := byService["server"]; len(got) != 2 || got[0] != "A1" || got[1] != "A2" {
		t.Fatalf("server ids/order wrong: %v", got)
	}
}

func TestContainerStatusParsesLabelAndHealth(t *testing.T) {
	inspect := func(out string) *Engine {
		f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
			if strings.Contains(cmd, "docker inspect") {
				return transport.Result{Stdout: out}, true
			}
			return transport.Result{}, false
		}}
		return New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	}
	cases := []struct{ out, wantRel, wantHealth string }{
		{"20260705-000808-e80fea1|healthy\n", "20260705-000808-e80fea1", "healthy"},
		{"|none\n", "", "none"},                               // no ob.release label, no healthcheck
		{"<no value>|unhealthy\n", "<no value>", "unhealthy"}, // nil Labels map
	}
	for _, c := range cases {
		rel, health, err := inspect(c.out).containerStatus(context.Background(), "ID1")
		if err != nil {
			t.Fatal(err)
		}
		if rel != c.wantRel || health != c.wantHealth {
			t.Fatalf("%q → (%q,%q), want (%q,%q)", c.out, rel, health, c.wantRel, c.wantHealth)
		}
	}
}
