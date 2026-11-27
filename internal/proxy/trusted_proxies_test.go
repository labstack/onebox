package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"gopkg.in/yaml.v3"
)

// The full static configuration with trusted proxies, byte for byte: the
// forwarded-header block sits inside every entrypoint Onebox renders, built in
// and declared, and nothing else moves.
func TestTrustedProxiesRenderIntoEveryEntrypoint(t *testing.T) {
	entrypoints := map[string]app.ProxyEntrypoint{"otlp-grpc": {Port: 4317}}
	got := string(renderManagedStaticConfig(entrypoints, nil, []string{"192.0.2.0/24", "2001:db8::/32"}))
	const want = `# Written by Onebox because the project declared no custom static proxy configuration.
# Add dynamic files through proxy.config, or include traefik.yml/traefik.yaml to own this file.
ping: {}
providers:
  file:
    directory: /etc/traefik/dynamic
    watch: true
entryPoints:
  web:
    address: ":80"
    forwardedHeaders:
      trustedIPs:
        - "192.0.2.0/24"
        - "2001:db8::/32"
    http:
      redirections:
        entryPoint: {to: websecure, scheme: https}
  websecure:
    address: ":443"
    forwardedHeaders:
      trustedIPs:
        - "192.0.2.0/24"
        - "2001:db8::/32"
    http:
      middlewares:
        - onebox-compress@file
  otlp-grpc:
    address: ":4317"
    forwardedHeaders:
      trustedIPs:
        - "192.0.2.0/24"
        - "2001:db8::/32"
certificatesResolvers:
  letsencrypt:
    acme:
      storage: /letsencrypt/acme.json
      httpChallenge:
        entryPoint: web
`
	if got != want {
		t.Fatalf("static configuration with trusted proxies:\n%s\nwant:\n%s", got, want)
	}
	var document struct {
		EntryPoints map[string]struct {
			ForwardedHeaders struct {
				TrustedIPs []string `yaml:"trustedIPs"`
			} `yaml:"forwardedHeaders"`
		} `yaml:"entryPoints"`
	}
	if err := yaml.Unmarshal([]byte(got), &document); err != nil {
		t.Fatalf("rendered configuration is not YAML: %v", err)
	}
	for _, name := range []string{"web", "websecure", "otlp-grpc"} {
		if ips := document.EntryPoints[name].ForwardedHeaders.TrustedIPs; strings.Join(ips, ",") != "192.0.2.0/24,2001:db8::/32" {
			t.Errorf("entrypoint %s trusts %v", name, ips)
		}
	}
}

// Upgrading Onebox must not restart a proxy whose project does not use the
// field. These digests were taken from the renderer before trusted proxies
// existed; an absent and an empty list must both reproduce them.
func TestStaticConfigWithoutTrustedProxiesIsUnchanged(t *testing.T) {
	entrypoints := map[string]app.ProxyEntrypoint{"otlp-grpc": {Port: 4317}}
	dns := &app.ProxyDNSChallenge{Provider: "cloudflare", Resolvers: []string{"1.1.1.1:53"}}
	for _, trusted := range [][]string{nil, {}} {
		for _, tc := range []struct {
			body []byte
			want string
		}{
			{renderManagedStaticConfig(nil, nil, trusted), "7d792804475ccf729251ca2f3482a7f6143a4d62bcb153c0c280f90164538229"},
			{renderManagedStaticConfig(entrypoints, dns, trusted), "29e77be16f406e93fba34fbdbfd4d582c5c53b6d0e180e747581b6baeb1b6059"},
		} {
			sum := sha256.Sum256(tc.body)
			if got := hex.EncodeToString(sum[:]); got != tc.want {
				t.Errorf("static configuration digest = %s, want %s:\n%s", got, tc.want, tc.body)
			}
		}
		id, err := StageForAppManaged("", t.TempDir(), "", DiscoveryImage("v2026.10.4-alpha"), "sample", "", entrypoints, true, false, nil, trusted)
		if err != nil {
			t.Fatal(err)
		}
		if id.Config != "sha256:5a25af9957faa759c5497d0f6a9b8e5a9f0eaaf297b490f664e2e1c187509a3f" ||
			id.Compose != "sha256:2bb10135d395a17c59be97fdd3c6f4801dba6228cf916dac81dc46a3451560d3" {
			t.Errorf("proxy identity without trusted proxies changed: %+v", id)
		}
	}
	if string(renderStaticConfig(nil)) != DefaultStaticConfig {
		t.Fatal("DefaultStaticConfig no longer matches the renderer")
	}
}

// Declaring trusted proxies changes the configuration digest, which is what
// makes the deploy plan report a Traefik restart, and leaves the container
// definition alone, so the change is a restart and not a recreate.
func TestTrustedProxiesChangeOnlyTheConfigurationIdentity(t *testing.T) {
	for _, cfg := range []map[string]string{nil, {"dynamic.yml": "http: {}\n"}} {
		stage := func(trusted []string) Identity {
			t.Helper()
			dir := ""
			if cfg != nil {
				dir = writeCfg(t, cfg)
			}
			id, err := StageForAppManaged(dir, t.TempDir(), "", "", "sample", "", nil, true, false, nil, trusted)
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		without, with := stage(nil), stage([]string{"192.0.2.0/24"})
		if without.Config == with.Config {
			t.Errorf("config %v: trusted proxies did not change the configuration identity", cfg)
		}
		if without.Compose != with.Compose {
			t.Errorf("config %v: trusted proxies changed the container definition", cfg)
		}
	}
}

func TestTrustedProxiesStageIntoDynamicOnlyConfig(t *testing.T) {
	staging := t.TempDir()
	if _, err := StageForAppManaged(writeCfg(t, map[string]string{"dynamic.yml": "http: {}\n"}), staging, "", "", "sample", "", nil, true, false, nil, []string{"198.51.100.0/24"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(staging, "config", "traefik.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "trustedIPs:\n        - \"198.51.100.0/24\"") {
		t.Fatalf("staged static configuration lost trusted proxies:\n%s", body)
	}
}

func TestTrustedProxiesRefuseProjectOwnedStaticConfig(t *testing.T) {
	for _, name := range []string{"traefik.yml", "traefik.yaml"} {
		cfg := writeCfg(t, map[string]string{name: testSocketlessStatic})
		_, err := StageForAppManaged(cfg, t.TempDir(), "", "", "sample", "", nil, false, false, nil, []string{"192.0.2.0/24"})
		if err == nil || !strings.Contains(err.Error(), "proxy.trustedProxies cannot be combined with project-owned traefik.yml") {
			t.Fatalf("%s: error = %v", name, err)
		}
		if _, err := StageForAppManaged(cfg, t.TempDir(), "", "", "sample", "", nil, false, false, nil, nil); err != nil {
			t.Fatalf("%s without trusted proxies: %v", name, err)
		}
	}
}
