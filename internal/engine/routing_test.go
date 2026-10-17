package engine

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/discovery"
	"github.com/labstack/onebox/internal/transport"
	"gopkg.in/yaml.v3"
)

func routingDocument(t *testing.T, body string) discovery.Dynamic {
	t.Helper()
	var doc discovery.Dynamic
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestRoutedInRequiresEveryExpectedService(t *testing.T) {
	role := app.Workload{Routes: []app.Route{{Port: 3000}, {Protocol: "tcp", Port: 5432}}}
	doc := routingDocument(t, "http:\n  services:\n    onebox_web:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.5:3000\n")
	if routedIn(doc, app.Names{}, "web", role, "10.0.0.5") {
		t.Fatal("accepted missing TCP service")
	}
	doc.TCP = &discovery.TCP{Services: map[string]discovery.TCPService{"onebox_web_r1": {LoadBalancer: discovery.TCPLoadBalancer{Servers: []discovery.TCPServer{{Address: "10.0.0.5:5432"}}}}}}
	if !routedIn(doc, app.Names{}, "web", role, "10.0.0.5") {
		t.Fatal("rejected complete routing")
	}
	if routedIn(doc, app.Names{}, "other", role, "10.0.0.5") {
		t.Fatal("matched unrelated workload")
	}
}

func TestRoutedInMatchesIPv6(t *testing.T) {
	doc := routingDocument(t, "http:\n  services:\n    onebox_web:\n      loadBalancer:\n        servers:\n          - url: http://[fd00::5]:3000\n")
	role := app.Workload{Routes: []app.Route{{Port: 3000}}}
	if !routedIn(doc, app.Names{}, "web", role, "fd00:0:0:0:0:0:0:5") {
		t.Fatal("did not match IPv6")
	}
}

func TestWithdrawnFromEveryService(t *testing.T) {
	role := app.Workload{Routes: []app.Route{{Port: 3000}, {Protocol: "tcp", Port: 5432}}}
	doc := routingDocument(t, "tcp:\n  services:\n    unrelated:\n      loadBalancer:\n        servers:\n          - address: '[fd00::5]:5432'\n")
	if !withdrawnFrom(doc, app.Names{}, "web", role, "fd00::5") {
		t.Fatal("unrelated service blocked withdrawal")
	}
	doc.TCP.Services["onebox_web_r1"] = doc.TCP.Services["unrelated"]
	if withdrawnFrom(doc, app.Names{}, "web", role, "fd00::5") {
		t.Fatal("accepted address still in TCP service")
	}
}

func TestRouteGatedOnlyForManagedProxyWithRoutes(t *testing.T) {
	spec := &app.Resolved{Spec: &app.Spec{}}
	e := New(spec, nil, &transport.Fake{}, Options{Out: io.Discard})
	role := app.Workload{Routes: []app.Route{{Port: 3000}}}
	if e.routeGated(role) {
		t.Fatal("gated unmanaged proxy")
	}
	spec.Proxy.Managed = true
	if !e.routeGated(role) {
		t.Fatal("did not gate managed routes")
	}
	if e.routeGated(app.Workload{}) {
		t.Fatal("gated workload without routes")
	}
}

func TestAwaitRoutingTreatsReadFailureAsNeither(t *testing.T) {
	for _, routed := range []bool{true, false} {
		for _, body := range []string{"failed", "http: [", ""} {
			clock := &virtualClock{}
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.HasPrefix(cmd, "docker inspect") {
					return transport.Result{Stdout: "10.0.0.5 fd00::5"}, true
				}
				if strings.HasPrefix(cmd, "cat ") {
					if body == "failed" {
						return transport.Result{ExitCode: 1, Stderr: "permission denied"}, true
					}
					return transport.Result{Stdout: body}, true
				}
				return transport.Result{}, false
			}}
			e := New(&app.Resolved{Spec: &app.Spec{}}, nil, f, Options{Out: io.Discard, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
			err := e.awaitRouting(context.Background(), "web", app.Workload{Routes: []app.Route{{Port: 3000}}}, "container", routed, time.Second)
			if err == nil {
				t.Fatalf("routed=%v body=%q accepted read failure", routed, body)
			}
			for _, want := range map[bool][]string{true: {"did not publish", "docker logs onebox-discovery"}, false: {"still routes", "ob resume"}}[routed] {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q missing %q", err, want)
				}
			}
			if body == "failed" && !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("lost last read error: %v", err)
			}
		}
	}
}

