package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
	"github.com/labstack/onebox/internal/ui"
)

// hostFake serves the host reads for a mixed host covering every app state:
// monk (in sync), blog (old release + unhealthy → diverged), drift (healthy but
// off its release → diverged via drift alone), starting (on release, warming up
// → not a problem), api (recorded, not running), stale (dir, never activated),
// ghost (containers but no current symlink → running unrecorded); the ob-proxy
// project (proxy summary, not a row); and a foreign grafana project (footer).
func hostFake() *transport.Fake {
	return &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps") && strings.Contains(cmd, "--format"):
			// id|project|service|ob.release|status  (host-wide, no --filter)
			return transport.Result{Stdout: strings.Join([]string{
				"m1|monk|server|R2|Up (healthy)",
				"m2|monk|worker|R2|Up",                        // no healthcheck → none, still in sync
				"m3|monk|postgres||Up (healthy)",              // accessory: no ob.release → NOT drift
				"b1|blog|web|R1|Up (unhealthy)",               // wrong release AND unhealthy → diverged
				"d1|drift|web|R1|Up (healthy)",                // healthy but wrong release → diverged (drift alone)
				"s1|starting|web|R2|Up 3s (health: starting)", // on release, warming up → starting (not drift)
				"gh1|ghost|web||Up (healthy)",                 // containers, no current symlink → running unrecorded
				"px|ob-proxy|proxy||Up (healthy)",             // proxy summary, not a row
				"g1|grafana|grafana||Up (healthy)",            // foreign: no app dir
				"orphan-line-no-project",                      // dropped: <5 fields
			}, "\n") + "\n"}, true
		case strings.Contains(cmd, "readlink"): // recorded releases
			return transport.Result{Stdout: strings.Join([]string{
				"monk|releases/R2",
				"blog|releases/R2",  // recorded R2 but a container runs R1
				"drift|releases/R2", // recorded R2 but a healthy container runs R1
				"starting|releases/R2",
				"api|releases/R3", // recorded, not running
				"stale|",          // dir, never activated
				"ghost|",          // dir with containers but no current → running unrecorded
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
	// grafana is foreign; ob-proxy is neither foreign nor an app; ghost is an app
	if ov.Foreign != 1 {
		t.Fatalf("foreign count: want 1 (grafana), got %d", ov.Foreign)
	}
	// exactly the seven app dirs, alphabetical
	if got := len(ov.Apps); got != 7 {
		t.Fatalf("app count: %d (%v)", got, ov.Apps)
	}
	if ov.Apps[0].App != "api" || ov.Apps[6].App != "starting" {
		t.Fatalf("apps must be alphabetical: %v", ov.Apps)
	}

	cases := []struct {
		app, state, key, health string
		running                 int
		proxied                 bool
	}{
		{"monk", "in sync", "in_sync", "healthy", 3, true},       // server+worker (R2) + unlabeled postgres accessory
		{"blog", "DIVERGED", "diverged", "1 unhealthy", 1, true}, // wrong release AND unhealthy
		{"drift", "DIVERGED", "diverged", "healthy", 1, false},   // drift alone: healthy container off its release
		{"starting", "starting", "starting", "1 starting", 1, false},
		{"api", "NOT RUNNING", "not_running", "-", 0, false},
		{"stale", "never activated", "never_activated", "-", 0, false},
		{"ghost", "running (unrecorded)", "running_unrecorded", "healthy", 1, false},
	}
	for _, c := range cases {
		r, ok := rowByApp(ov, c.app)
		if !ok {
			t.Fatalf("missing app %s", c.app)
		}
		if r.StateLabel() != c.state || r.StateKey() != c.key || r.Health != c.health || r.Running != c.running || r.Proxied != c.proxied {
			t.Fatalf("%s: got state=%q key=%q health=%q running=%d proxied=%v; want %q %q %q %d %v",
				c.app, r.StateLabel(), r.StateKey(), r.Health, r.Running, r.Proxied, c.state, c.key, c.health, c.running, c.proxied)
		}
	}
	if !ov.HasProblems() { // blog + drift diverged, api not running, ghost unrecorded
		t.Fatal("HasProblems must be true")
	}
}

// starting and never-activated are deliberately NOT problems (they must not trip
// --fail-on-drift); not-running, running-unrecorded, and diverged are.
func TestAppStateProblemClassification(t *testing.T) {
	problem := map[appState]bool{
		stateInSync:            false,
		stateStarting:          false,
		stateNeverActivated:    false,
		stateUnknown:           false,
		stateNotRunning:        true,
		stateRunningUnrecorded: true,
		stateDiverged:          true,
	}
	for st, want := range problem {
		if (AppRow{State: st}).Problem() != want {
			t.Errorf("state %d problem()=%v, want %v", st, !want, want)
		}
	}
	// The zero value must NOT read as a problem-free "in sync": a bare AppRow{}
	// is unknown, so a forgotten State assignment can't masquerade as healthy.
	if (AppRow{}).StateLabel() == "in sync" {
		t.Fatal("zero-value AppRow must not render as in sync")
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

// --incomplete must carry an app's unfinished-deploy flag all the way onto its
// row, not just issue the extra read.
func TestHostListIncompleteFlag(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps"):
			return transport.Result{Stdout: "b1|blog|web|R2|Up (healthy)\n"}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "blog|releases/R2\n"}, true
		case strings.Contains(cmd, "journal"): // HostIncomplete: blog has a started-but-unfinished deploy
			return transport.Result{Stdout: "@@ob-app@@blog\n@@ob-file@@\n" +
				`{"deploy_id":"B1","event":"start"}` + "\n\n"}, true
		}
		return transport.Result{}, false
	}}
	u := ui.New(&bytes.Buffer{}, false)
	ov, err := HostList(context.Background(), f, u, ListOptions{Incomplete: true})
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := rowByApp(ov, "blog"); !ok || !r.Incomplete {
		t.Fatalf("blog's unfinished deploy must set AppRow.Incomplete: %+v", r)
	}
}

// A managed proxy that is registered (apps behind it) but absent from docker ps
// is DOWN — it must be surfaced (a down shared proxy takes every app offline)
// and must trip --fail-on-drift, not vanish into "no managed proxy".
func TestHostListManagedProxyDown(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps"): // no ob-proxy project present
			return transport.Result{Stdout: "m1|monk|server|R2|Up (healthy)\n"}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "monk|releases/R2\n"}, true
		case strings.Contains(cmd, "proxy/apps"): // monk is registered behind the managed proxy
			return transport.Result{Stdout: "monk\n"}, true
		}
		return transport.Result{}, false
	}}
	u := ui.New(&bytes.Buffer{}, false)
	ov, err := HostList(context.Background(), f, u, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ov.Proxy.Managed || ov.Proxy.Running {
		t.Fatalf("a registered-but-absent proxy must read as managed+down: %+v", ov.Proxy)
	}
	if !ov.HasProblems() {
		t.Fatal("a down managed proxy must make HasProblems true (--fail-on-drift)")
	}
}

