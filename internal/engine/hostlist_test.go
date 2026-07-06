package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
	"github.com/labstack/onebox/internal/ui"
)

// hostFake serves the three host reads for a mixed host: apps monk (in sync),
// blog (running an old release, unhealthy → diverged), api (recorded but not
// running), stale (dir, never activated); the ob-proxy project (summary, not a
// row); and a foreign grafana project (footer, not a row).
func hostFake() *transport.Fake {
	return &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps") && strings.Contains(cmd, "--format"):
			// id|project|service|ob.release|status  (host-wide, no --filter)
			return transport.Result{Stdout: strings.Join([]string{
				"m1|monk|server|R2|Up (healthy)",
				"m2|monk|worker|R2|Up",           // no healthcheck → none, still in sync
				"m3|monk|postgres||Up (healthy)", // accessory: no ob.release → NOT drift
				"b1|blog|web|R1|Up (unhealthy)",
				"px|ob-proxy|proxy||Up (healthy)",
				"g1|grafana|grafana||Up (healthy)", // foreign: no app dir
				"orphan-line-no-project",           // dropped: <5 fields
			}, "\n") + "\n"}, true
		case strings.Contains(cmd, "readlink"): // recorded releases
			return transport.Result{Stdout: strings.Join([]string{
				"monk|releases/R2",
				"blog|releases/R2", // recorded R2 but a container runs R1
				"api|releases/R3",  // recorded, not running
				"stale|",           // never activated
			}, "\n") + "\n"}, true
		case strings.Contains(cmd, "proxy/apps"):
			return transport.Result{Stdout: "monk\nblog\n"}, true
		}
		return transport.Result{}, false
	}}
}

func rowByApp(ov HostOverview, app string) (AppRow, bool) {
	for _, r := range ov.Apps {
		if r.App == app {
			return r, true
		}
	}
	return AppRow{}, false
}

func TestHostList(t *testing.T) {
	f := hostFake()
	u := ui.New(&bytes.Buffer{}, false)
	ov, err := HostList(context.Background(), f, u, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// proxy summary from the ob-proxy project — not an app row
	if !ov.Proxy.Managed || !ov.Proxy.Running || ov.Proxy.Health != "healthy" {
		t.Fatalf("proxy summary: %+v", ov.Proxy)
	}
	// grafana is foreign; ob-proxy is neither foreign nor an app
	if ov.Foreign != 1 {
		t.Fatalf("foreign count: want 1 (grafana), got %d", ov.Foreign)
	}
	// exactly the four app dirs, alphabetical
	if got := len(ov.Apps); got != 4 {
		t.Fatalf("app count: %d (%v)", got, ov.Apps)
	}
	if ov.Apps[0].App != "api" || ov.Apps[3].App != "stale" {
		t.Fatalf("apps must be alphabetical: %v", ov.Apps)
	}

	cases := []struct {
		app, state, health string
		running            int
		proxied            bool
	}{
		{"monk", "in sync", "healthy", 3, true}, // server+worker (R2) + unlabeled postgres accessory
		{"blog", "DIVERGED", "1 unhealthy", 1, true},
		{"api", "NOT RUNNING", "-", 0, false},
		{"stale", "never activated", "-", 0, false},
	}
	for _, c := range cases {
		r, ok := rowByApp(ov, c.app)
		if !ok {
			t.Fatalf("missing app %s", c.app)
		}
		if r.StateLabel() != c.state || r.Health != c.health || r.Running != c.running || r.Proxied != c.proxied {
			t.Fatalf("%s: got state=%q health=%q running=%d proxied=%v; want %q %q %d %v",
				c.app, r.StateLabel(), r.Health, r.Running, r.Proxied, c.state, c.health, c.running, c.proxied)
		}
	}
	if !ov.HasProblems() { // blog diverged + api not running
		t.Fatal("HasProblems must be true")
	}
}

// The whole overview is 3 host reads by default, one more with --incomplete —
// never a per-app fan-out.
func TestHostListRoundTripBudget(t *testing.T) {
	f := hostFake()
	u := ui.New(&bytes.Buffer{}, false)
	if _, err := HostList(context.Background(), f, u, ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Commands); got != 3 {
		t.Fatalf("default must be 3 host reads, got %d:\n%s", got, strings.Join(f.Commands, "\n"))
	}

	f2 := hostFake()
	if _, err := HostList(context.Background(), f2, u, ListOptions{Incomplete: true}); err != nil {
		t.Fatal(err)
	}
	if got := len(f2.Commands); got != 4 {
		t.Fatalf("--incomplete must be 4 host reads, got %d:\n%s", got, strings.Join(f2.Commands, "\n"))
	}
}

// An empty host (no app dirs, no containers) yields no rows and no proxy.
func TestHostListEmptyHost(t *testing.T) {
	f := &transport.Fake{} // every read returns empty
	u := ui.New(&bytes.Buffer{}, false)
	ov, err := HostList(context.Background(), f, u, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Apps) != 0 || ov.Foreign != 0 || ov.Proxy.Managed || ov.HasProblems() {
		t.Fatalf("empty host must be empty: %+v", ov)
	}
}

// A read error in the wave surfaces; a suspicious container id is rejected.
func TestHostListSurfacesReadError(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps") {
			return transport.Result{Stdout: "e;reboot|monk|web|R2|Up (healthy)\n"}, true
		}
		return transport.Result{}, false
	}}
	u := ui.New(&bytes.Buffer{}, false)
	if _, err := HostList(context.Background(), f, u, ListOptions{}); err == nil {
		t.Fatal("a suspicious container id must fail HostList")
	}
}
