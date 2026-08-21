package app

import (
	"strings"
	"testing"
)

// 8.5 — every name Onebox derives carries the application.
//
// Container, volume and network names are host-global in the container
// runtime. A workload-scoped name such as `web` or `data` can collide with
// something Onebox does not own, and the collision surfaces as a container
// that vanishes or a volume shared between two applications — not as an error.
//
// The transient rollout name is included deliberately: it exists for seconds
// during a handover, which is exactly when nobody is looking at it.
func TestEveryDerivedNameCarriesTheApplication(t *testing.T) {
	n := Names{App: "shop", BasePath: DefaultBasePath}

	for label, got := range map[string]string{
		"container":           n.Container("web", 1),
		"replica container":   n.Container("web", 2),
		"transient rollout":   n.TransientContainer("web"),
		"workload volume":     n.WorkloadVolume("web", "uploads"),
		"service container":   n.ServiceContainer("postgres"),
		"service project":     n.ServiceProject("postgres"),
		"service volume":      n.ServiceVolume("postgres", "data"),
		"service network":     n.ServiceNetwork(),
		"application network": n.ApplicationNetwork(),
		"compose project":     n.ComposeProject(),
		"proxy service":       n.ProxyService("web"),
		"proxy service r1":    n.ProxyServiceFor("web", 1),
		"router":              n.Router("web", 0),
		"application dir":     n.AppDir(),
		"release dir":         n.ReleaseDir("R1"),
	} {
		if !strings.Contains(got, "shop") {
			t.Errorf("%s = %q, which does not carry the application", label, got)
		}
	}

	// And two applications never derive the same name for the same thing.
	other := Names{App: "ledger", BasePath: DefaultBasePath}
	for label, pair := range map[string][2]string{
		"container":           {n.Container("web", 1), other.Container("web", 1)},
		"transient":           {n.TransientContainer("web"), other.TransientContainer("web")},
		"workload volume":     {n.WorkloadVolume("web", "data"), other.WorkloadVolume("web", "data")},
		"service volume":      {n.ServiceVolume("postgres", "data"), other.ServiceVolume("postgres", "data")},
		"service network":     {n.ServiceNetwork(), other.ServiceNetwork()},
		"application network": {n.ApplicationNetwork(), other.ApplicationNetwork()},
		"router":              {n.Router("web", 0), other.Router("web", 0)},
		"application dir":     {n.AppDir(), other.AppDir()},
	} {
		if pair[0] == pair[1] {
			t.Errorf("%s: two applications derive the same name %q", label, pair[0])
		}
	}
}

// 6.3 — a multi-route workload and a non-HTTP route survive the whole path:
// the canonical form describes them, and the generated labels route them.
func TestMultiRouteAndNonHTTPRouteEndToEnd(t *testing.T) {
	body := `api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@203.0.113.10}
workloads:
  web:
    role: application
    image: nginx
    health: /healthz
    routes:
      - {domain: shop.example.com, path: /, port: 3000, middlewares: [compress@file, secure-headers@file]}
      - {domain: shop.example.com, path: /api, port: 3001}
      - {domain: grpc.example.com, port: 9000, entrypoint: grpc, scheme: h2c}
      - {domain: db.example.com, port: 5432, protocol: tcp, tls: passthrough, entrypoint: pg, middlewares: [office-only@file]}
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
	for _, want := range []string{"3000", "3001", "9000", "5432", "h2c", "passthrough", "grpc", "pg", "compress@file", "secure-headers@file", "office-only@file"} {
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
		"traefik.http.services.shop_web.loadbalancer.server.port: \"3000\"",
		"traefik.http.services.shop_web_r1.loadbalancer.server.port: \"3001\"",
		"traefik.http.services.shop_web_r2.loadbalancer.server.port: \"9000\"",
		"traefik.tcp.services.shop_web_r3.loadbalancer.server.port: \"5432\"",
		// Each router names the backend it means.
		"traefik.http.routers.shop_web_r0.service: shop_web",
		"traefik.http.routers.shop_web_r1.service: shop_web_r1",
		// Middleware order is authored behavior, not a set to sort.
		"traefik.http.routers.shop_web_r0.middlewares: compress@file,secure-headers@file",
		// The non-HTTP route is a TCP router matching on SNI, forwarded intact.
		"traefik.tcp.routers.shop_web_r3.rule: HostSNI(`db.example.com`)",
		"traefik.tcp.routers.shop_web_r3.middlewares: office-only@file",
		"traefik.tcp.routers.shop_web_r3.tls.passthrough: \"true\"",
		// And the scheme reaches the backend that needs it.
		"traefik.http.services.shop_web_r2.loadbalancer.server.scheme: h2c",
	} {
		if !strings.Contains(runtime, want) {
			t.Errorf("the generated runtime is missing:\n  %s", want)
		}
	}
	if strings.Contains(runtime, "traefik.http.routers.shop_web_r1.middlewares") {
		t.Fatal("middleware from route zero leaked onto route one")
	}
}

func TestRouteMiddlewareOrderPreservesRepetition(t *testing.T) {
	body := `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@203.0.113.10}}
workloads:
  web:
    image: nginx
    routes:
      - {domain: shop.example.com, port: 3000, middlewares: [prefix@file, auth@file, prefix@file]}
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
	if want := "traefik.http.routers.shop_web_r0.middlewares: prefix@file,auth@file,prefix@file"; !strings.Contains(string(rendered.Bytes), want) {
		t.Fatalf("middleware chain lost its authored order or repetition:\n%s", rendered.Bytes)
	}
}
