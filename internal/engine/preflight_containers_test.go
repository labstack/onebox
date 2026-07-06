package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// projectContainers reads "id|service|ob.release|status" lines.
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

// The injection guard: a container id that isn't a plain hex/alnum token must
// abort, not flow into the map a caller might reuse in a command.
func TestProjectContainersRejectsSuspiciousID(t *testing.T) {
	e, _ := containerEngine(t, "S1;reboot|server|R2|Up (healthy)\n")
	if _, err := e.projectContainers(context.Background()); err == nil {
		t.Fatal("a non-alnum container id must be rejected")
	}
	e2, _ := containerEngine(t, strings.Repeat("a", 65)+"|server|R2|Up\n") // >64 chars
	if _, err := e2.projectContainers(context.Background()); err == nil {
		t.Fatal("an over-length container id must be rejected")
	}
}

// Blank lines and containers with no compose-service label are dropped; real
// services still map, carrying release + parsed health.
func TestProjectContainersParsesAndDropsUnlabeled(t *testing.T) {
	e, _ := containerEngine(t, "S1|server|R2|Up 2 hours (healthy)\nORPHAN\n\nPG1|postgres||Up 2 days\n")
	byService, err := e.projectContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(byService) != 2 {
		t.Fatalf("want only server+postgres, got %v", byService)
	}
	if c := byService["server"][0]; c.id != "S1" || c.release != "R2" || c.health != "healthy" {
		t.Fatalf("server parse wrong: %+v", c)
	}
	if c := byService["postgres"][0]; c.release != "" || c.health != "none" { // no label, no healthcheck
		t.Fatalf("postgres parse wrong: %+v", c)
	}
}

// Multiple containers of one service keep docker's newest-first order.
func TestProjectContainersGroupsByService(t *testing.T) {
	e, _ := containerEngine(t, "A1|server|R2|Up (healthy)\nA2|server|R2|Up (healthy)\nP1|postgres||Up\n")
	byService, err := e.projectContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := byService["server"]; len(got) != 2 || got[0].id != "A1" || got[1].id != "A2" {
		t.Fatalf("server ids/order wrong: %v", got)
	}
}

// healthFromStatus mirrors what docker inspect .State.Health.Status would say
// for a RUNNING container, and reports "down" for one that isn't up — a
// crash-looping (Restarting) container must not read as a healthy no-healthcheck
// one ("none"), which is exactly what status treats as fine.
func TestHealthFromStatus(t *testing.T) {
	cases := map[string]string{
		"Up 25 hours":                     "none",
		"Up 25 hours (healthy)":           "healthy",
		"Up 3 minutes (unhealthy)":        "unhealthy",
		"Up 5 seconds (health: starting)": "starting",
		"Restarting (1) 5 seconds ago":    "down", // crash loop — was silently "none" before
		"Exited (1) 2 seconds ago":        "down",
		"Created":                         "down",
		"Dead":                            "down",
	}
	for status, want := range cases {
		if got := healthFromStatus(status); got != want {
			t.Fatalf("healthFromStatus(%q) = %q, want %q", status, got, want)
		}
	}
}
