package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCfg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPathsHostScoped(t *testing.T) {
	t.Setenv("YEET_BASE_DIR", "/tmp/yeetbase")
	p := HostPaths()
	if p.Base != "/tmp/yeetbase/_host" {
		t.Fatalf("base: %s", p.Base)
	}
	if p.Compose != "/tmp/yeetbase/_host/proxy/compose.yaml" || p.Apps != "/tmp/yeetbase/_host/proxy/apps" {
		t.Fatalf("paths: %+v", p)
	}
	if p.Lock != "/tmp/yeetbase/_host/lock" || p.Acme != "/tmp/yeetbase/_host/proxy/acme" {
		t.Fatalf("paths: %+v", p)
	}
}

func TestRenderCompose(t *testing.T) {
	b := string(RenderCompose("traefik:v3.7", "yeet-ingress", true))
	for _, want := range []string{
		"container_name: yeet-proxy",
		"image: traefik:v3.7",
		`"80:80"`, `"443:443"`,
		"/var/run/docker.sock:/var/run/docker.sock:ro",
		"./config:/etc/traefik:ro",
		"./acme:/letsencrypt",
		"config/.env",
		`["CMD", "traefik", "healthcheck"]`,
		"name: yeet-ingress",
	} {
		if !strings.Contains(b, want) {
			t.Fatalf("rendered compose missing %q:\n%s", want, b)
		}
	}
	noEnv := string(RenderCompose("traefik:v3.7", "yeet-ingress", false))
	if strings.Contains(noEnv, ".env") {
		t.Fatalf("env_file must be omitted without .env:\n%s", noEnv)
	}
}

func TestStage(t *testing.T) {
	cfgDir := writeCfg(t, map[string]string{
		"traefik.yml": "ping: {}\n",
		"dynamic.yml": "http: {}\n",
		".env":        "CF_DNS_API_TOKEN=x\n",
	})
	staging := t.TempDir()
	hash, err := Stage(cfgDir, staging, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("empty hash")
	}
	// compose rendered with defaults; config copied
	b, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "image: "+DefaultImage) || !strings.Contains(string(b), "name: "+DefaultNetwork) {
		t.Fatalf("defaults not applied:\n%s", b)
	}
	if !strings.Contains(string(b), "config/.env") {
		t.Fatalf(".env present — env_file expected:\n%s", b)
	}
	for _, f := range []string{"config/traefik.yml", "config/dynamic.yml", "config/.env"} {
		if _, err := os.Stat(filepath.Join(staging, f)); err != nil {
			t.Fatalf("staged file %s: %v", f, err)
		}
	}

	// determinism + sensitivity
	staging2 := t.TempDir()
	hash2, err := Stage(cfgDir, staging2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if hash != hash2 {
		t.Fatalf("hash not deterministic: %s vs %s", hash, hash2)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "dynamic.yml"), []byte("http: {middlewares: {}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash3, err := Stage(cfgDir, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if hash3 == hash {
		t.Fatal("hash must change when config content changes")
	}
}

func TestStageRequiresTraefikYML(t *testing.T) {
	cfgDir := writeCfg(t, map[string]string{"dynamic.yml": "http: {}\n"})
	if _, err := Stage(cfgDir, t.TempDir(), "", ""); err == nil || !strings.Contains(err.Error(), "traefik.yml") {
		t.Fatalf("want traefik.yml contract error, got %v", err)
	}
}

func TestStageRejectsSubdirs(t *testing.T) {
	cfgDir := writeCfg(t, map[string]string{"traefik.yml": "ping: {}\n"})
	if err := os.Mkdir(filepath.Join(cfgDir, "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(cfgDir, t.TempDir(), "", ""); err == nil || !strings.Contains(err.Error(), "flat") {
		t.Fatalf("want flat-dir contract error, got %v", err)
	}
}

func acmeJSON(t *testing.T, domain string, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemB := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return fmt.Sprintf(`{"le":{"Certificates":[{"domain":{"main":%q},"certificate":%q}]}}`,
		domain, base64.StdEncoding.EncodeToString(pemB))
}

func TestCertExpiries(t *testing.T) {
	exp := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	certs, err := CertExpiries([]byte(acmeJSON(t, "monk.trade", exp)))
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 1 || certs[0].Domain != "monk.trade" || !certs[0].NotAfter.Equal(exp) {
		t.Fatalf("got %+v", certs)
	}

	// a fresh acme.json is an empty file — no certs, no error
	for _, empty := range []string{"", "{}", `{"le":{"Certificates":null}}`} {
		certs, err := CertExpiries([]byte(empty))
		if err != nil || len(certs) != 0 {
			t.Fatalf("empty store %q: %v %v", empty, certs, err)
		}
	}

	// garbage never breaks status
	if _, err := CertExpiries([]byte("not json")); err == nil {
		t.Fatal("garbage must error (caller reports, never crashes)")
	}
}
