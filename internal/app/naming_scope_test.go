package app

import (
	"strings"
	"testing"
)

// 8.5 — every name derived from the author's workloads carries the
// application; everything Onebox derives for itself is in the onebox namespace.
//
// Container names are host-global in the container runtime. A workload-scoped
// name such as `web-1` can collide with something the operator runs by hand,
// and the collision surfaces as a container that vanishes — not as an error.
// The transient rollout name is included deliberately: it exists for seconds
// during a handover, which is exactly when nobody is looking at it.
//
// Onebox's own containers, volumes, projects, networks, proxy routes and state
// directory do not carry the application. A host has one application, the onebox.app label records which, and
// the host is released only after those resources are removed.
func TestEveryDerivedNameCarriesTheApplication(t *testing.T) {
	n := Names{App: "shop", BasePath: DefaultBasePath}

	for label, got := range map[string]string{
		"container":           n.Container("web", 1),
		"replica container":   n.Container("web", 2),
		"transient rollout":   n.TransientContainer("web"),
		"application network": n.ApplicationNetwork(),
		"compose project":     n.ComposeProject(),
	} {
		if !strings.Contains(got, "shop") {
			t.Errorf("%s = %q, which does not carry the application", label, got)
		}
	}
	for label, got := range map[string]string{
		"service container": n.ServiceContainer("postgres"),
		"restore container": n.BackupRestoreContainer("postgres"),
	} {
		if !strings.HasPrefix(got, Namespace+"-") {
			t.Errorf("%s = %q, which is not in the onebox-* namespace", label, got)
		}
	}
	for label, got := range map[string]string{
		"workload volume":  n.WorkloadVolume("web", "uploads"),
		"service project":  n.ServiceProject("postgres"),
		"service volume":   n.ServiceVolume("postgres", "data"),
		"service network":  n.ServiceNetwork(),
		"restore project":  n.BackupRestoreProject("postgres"),
		"restore network":  n.BackupRestoreNetwork("postgres"),
		"restore volume":   n.BackupRestoreVolume("postgres"),
		"proxy service":    n.ProxyService("web"),
		"proxy service r1": n.ProxyServiceFor("web", 1),
		"router":           n.Router("web", 0),
	} {
		if !strings.HasPrefix(got, Namespace+"_") {
			t.Errorf("%s = %q, which is not in the onebox_ namespace", label, got)
		}
		if strings.Contains(got, "shop") {
			t.Errorf("%s = %q, which carries the application", label, got)
		}
	}
	for label, got := range map[string]string{
		"application dir": n.AppDir(),
		"release dir":     n.ReleaseDir("R1"),
	} {
		if !strings.HasPrefix(got, "/var/lib/onebox/app") {
			t.Errorf("%s = %q, which is not under /var/lib/onebox/app", label, got)
		}
	}
}

// 6.3 — a multi-route workload and a non-HTTP route survive the whole path:
// the canonical form describes them, and the generated labels route them.
func TestMultiRouteAndNonHTTPRouteEndToEnd(t *testing.T) {
	body := `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: shop
spec:
  environments:
    production: {server: root@203.0.113.10}
  workloads:
    web:
      role: Application
      image: nginx
      health: /healthz
      routes:
        - {hostname: shop.example.com, path: /, port: 3000, middlewares: [compress@file, secure-headers@file]}
        - {hostname: shop.example.com, path: /api, port: 3001}
        - {hostname: grpc.example.com, port: 9000, entrypoint: grpc, scheme: h2c}
        - {hostname: db.example.com, port: 5432, protocol: tcp, tls: Passthrough, entrypoint: pg, middlewares: [office-only@file]}
  proxy: {config: traefik}
`
	r, err := loadText(t, body).Resolve("production")
	if err != nil {
		t.Fatal(err)
	}

	canonical, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"3000", "3001", "9000", "5432", "h2c", "Passthrough", "grpc", "pg", "compress@file", "secure-headers@file", "office-only@file"} {
		if !strings.Contains(string(canonical), want) {
			t.Errorf("the canonical form lost %q", want)
		}
	}

	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := string(rendered.Bytes)
	for _, want := range []string{
		// One backend per route, each carrying its own port.
		"traefik.http.services.onebox_web.loadbalancer.server.port: \"3000\"",
		"traefik.http.services.onebox_web_r1.loadbalancer.server.port: \"3001\"",
		"traefik.http.services.onebox_web_r2.loadbalancer.server.port: \"9000\"",
		"traefik.tcp.services.onebox_web_r3.loadbalancer.server.port: \"5432\"",
		// Each router names the backend it means.
		"traefik.http.routers.onebox_web_r0.service: onebox_web",
		"traefik.http.routers.onebox_web_r1.service: onebox_web_r1",
		// Middleware order is authored behavior, not a set to sort.
		"traefik.http.routers.onebox_web_r0.middlewares: compress@file,secure-headers@file",
		// The non-HTTP route is a TCP router matching on SNI, forwarded intact.
		"traefik.tcp.routers.onebox_web_r3.rule: HostSNI(`db.example.com`)",
		"traefik.tcp.routers.onebox_web_r3.middlewares: office-only@file",
		"traefik.tcp.routers.onebox_web_r3.tls.passthrough: \"true\"",
		// And the scheme reaches the backend that needs it.
		"traefik.http.services.onebox_web_r2.loadbalancer.server.scheme: h2c",
	} {
		if !strings.Contains(runtime, want) {
			t.Errorf("the generated runtime is missing:\n  %s", want)
		}
	}
	if strings.Contains(runtime, "traefik.http.routers.onebox_web_r1.middlewares") {
		t.Fatal("middleware from route zero leaked onto route one")
	}
}

func TestRouteMiddlewareOrderPreservesRepetition(t *testing.T) {
	body := `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: shop
spec:
  environments: {production: {server: root@203.0.113.10}}
  workloads:
    web:
      image: nginx
      routes:
        - {hostname: shop.example.com, port: 3000, middlewares: [prefix@file, auth@file, prefix@file]}
  proxy: {managed: false}
`
	r, err := loadText(t, body).Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "traefik.http.routers.onebox_web_r0.middlewares: prefix@file,auth@file,prefix@file"; !strings.Contains(string(rendered.Bytes), want) {
		t.Fatalf("middleware chain lost its authored order or repetition:\n%s", rendered.Bytes)
	}
}
