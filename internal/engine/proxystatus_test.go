package engine

import (
	"bytes"
	"context"
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
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

func acmeFixture(t *testing.T, domain string, notAfter time.Time) string {
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

// statusProxyEngine: a managed-proxy engine + fake answering every Status
// query. appliedHash/acme parametrize the proxy state on the "host".
func statusProxyEngine(t *testing.T, appliedHash *string, acme string, proxyHealth string) (*Engine, *transport.Fake, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "traefik", "traefik.yml"), []byte("ping: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localHash, err := proxy.Stage(filepath.Join(dir, "traefik"), t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if *appliedHash == "" {
		*appliedHash = localHash // default: in sync
	}
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R7\n"}, true
		case strings.Contains(cmd, "project='onebox-proxy'"): // proxy id + health in one ps
			return transport.Result{Stdout: "PX1|Up 2 days (" + proxyHealth + ")\n"}, true
		case strings.Contains(cmd, "cat '/var/lib/ob/_host/proxy/config.hash'"):
			return transport.Result{Stdout: *appliedHash + "\n"}, true
		case strings.Contains(cmd, "cat '/var/lib/ob/_host/owner'"):
			return transport.Result{Stdout: "sample\n"}, true
		case strings.Contains(cmd, "cat '/var/lib/ob/_host/proxy/acme/acme.json'"):
			return transport.Result{Stdout: acme}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "ob.app='sample'"):
			return transport.Result{Stdout: "S1|web|R7|Up (healthy)\n" +
				"W1|worker|R7|Up (healthy)\nPG1|postgres|R7|Up (healthy)\n"}, true
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal"):
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}
	cfg := testConfig()
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	var out bytes.Buffer
	now := func() time.Time { return time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC) }
	e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep, Now: now, LocalDir: dir})
	return e, f, &out, localHash
}

func TestStatusManagedProxyInSync(t *testing.T) {
	applied := ""
	acme := acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)) // 73d out
	e, f, out, _ := statusProxyEngine(t, &applied, acme, "healthy")
	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("status: %v\n%s\n%s", err, out.String(), strings.Join(f.Commands, "\n"))
	}
	s := out.String()
	if !strings.Contains(s, "proxy") || !strings.Contains(s, "healthy") {
		t.Fatalf("proxy line missing:\n%s", s)
	}
	if !strings.Contains(s, "owner: sample") {
		t.Fatalf("host owner missing:\n%s", s)
	}
	if !strings.Contains(s, "app.example.com") || !strings.Contains(s, "2026-09-15") || !strings.Contains(s, "73d") {
		t.Fatalf("cert expiry missing:\n%s", s)
	}
	if !strings.Contains(s, "all in sync") {
		t.Fatalf("must be in sync:\n%s", s)
	}
}

func TestStatusManagedProxyConfigDrift(t *testing.T) {
	applied := "deadbeefdeadbeef"
	acme := acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	e, _, out, _ := statusProxyEngine(t, &applied, acme, "healthy")
	err := e.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "divergence") {
		t.Fatalf("drifted config must be a divergence, got %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "DRIFTED") {
		t.Fatalf("drift must be reported:\n%s", out.String())
	}
}

func TestStatusManagedProxyCertRenewalOverdue(t *testing.T) {
	applied := ""
	acme := acmeFixture(t, "app.example.com", time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)) // 10d out — lego renews at 30
	e, _, out, _ := statusProxyEngine(t, &applied, acme, "healthy")
	err := e.Status(context.Background())
	if err == nil {
		t.Fatalf("renewal overdue must be a divergence:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "RENEWAL OVERDUE") {
		t.Fatalf("overdue renewal must be flagged:\n%s", out.String())
	}
}

// An up-but-unhealthy proxy (health parsed from ps .Status) must force
// divergence — all other proxy tests only ever pass "healthy".
func TestStatusManagedProxyUnhealthy(t *testing.T) {
	applied := ""
	acme := acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	e, _, out, _ := statusProxyEngine(t, &applied, acme, "unhealthy")
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("an unhealthy proxy must be a divergence:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "unhealthy") {
		t.Fatalf("proxy health must be shown:\n%s", out.String())
	}
}

// proxyContainer parses an id from docker ps; a non-alnum id must be rejected,
// mirroring projectContainers' guard.
func TestProxyContainerRejectsSuspiciousID(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "project='onebox-proxy'") {
			return transport.Result{Stdout: "PX1;rm -rf|Up (healthy)\n"}, true
		}
		return transport.Result{}, false
	}}
	cfg := testConfig()
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if _, _, err := e.proxyContainer(context.Background()); err == nil {
		t.Fatal("a suspicious proxy container id must be rejected")
	}
}

func TestStatusManagedProxyNotRunning(t *testing.T) {
	applied := ""
	e, f, out, _ := statusProxyEngine(t, &applied, "", "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "project='onebox-proxy'") {
			return transport.Result{Stdout: ""}, true
		}
		return base(cmd)
	}
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("absent proxy must be a divergence:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "NOT RUNNING") {
		t.Fatalf("absent proxy must be reported:\n%s", out.String())
	}
}

func TestStatusUnmanagedProxyUnchanged(t *testing.T) {
	// no proxy block: status must not query _host at all
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R7\n"}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "ob.app='sample'"):
			return transport.Result{Stdout: "S1|web|R7|Up (healthy)\n" +
				"W1|worker|R7|Up (healthy)\nPG1|postgres|R7|Up (healthy)\n"}, true
		}
		return transport.Result{}, false
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "_host") {
		t.Fatalf("unmanaged status must not touch _host:\n%s", strings.Join(f.Commands, "\n"))
	}
}
