package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func routedContainer(id string, created time.Time, health, ip string, labels map[string]string) Container {
	base := map[string]string{"traefik.enable": "true"}
	for key, value := range labels {
		base[key] = value
	}
	return Container{
		ID: id, Created: created, Running: true, Health: health,
		Labels: base, Networks: map[string]string{"ob-ingress": ip},
	}
}

func TestBuildPreservesHealthAwareHTTPRouting(t *testing.T) {
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	labels := map[string]string{
		"traefik.http.routers.shop_web_r0.rule":                     "Host(`shop.example.com`)",
		"traefik.http.routers.shop_web_r0.entrypoints":              "websecure",
		"traefik.http.routers.shop_web_r0.middlewares":              "compress@file,secure@file",
		"traefik.http.routers.shop_web_r0.tls":                      "true",
		"traefik.http.routers.shop_web_r0.tls.certresolver":         "letsencrypt",
		"traefik.http.routers.shop_web_r0.service":                  "shop_web",
		"traefik.http.services.shop_web.loadbalancer.server.port":   "3000",
		"traefik.http.services.shop_web.loadbalancer.server.scheme": "h2c",
	}
	containers := []Container{
		routedContainer("healthy", old, "healthy", "172.20.0.2", labels),
		routedContainer("no-health", old, "", "172.20.0.3", labels),
		routedContainer("starting", old, "starting", "172.20.0.4", labels),
		routedContainer("unhealthy", old, "unhealthy", "172.20.0.5", labels),
	}
	document, err := Build(containers, "ob-ingress")
	if err != nil {
		t.Fatal(err)
	}
	router := document.HTTP.Routers["shop_web_r0"]
	if router.Rule != "Host(`shop.example.com`)" || router.Service != "shop_web" {
		t.Fatalf("router = %+v", router)
	}
	if strings.Join(router.Middlewares, ",") != "compress@file,secure@file" || router.TLS.CertResolver != "letsencrypt" {
		t.Fatalf("router middleware/tls = %+v", router)
	}
	servers := document.HTTP.Services["shop_web"].LoadBalancer.Servers
	if len(servers) != 2 || servers[0].URL != "h2c://172.20.0.2:3000" || servers[1].URL != "h2c://172.20.0.3:3000" {
		t.Fatalf("eligible servers = %+v", servers)
	}
}

func TestBuildUsesNewestHealthyRouterDuringRollAndRollback(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	labels := func(domain string) map[string]string {
		return map[string]string{
			"traefik.http.routers.app_web_r0.rule":                   "Host(`" + domain + "`)",
			"traefik.http.routers.app_web_r0.entrypoints":            "websecure",
			"traefik.http.routers.app_web_r0.service":                "app_web",
			"traefik.http.services.app_web.loadbalancer.server.port": "8080",
		}
	}
	document, err := Build([]Container{
		routedContainer("old-release", base, "healthy", "172.20.0.2", labels("old.example.com")),
		routedContainer("new-container", base.Add(time.Minute), "healthy", "172.20.0.3", labels("new.example.com")),
	}, "ob-ingress")
	if err != nil {
		t.Fatal(err)
	}
	if got := document.HTTP.Routers["app_web_r0"].Rule; got != "Host(`new.example.com`)" {
		t.Fatalf("newest healthy route must win during a transition: %s", got)
	}
}

func TestBuildTCPPassthrough(t *testing.T) {
	document, err := Build([]Container{routedContainer("tcp", time.Now(), "healthy", "2001:db8::5", map[string]string{
		"traefik.tcp.routers.app_db_r0.rule":                   "HostSNI(`db.example.com`)",
		"traefik.tcp.routers.app_db_r0.entrypoints":            "postgres",
		"traefik.tcp.routers.app_db_r0.service":                "app_db",
		"traefik.tcp.routers.app_db_r0.tls":                    "true",
		"traefik.tcp.routers.app_db_r0.tls.passthrough":        "true",
		"traefik.tcp.routers.app_db_r0.tls.certresolver":       "letsencrypt",
		"traefik.tcp.services.app_db.loadbalancer.server.port": "5432",
	})}, "ob-ingress")
	if err != nil {
		t.Fatal(err)
	}
	if !document.TCP.Routers["app_db_r0"].TLS.Passthrough {
		t.Fatal("TCP passthrough was lost")
	}
	if document.TCP.Routers["app_db_r0"].TLS.CertResolver != "letsencrypt" {
		t.Fatal("TCP certificate resolver was lost")
	}
	if got := document.TCP.Services["app_db"].LoadBalancer.Servers[0].Address; got != "[2001:db8::5]:5432" {
		t.Fatalf("IPv6 backend = %s", got)
	}
}

func TestBuildRejectsInvalidGeneratedBackend(t *testing.T) {
	_, err := Build([]Container{routedContainer("bad", time.Now(), "healthy", "172.20.0.2", map[string]string{
		"traefik.http.routers.app_web_r0.rule":                   "Host(`app.example.com`)",
		"traefik.http.routers.app_web_r0.service":                "app_web",
		"traefik.http.services.app_web.loadbalancer.server.port": "root",
	})}, "ob-ingress")
	if err == nil || !strings.Contains(err.Error(), "invalid backend port") {
		t.Fatalf("invalid port error = %v", err)
	}
}

func TestWriteAtomicProducesTraefikYAML(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "onebox.yml")
	document := Dynamic{HTTP: &HTTP{Routers: map[string]HTTPRouter{
		"app": {Rule: "Host(`app.example.com`)", Service: "app"},
	}}}
	if err := WriteAtomic(filename, document); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Dynamic
	if err := yaml.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("generated YAML is invalid: %v\n%s", err, body)
	}
	if decoded.HTTP.Routers["app"].Service != "app" {
		t.Fatalf("decoded document = %+v", decoded)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".onebox-routes-*")); len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}
