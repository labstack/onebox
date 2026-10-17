package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

func TestDecideProxy(t *testing.T) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	digestC := "sha256:" + strings.Repeat("c", 64)
	staged := proxy.Identity{Config: digestA, Compose: digestB}
	tests := []struct {
		name   string
		mutate func(*proxyObservation)
		want   proxyAction
	}{
		{"unknown marker", func(o *proxyObservation) { o.AppliedKnown = false }, proxyApply},
		{"config drift", func(o *proxyObservation) { o.Applied.Config = digestC }, proxyApply},
		{"compose drift", func(o *proxyObservation) { o.Applied.Compose = digestC }, proxyConverge},
		{"traefik missing", func(o *proxyObservation) { o.ProxyIDs = nil }, proxyConverge},
		{"discovery missing", func(o *proxyObservation) { o.DiscoveryIDs = nil }, proxyConverge},
		{"output missing", func(o *proxyObservation) { o.OutputInode = "" }, proxyConverge},
		{"all present and equal", func(o *proxyObservation) {}, proxyUnchanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := proxyObservation{Applied: staged, AppliedKnown: true, ProxyIDs: []string{"PX1"}, DiscoveryIDs: []string{"D1"}, OutputInode: "12"}
			tt.mutate(&o)
			if got := decideProxy(o, staged); got != tt.want {
				t.Fatalf("decideProxy = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestObserveProxyIsReadOnly(t *testing.T) {
	f := &transport.Fake{}
	e, staged, _ := proxyFixture(t, f)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.HasPrefix(cmd, "cat "):
			return transport.Result{Stdout: staged.String() + "\n"}, true
		case strings.Contains(cmd, "service=proxy"):
			return transport.Result{Stdout: "PX1\n"}, true
		case strings.Contains(cmd, "service=discovery"):
			return transport.Result{Stdout: "D1\n"}, true
		case strings.Contains(cmd, "stat -c %i"):
			return transport.Result{Stdout: "12\n"}, true
		}
		return transport.Result{}, false
	}
	o, err := e.observeProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !o.AppliedKnown || o.Applied != staged || strings.Join(o.ProxyIDs, ",") != "PX1" || strings.Join(o.DiscoveryIDs, ",") != "D1" || o.OutputInode != "12" {
		t.Fatalf("observation = %+v", o)
	}
	for _, cmd := range f.Commands {
		for _, mutation := range []string{"rm ", "mv ", "mkdir", "up -d", "docker restart", "> "} {
			if strings.Contains(cmd, mutation) {
				t.Fatalf("observe mutated host: %s", cmd)
			}
		}
	}
	if len(f.Commands) != 4 || len(f.Uploads) != 0 {
		t.Fatalf("commands = %v; uploads = %v", f.Commands, f.Uploads)
	}
}

func TestObserveProxyMarker(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, marker := range []string{"", digest, digest + " sha256:abc", "unparseable"} {
		t.Run(marker, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.HasPrefix(cmd, "cat ") {
					return transport.Result{Stdout: marker}, true
				}
				return transport.Result{}, false
			}}
			e, _, _ := proxyFixture(t, f)
			o, err := e.observeProxy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if o.AppliedKnown {
				t.Fatalf("invalid marker accepted: %+v", o)
			}
			if o.OutputInode != "" {
				t.Fatalf("absent output inode = %q", o.OutputInode)
			}
		})
	}
}

func TestProxyActionString(t *testing.T) {
	for action, want := range map[proxyAction]string{
		proxyUnchanged: "unchanged",
		proxyConverge:  "update changed containers only, no restart",
		proxyApply:     "restart Traefik (configuration changed)",
	} {
		if got := action.String(); got != want {
			t.Fatalf("action %d = %q, want %q", action, got, want)
		}
	}
}

func TestOutputInodeCommand(t *testing.T) {
	const path = "/proxy with spaces/onebox.yml"
	want := "if [ -s '/proxy with spaces/onebox.yml' ]; then stat -c %i '/proxy with spaces/onebox.yml'; fi"
	if got := outputInodeCommand(path); got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestStageProxy(t *testing.T) {
	e, want, _ := proxyFixture(t, &transport.Fake{})
	got, err := e.stageProxy(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

func TestPlanProxyReportsEachAction(t *testing.T) {
	for _, tt := range []struct {
		name string
		want string
	}{
		{"unchanged", "unchanged"},
		{"compose drift", "update changed containers only, no restart"},
		{"config drift", "restart Traefik (configuration changed)"},
		{"unmanaged", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &transport.Fake{}
			e, applied, _ := proxyFixture(t, f)
			switch tt.name {
			case "compose drift":
				applied.Compose = "sha256:" + strings.Repeat("c", 64)
			case "config drift":
				applied.Config = "sha256:" + strings.Repeat("c", 64)
			case "unmanaged":
				e.Spec.Proxy.Managed = false
			}
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch {
				case strings.HasPrefix(cmd, "cat "):
					return transport.Result{Stdout: applied.String() + "\n"}, true
				case strings.Contains(cmd, "service=proxy"):
					return transport.Result{Stdout: "PX1\n"}, true
				case strings.Contains(cmd, "service=discovery"):
					return transport.Result{Stdout: "D1\n"}, true
				case strings.Contains(cmd, "stat -c %i"):
					return transport.Result{Stdout: "12\n"}, true
				}
				return transport.Result{}, false
			}
			got, err := e.PlanProxy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("PlanProxy = %q, want %q", got, tt.want)
			}
			for _, cmd := range f.Commands {
				for _, mutation := range []string{"rm ", "mv ", "mkdir", "up -d", "docker restart", "> "} {
					if strings.Contains(cmd, mutation) {
						t.Fatalf("plan mutated host: %s", cmd)
					}
				}
			}
			wantCommands := 4
			if tt.name == "unmanaged" {
				wantCommands = 0
			}
			if len(f.Commands) != wantCommands || len(f.Uploads) != 0 {
				t.Fatalf("commands = %v; uploads = %v", f.Commands, f.Uploads)
			}
		})
	}
}