// An empty host (no app dirs, no containers) yields no rows and no proxy.
func TestHostListEmptyHost(t *testing.T) {
	f := &transport.Fake{} // every read returns empty, exit 0
	u := ui.New(&bytes.Buffer{}, false)
	ov, err := HostList(context.Background(), f, u, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Apps) != 0 || ov.Foreign != 0 || ov.Proxy.Managed || ov.HasProblems() {
		t.Fatalf("empty host must be empty: %+v", ov)
	}
}

// A suspicious container id from docker ps is rejected (injection guard).
func TestHostListRejectsSuspiciousID(t *testing.T) {
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

// Fail closed, never a false all-clear: a failed docker ps (daemon down) and an
// unreadable release root must error rather than read as "empty host / in sync".
func TestHostListFailsClosedOnReadFailure(t *testing.T) {
	// docker ps exits non-zero (daemon unreachable) — must not read as 0 containers.
	dockerDown := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps") {
			return transport.Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}, true
		}
		return transport.Result{}, false
	}}
	u := ui.New(&bytes.Buffer{}, false)
	if _, err := HostList(context.Background(), dockerDown, u, ListOptions{}); err == nil {
		t.Fatal("a failed docker ps must fail HostList, not report an empty host")
	}

	// The release root is present but unreadable — must not read as "no apps".
	rootUnreadable := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{ExitCode: 17, Stderr: "Permission denied"}, true
		}
		return transport.Result{}, false // docker ps / proxy reads succeed empty
	}}
	if _, err := HostList(context.Background(), rootUnreadable, u, ListOptions{}); err == nil {
		t.Fatal("an unreadable release root must fail HostList, not report a clean empty host")
	}
}

// A genuine transport-level failure (connection reset) surfaces out of HostList,
// distinct from a remote command exiting non-zero.
func TestHostListSurfacesTransportError(t *testing.T) {
	f := &transport.Fake{Err: func(cmd string) error {
		if strings.Contains(cmd, "docker ps") {
			return errors.New("connection reset")
		}
		return nil
	}}
	u := ui.New(&bytes.Buffer{}, false)
	if _, err := HostList(context.Background(), f, u, ListOptions{}); err == nil {
		t.Fatal("a transport error must surface from HostList")
	}
}

// healthSummary collapses replicas worst-first: unhealthy > starting > healthy > none.
func TestHealthSummaryWorstFirst(t *testing.T) {
	cases := []struct {
		health []string
		want   string
	}{
		{nil, "-"},
		{[]string{"none", "none"}, "none"},
		{[]string{"healthy", "none"}, "healthy"},
		{[]string{"starting", "healthy"}, "1 starting"},               // starting outranks healthy
		{[]string{"unhealthy", "starting", "healthy"}, "1 unhealthy"}, // unhealthy outranks all
		{[]string{"unhealthy", "unhealthy"}, "2 unhealthy"},
	}
	for _, c := range cases {
		var cs []svcContainer
		for _, h := range c.health {
			cs = append(cs, svcContainer{health: h})
		}
		if got := healthSummary(cs); got != c.want {
			t.Errorf("healthSummary(%v) = %q, want %q", c.health, got, c.want)
		}
	}
}
