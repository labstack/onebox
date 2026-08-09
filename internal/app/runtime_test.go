package app

import (
	"testing"
	"time"
)

// A project that declares its dependencies once should not have to restate
// them as a deploy order. Release order comes from `needs` when the author
// left deployment.order out.
func TestReleaseOrderFollowsNeeds(t *testing.T) {
	p := &Spec{Workloads: map[string]Workload{
		"web":    {Role: RoleApplication, Needs: []Need{{Name: "api"}}},
		"api":    {Role: RoleApplication, Needs: []Need{{Name: "cache"}}},
		"cache":  {Role: RoleDaemon},
		"import": {Role: RoleJob},
	}}
	got := p.ReleaseOrder()
	want := []string{"cache", "api", "web"}
	if len(got) != len(want) {
		t.Fatalf("release order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("release order = %v, want %v", got, want)
		}
	}
	if jobs := p.JobOrder(); len(jobs) != 1 || jobs[0] != "import" {
		t.Errorf("jobs = %v, want [import]; a job must never be in the release order", jobs)
	}
}

// An explicit order wins, and a workload the author forgot to list still gets
// released — silently dropping it would take a workload out of production
// because of a typo in an ordering hint.
func TestReleaseOrderHonoursExplicitAndKeepsOmissions(t *testing.T) {
	p := &Spec{
		Deployment: Deployment{Order: []string{"web", "api"}},
		Workloads: map[string]Workload{
			"web": {Role: RoleApplication}, "api": {Role: RoleApplication},
			"cache": {Role: RoleDaemon},
		},
	}
	got := p.ReleaseOrder()
	if len(got) != 3 || got[0] != "web" || got[1] != "api" || got[2] != "cache" {
		t.Fatalf("release order = %v, want [web api cache]", got)
	}
}

func TestDrainWaitDerivesFromHealthTiming(t *testing.T) {
	w := Workload{Role: RoleApplication, Health: &Health{Interval: "1s", Retries: 4}}
	if got := w.DrainWait(); got != 4*time.Second {
		t.Errorf("drain wait = %v, want 4s (retries × interval)", got)
	}
	w.Drain = &Drain{Wait: "9s"}
	if got := w.DrainWait(); got != 9*time.Second {
		t.Errorf("drain wait = %v, want the declared 9s", got)
	}
}

// Truncating a sub-second grace to zero would turn a graceful stop into an
// immediate kill.
func TestStopGraceRoundsUp(t *testing.T) {
	w := Workload{Drain: &Drain{Grace: "500ms"}}
	if got := w.StopGraceSeconds(); got != 1 {
		t.Errorf("stop grace = %d, want 1", got)
	}
	if got := (Workload{}).StopGraceSeconds(); got != 30 {
		t.Errorf("default stop grace = %d, want 30", got)
	}
}

func TestParseDurationAcceptsDays(t *testing.T) {
	d, ok := ParseDuration("7d")
	if !ok || d != 7*24*time.Hour {
		t.Errorf("7d = %v %v, want 168h", d, ok)
	}
	if _, ok := ParseDuration("soon"); ok {
		t.Error("nonsense must not parse")
	}
}

// The SSH address is derived in one place, and it carries the port.
//
// A declared port that goes missing does not fail — it connects to 22 and
// succeeds against whatever is there, which is the worst shape a bug of this
// kind can take.
func TestTargetCarriesTheDeclaredPort(t *testing.T) {
	for name, tc := range map[string]struct {
		server Server
		want   string
	}{
		"user and port":  {Server{User: "deploy", Host: "example.com", Port: 2222}, "deploy@example.com:2222"},
		"port only":      {Server{Host: "example.com", Port: 2222}, "example.com:2222"},
		"no port":        {Server{User: "root", Host: "example.com"}, "root@example.com"},
		"ipv6 with port": {Server{User: "root", Host: "2a01:4ff::1", Port: 2222}, "root@[2a01:4ff::1]:2222"},
		"ipv6 no port":   {Server{User: "root", Host: "2a01:4ff::1"}, "root@2a01:4ff::1"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := (Environment{Server: tc.server}).Destination(); got != tc.want {
				t.Errorf("Target() = %q, want %q", got, tc.want)
			}
		})
	}
}
