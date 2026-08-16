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

	"github.com/labstack/onebox/internal/app"
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
	// The base comes from the project's resolved names, so an app declaring
	// base_path puts the host proxy beside its own state rather than in a
	// second tree nothing else reads.
	p := HostPaths(app.Names{App: "sample", BasePath: "/tmp/obbase"})
	if p.Base != "/tmp/obbase/_host" {
		t.Fatalf("base: %s", p.Base)
	}
	if p.Compose != "/tmp/obbase/_host/proxy/compose.yaml" || p.Owner != "/tmp/obbase/_host/owner" {
		t.Fatalf("paths: %+v", p)
	}
	if p.Lock != "/tmp/obbase/_host/lock" || p.Acme != "/tmp/obbase/_host/proxy/acme" {
		t.Fatalf("paths: %+v", p)
	}
}

func TestRenderCompose(t *testing.T) {
	b := string(RenderCompose("traefik:v3.7", "ob-ingress", true))
	for _, want := range []string{
		"container_name: ob-proxy",
		"image: traefik:v3.7",
		`"80:80"`, `"443:443"`,
		"/var/run/docker.sock:/var/run/docker.sock:ro",
		"./config:/etc/traefik:ro",
		"./acme:/letsencrypt",
		"config/.env",
		`["CMD", "traefik", "healthcheck"]`,
		"name: ob-ingress",
	} {
		if !strings.Contains(b, want) {
			t.Fatalf("rendered compose missing %q:\n%s", want, b)
		}
	}
	noEnv := string(RenderCompose("traefik:v3.7", "ob-ingress", false))
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

func TestStageRequiresTraefikConfig(t *testing.T) {
	cfgDir := writeCfg(t, map[string]string{"dynamic.yml": "http: {}\n"})
	if _, err := Stage(cfgDir, t.TempDir(), "", ""); err == nil ||
		!strings.Contains(err.Error(), "traefik.yml") || !strings.Contains(err.Error(), "traefik.yaml") {
		t.Fatalf("want both supported static config names in the contract error, got %v", err)
	}
}

func TestStageAcceptsTraefikYAML(t *testing.T) {
	cfgDir := writeCfg(t, map[string]string{"traefik.yaml": "ping: {}\n"})
	staging := t.TempDir()
	if _, err := Stage(cfgDir, staging, "", ""); err != nil {
		t.Fatalf("traefik.yaml must be accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "config", "traefik.yaml")); err != nil {
		t.Fatalf("traefik.yaml was not staged: %v", err)
	}
}

func TestStageRejectsAmbiguousTraefikConfig(t *testing.T) {
	cfgDir := writeCfg(t, map[string]string{
		"traefik.yml":  "ping: {}\n",
		"traefik.yaml": "ping: {}\n",
	})
	if _, err := Stage(cfgDir, t.TempDir(), "", ""); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("two static config files must be refused as ambiguous: %v", err)
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
	certs, err := CertExpiries([]byte(acmeJSON(t, "app.example.com", exp)))
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 1 || certs[0].Domain != "app.example.com" || !certs[0].NotAfter.Equal(exp) {
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

// A project that declares a domain and nothing else must be able to bootstrap.
// Onebox owns the proxy; requiring the author to write Traefik's static
// configuration first contradicts that, and the file they would write is the
// same one every time.
func TestDefaultStaticConfigIsWrittenWhenNoneIsDeclared(t *testing.T) {
	staging := t.TempDir()
	hash, err := Stage("", staging, "traefik:v3.7", "ob-ingress")
	if err != nil {
		t.Fatalf("a project without proxy.config must still bootstrap: %v", err)
	}
	if hash == "" {
		t.Fatal("the written configuration must have an identity to compare against")
	}
	body, err := os.ReadFile(filepath.Join(staging, "config", "traefik.yml"))
	if err != nil {
		t.Fatalf("no static configuration was written: %v", err)
	}
	for _, want := range []string{"ping: {}", "/letsencrypt/acme.json", "exposedByDefault: false"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the default configuration is missing %q:\n%s", want, body)
		}
	}
}

// A declared directory still owns the configuration entirely, and one missing
// either supported static file says what to do about it.
func TestDeclaredConfigStillOwnsItAndSaysWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other.yml"), []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Stage(dir, t.TempDir(), "traefik:v3.7", "ob-ingress")
	if err == nil {
		t.Fatal("a declared config directory without traefik.yml or traefik.yaml must be refused")
	}
	if !strings.Contains(err.Error(), "Remove proxy.config") {
		t.Errorf("the refusal must say how to resolve it: %v", err)
	}
}