func TestRouteAddressUsesProxyNetwork(t *testing.T) {
	for _, tc := range []struct{ output, want string }{{"10.0.0.5 fd00::5\n", "10.0.0.5"}, {" fd00::5\n", "fd00::5"}, {"", ""}} {
		f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
			if !strings.Contains(cmd, `.NetworkSettings.Networks "ingress"`) {
				t.Fatalf("unexpected inspect: %s", cmd)
			}
			return transport.Result{Stdout: tc.output}, true
		}}
		spec := &app.Resolved{Spec: &app.Spec{}}
		spec.Proxy.Network = "ingress"
		e := New(spec, nil, f, Options{Out: io.Discard})
		got, err := e.routeAddress(context.Background(), "container")
		if err != nil || got != tc.want {
			t.Fatalf("address = %q, %v; want %q", got, err, tc.want)
		}
		if tc.want == "" {
			err = e.awaitRouting(context.Background(), "web", app.Workload{Routes: []app.Route{{Port: 3000}}}, "container", true, time.Second)
			if err == nil || !strings.Contains(err.Error(), "ingress") {
				t.Fatalf("missing network error: %v", err)
			}
		} else if err != nil || got != tc.want {
			t.Fatalf("address = %q, %v; want %q", got, err, tc.want)
		}
	}
}

func TestAwaitRoutingWaitsForPublishedChange(t *testing.T) {
	for _, routed := range []bool{true, false} {
		clock := &virtualClock{}
		reads, inspections := 0, 0
		f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
			if strings.HasPrefix(cmd, "docker inspect") {
				inspections++
				return transport.Result{Stdout: "10.0.0.5 "}, true
			}
			reads++
			present := reads == 1
			if routed {
				present = !present
			}
			body := "{}"
			if present {
				body = "http:\n  services:\n    onebox_web:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.5:3000\n"
			}
			return transport.Result{Stdout: body}, true
		}}
		e := New(&app.Resolved{Spec: &app.Spec{}}, nil, f, Options{Out: io.Discard, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
		if err := e.awaitRouting(context.Background(), "web", app.Workload{Routes: []app.Route{{Port: 3000}}}, "container", routed, time.Second); err != nil {
			t.Fatal(err)
		}
		if reads != 2 || inspections != 1 {
			t.Fatalf("reads=%d inspections=%d", reads, inspections)
		}
	}
}

func TestAwaitRoutingRejectsCancelledRead(t *testing.T) {
	for _, routed := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
			if strings.HasPrefix(cmd, "docker inspect") {
				return transport.Result{Stdout: "10.0.0.5 "}, true
			}
			cancel()
			return transport.Result{Stdout: "{}"}, true
		}}
		e := New(&app.Resolved{Spec: &app.Spec{}}, nil, f, Options{Out: io.Discard})
		if err := e.awaitRouting(ctx, "web", app.Workload{Routes: []app.Route{{Port: 3000}}}, "container", routed, time.Second); err == nil {
			t.Fatal("accepted cancelled read")
		}
	}
}

func TestAwaitRoutingRejectsIncompleteYAMLStream(t *testing.T) {
	present := "http:\n  services:\n    onebox_web:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.5:3000\n"
	for _, mode := range []struct {
		name   string
		routed bool
	}{{"publish", true}, {"withdraw", false}} {
		for _, tc := range []struct{ name, body string }{
			{"explicit empty", "---\n"},
			{"malformed second document", "{}\n---\nhttp: ["},
			{"second document", "{}\n---\n{}\n"},
			{"published then malformed", present + "---\nhttp: ["},
			{"published then second document", present + "---\n{}\n"},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				clock := &virtualClock{}
				f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
					if strings.HasPrefix(cmd, "docker inspect") {
						return transport.Result{Stdout: "10.0.0.5 "}, true
					}
					return transport.Result{Stdout: tc.body}, true
				}}
				e := New(&app.Resolved{Spec: &app.Spec{}}, nil, f, Options{Out: io.Discard, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
				err := e.awaitRouting(context.Background(), "web", app.Workload{Routes: []app.Route{{Port: 3000}}}, "container", mode.routed, time.Second)
				if err == nil {
					t.Fatal("accepted incomplete YAML stream")
				}
				if !strings.Contains(err.Error(), "read published routes") {
					t.Fatalf("missing read error: %v", err)
				}
			})
		}
	}
}
