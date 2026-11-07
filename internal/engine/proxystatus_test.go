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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

func acmeFixture(t *testing.T, domain string, notAfter time.Time) string {
	t.Helper()
	return acmeStoreFixture(map[string][]string{
		app.ManagedCertificateResolver: {acmeCertificate(t, domain, notAfter)},
	})
}

// acmeStoreFixture builds a store from resolver keys and their certificate
// entries, as written by acmeCertificate.
func acmeStoreFixture(resolvers map[string][]string) string {
	var body strings.Builder
	body.WriteString("{")
	first := true
	for name, certs := range resolvers {
		if !first {
			body.WriteString(",")
		}
		first = false
		fmt.Fprintf(&body, `%q:{"Certificates":[%s]}`, name, strings.Join(certs, ","))
	}
	body.WriteString("}")
	return body.String()
}

// acmeCertificate is one entry of a resolver's Certificates list.
func acmeCertificate(t *testing.T, domain string, notAfter time.Time) string {
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
	return fmt.Sprintf(`{"domain":{"main":%q},"certificate":%q}`,
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
	if err := os.WriteFile(filepath.Join(dir, "traefik", "traefik.yml"), []byte(testManagedProxyStatic), 0o600); err != nil {
		t.Fatal(err)
	}
	localHash, err := proxy.StageForApp(filepath.Join(dir, "traefik"), t.TempDir(), "",
		proxy.DiscoveryImage("dev"), "sample", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if *appliedHash == "" {
		*appliedHash = localHash.String() // default: in sync
	}
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R7\n"}, true
		case strings.Contains(cmd, "com.docker.compose.service=discovery"):
			return transport.Result{Stdout: "PD1\n"}, true
		case strings.Contains(cmd, "project='onebox-proxy'"): // proxy id + health in one ps
			return transport.Result{Stdout: "PX1|Up 2 days (" + proxyHealth + ")\n"}, true
		case strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/config.hash'"):
			return transport.Result{Stdout: *appliedHash + "\n"}, true
		case strings.Contains(cmd, "cat '/var/lib/onebox/_host/owner'"):
			return transport.Result{Stdout: "sample production\n"}, true
		case strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/acme/acme.json'"):
			return transport.Result{Stdout: acme}, true
		case strings.Contains(cmd, "cat '/var/lib/onebox/_host/proxy/config/traefik.yml'"):
			return transport.Result{Stdout: testManagedProxyStatic}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "onebox.app='sample'"):
			return transport.Result{Stdout: "S1|web|R7|Up (healthy)\n" +
				"W1|worker|R7|Up (healthy)\nPG1|postgres|R7|Up (healthy)\n"}, true
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/onebox/app/journal"):
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}
	cfg := testConfig()
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	var out bytes.Buffer
	now := func() time.Time { return time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC) }
	e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep, Now: now, LocalDir: dir})
	return e, f, &out, localHash.String()
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

func TestStatusAggregatesExactAndWildcardCertificateStores(t *testing.T) {
	applied := ""
	exact := acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	wildcard := acmeFixture(t, "*.preview.example.com", time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	e, f, out, _ := statusProxyEngine(t, &applied, exact, "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "acme-wildcard.json") {
			return transport.Result{Stdout: wildcard}, true
		}
		return base(cmd)
	}

	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	for _, domain := range []string{"app.example.com", "*.preview.example.com"} {
		if !strings.Contains(out.String(), domain) {
			t.Errorf("certificate from managed store %q missing:\n%s", domain, out.String())
		}
	}
}

func TestStatusManagedProxyEnvironmentQualifiedOwner(t *testing.T) {
	applied := ""
	acme := acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	e, f, out, _ := statusProxyEngine(t, &applied, acme, "healthy")
	e.Opts.Environment = "production"
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "_host/owner") {
			return transport.Result{Stdout: "sample production\n"}, true
		}
		return base(cmd)
	}

	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("status rejected an environment-qualified owner: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "owner: sample/production") {
		t.Fatalf("qualified host owner missing:\n%s", out.String())
	}
}

func TestStatusManagedProxyEnvironmentMismatch(t *testing.T) {
	applied := ""
	acme := acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	e, f, out, _ := statusProxyEngine(t, &applied, acme, "healthy")
	e.Opts.Environment = "production"
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "_host/owner") {
			return transport.Result{Stdout: "sample staging\n"}, true
		}
		return base(cmd)
	}

	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("status accepted another environment's host:\n%s", out.String())
	}
	for _, want := range []string{"staging environment", "status targets production"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("environment mismatch did not mention %q:\n%s", want, out.String())
		}
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

// staleResolverStore is the store from issue #235: a resolver key left behind
// by an older proxy configuration still holds a certificate for a domain that
// the configured resolver also covers. Nothing renews the leftover, so it sits
// inside the renewal floor while the served certificate has plenty of runway.
func staleResolverStore(t *testing.T) string {
	t.Helper()
	return acmeStoreFixture(map[string][]string{
		"le":                           {acmeCertificate(t, "app.example.com", time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))}, // 7d out
		app.ManagedCertificateResolver: {acmeCertificate(t, "app.example.com", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))}, // 78d out
	})
}

func TestStatusIgnoresCertificateFromUnconfiguredResolver(t *testing.T) {
	applied := ""
	e, _, out, _ := statusProxyEngine(t, &applied, staleResolverStore(t), "healthy")
	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("an orphaned resolver's certificate must not make status diverge: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "RENEWAL OVERDUE") {
		t.Fatalf("orphaned certificate must not be flagged overdue:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `the applied configuration has no resolver "le" writing this store`) || !strings.Contains(out.String(), "/var/lib/onebox/_host/proxy/acme/acme.json") {
		t.Fatalf("orphaned certificate must name its resolver and store:\n%s", out.String())
	}
}

// A newer certificate under a leftover resolver is not served, so it must not
// hide a managed resolver whose renewal is failing.
func TestStatusFlagsManagedCertificateEvenWhenOrphanedOneIsNewer(t *testing.T) {
	applied := ""
	acme := acmeStoreFixture(map[string][]string{
		app.ManagedCertificateResolver: {acmeCertificate(t, "app.example.com", time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC))}, // 10d out
		"le":                           {acmeCertificate(t, "app.example.com", time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))},  // 60d out
	})
	e, _, out, _ := statusProxyEngine(t, &applied, acme, "healthy")
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("the managed certificate is overdue, so status must diverge:\n%s", out.String())
	}
	if n := strings.Count(out.String(), "RENEWAL OVERDUE"); n != 1 {
		t.Fatalf("exactly the managed certificate must be flagged; got %d:\n%s", n, out.String())
	}
}

// A host that once served wildcard routes keeps their certificate after the
// routes and the DNS challenge are removed. The static configuration no longer
// defines the wildcard resolver, so nothing renews that certificate.
func TestStatusIgnoresWildcardCertificateAfterResolverIsRemoved(t *testing.T) {
	applied := ""
	wildcard := acmeStoreFixture(map[string][]string{
		app.ManagedWildcardCertificateResolver: {acmeCertificate(t, "*.preview.example.com", time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))},
	})
	e, f, out, _ := statusProxyEngine(t, &applied, acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)), "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "acme-wildcard.json") {
			return transport.Result{Stdout: wildcard}, true
		}
		return base(cmd)
	}
	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("an unconfigured wildcard resolver's certificate must not make status diverge: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "RENEWAL OVERDUE") {
		t.Fatalf("orphaned wildcard certificate must not be flagged overdue:\n%s", out.String())
	}
}

// A configured resolver renews only the store its acme.storage names. The
// same key in the other store file is a leftover.
func TestStatusIgnoresConfiguredResolverKeyInOtherStore(t *testing.T) {
	applied := ""
	misplaced := acmeFixture(t, "app.example.com", time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))
	e, f, out, _ := statusProxyEngine(t, &applied, acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)), "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "acme-wildcard.json") {
			return transport.Result{Stdout: misplaced}, true
		}
		return base(cmd)
	}
	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("a resolver key in a store it does not write must not make status diverge: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "/var/lib/onebox/_host/proxy/acme/acme-wildcard.json") {
		t.Fatalf("orphaned certificate must name the store it sits in:\n%s", out.String())
	}
}

// withHostCommand answers one more host command ahead of the fixture's own.
func withHostCommand(f *transport.Fake, match string, result transport.Result) {
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, match) {
			return result, true
		}
		return base(cmd)
	}
}

// The host still runs a configuration whose wildcard resolver the local
// project has since removed. That resolver still renews on the host, so its
// failing renewal must be reported, not called a leftover to delete.
func TestStatusJudgesByAppliedConfigNotLocalConfig(t *testing.T) {
	applied := ""
	e, f, out, _ := statusProxyEngine(t, &applied, acmeFixture(t, "app.example.com", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)), "healthy")
	withHostCommand(f, "cat '/var/lib/onebox/_host/proxy/config/traefik.yml'", transport.Result{
		Stdout: testManagedProxyStatic + "  " + app.ManagedWildcardCertificateResolver + ":\n    acme:\n      storage: /letsencrypt/acme-wildcard.json\n      dnsChallenge:\n        provider: cloudflare\n",
	})
	withHostCommand(f, "acme-wildcard.json", transport.Result{Stdout: acmeStoreFixture(map[string][]string{
		app.ManagedWildcardCertificateResolver: {acmeCertificate(t, "*.preview.example.com", time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))},
	})})
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("the applied wildcard resolver's certificate is overdue, so status must diverge:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "RENEWAL OVERDUE") || strings.Contains(out.String(), "remove the key") {
		t.Fatalf("a resolver the host applied must be judged, never offered for removal:\n%s", out.String())
	}
}

// A resolver may write a store under any name in the store directory. Status
// reads that file too, so its certificates are judged.
func TestStatusReadsStoreNamedByAppliedConfig(t *testing.T) {
	applied := ""
	e, f, out, _ := statusProxyEngine(t, &applied, "", "healthy")
	withHostCommand(f, "cat '/var/lib/onebox/_host/proxy/config/traefik.yml'", transport.Result{
		Stdout: "certificatesResolvers:\n  letsencrypt:\n    acme:\n      storage: /letsencrypt/certs.json\n",
	})
	withHostCommand(f, "cat '/var/lib/onebox/_host/proxy/acme/certs.json'", transport.Result{
		Stdout: acmeFixture(t, "app.example.com", time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)),
	})
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("the certificate in the configured store is overdue, so status must diverge:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "RENEWAL OVERDUE") {
		t.Fatalf("the configured store must be read and judged:\n%s", out.String())
	}
}

// An applied configuration that cannot be parsed must not hide an overdue
// renewal, and must not mark anything for removal.
func TestStatusJudgesByExpiryWhenAppliedConfigIsUnreadable(t *testing.T) {
	applied := ""
	e, f, _, _ := statusProxyEngine(t, &applied, staleResolverStore(t), "healthy")
	withHostCommand(f, "cat '/var/lib/onebox/_host/proxy/config/traefik.yml'", transport.Result{Stdout: "certificatesResolvers: ["})

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	px := snapshot.Proxy
	if px == nil || px.Complete || !px.Diverged {
		t.Fatalf("an unparsable applied configuration must leave the proxy incomplete and diverged: %#v", px)
	}
	overdue := false
	for _, c := range px.Certificates {
		if c.Orphaned {
			t.Fatalf("no certificate may be called orphaned without the configuration: %#v", px.Certificates)
		}
		overdue = overdue || c.RenewalOverdue
	}
	if !overdue {
		t.Fatalf("the 7-day certificate must still be judged overdue by expiry: %#v", px.Certificates)
	}
	if px.LocalConfigHash == "" {
		t.Fatalf("the local configuration read is independent and must survive: %#v", px)
	}
}

// A resolver whose store cannot be placed might write anywhere, so its
// certificates are judged by expiry and never offered for removal.
func TestJudgeCertificatesTrustsResolverWithUnplacedStore(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	raw := []acmeStore{{file: "acme.json", path: "/h/acme.json", body: acmeFixture(t, "app.example.com", time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))}}
	certs, err := judgeCertificates(raw, map[string]string{app.ManagedCertificateResolver: ""}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 1 || certs[0].Orphaned || !certs[0].Overdue {
		t.Fatalf("certificate of a resolver with an unplaced store must be judged by expiry: %#v", certs)
	}
}

// A failed read of the applied configuration must not cost the certificate
// expiries that the same step reads next.
func TestStatusKeepsCertificatesWhenAppliedConfigReadFails(t *testing.T) {
	applied := ""
	e, f, _, _ := statusProxyEngine(t, &applied, acmeFixture(t, "app.example.com", time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)), "healthy")
	withHostCommand(f, "cat '/var/lib/onebox/_host/proxy/config/traefik.yml'", transport.Result{ExitCode: 1})

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	px := snapshot.Proxy
	if px == nil || len(px.Certificates) != 1 || !px.Certificates[0].RenewalOverdue {
		t.Fatalf("certificates must still be read and judged by expiry: %#v", px)
	}
}

func TestJudgeCertificatesUsesConfiguredStores(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	soon := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	raw := []acmeStore{
		{file: "acme.json", path: "/h/acme.json", body: acmeFixture(t, "app.example.com", soon)},
		{file: "acme-wildcard.json", path: "/h/acme-wildcard.json", body: acmeStoreFixture(map[string][]string{
			app.ManagedWildcardCertificateResolver: {acmeCertificate(t, "*.preview.example.com", soon)},
		})},
	}
	withDNS := map[string]string{
		app.ManagedCertificateResolver:         "acme.json",
		app.ManagedWildcardCertificateResolver: "acme-wildcard.json",
	}
	certs, err := judgeCertificates(raw, withDNS, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range certs {
		if !c.Overdue || c.Orphaned {
			t.Fatalf("both resolvers are configured for their stores, so both certificates are overdue: %#v", certs)
		}
	}

	// The configuration is unknown: judge by expiry, orphan nothing.
	certs, err = judgeCertificates(raw, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range certs {
		if !c.Overdue || c.Orphaned {
			t.Fatalf("an unknown configuration must fall back to expiry alone: %#v", certs)
		}
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

func TestStatusManagedProxyReportsMissingDiscoveryController(t *testing.T) {
	applied := ""
	e, f, out, _ := statusProxyEngine(t, &applied, "", "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "com.docker.compose.service=discovery") {
			return transport.Result{Stdout: ""}, true
		}
		return base(cmd)
	}
	if err := e.Status(context.Background()); err == nil {
		t.Fatalf("missing discovery controller must be a divergence:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "discovery controller NOT RUNNING") {
		t.Fatalf("missing discovery controller was not reported:\n%s", out.String())
	}
}

func TestStatusUnmanagedProxyUnchanged(t *testing.T) {
	// no proxy block: status must not query _host at all
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R7\n"}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "onebox.app='sample'"):
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

func TestProxyDriftParts(t *testing.T) {
	local := proxy.Identity{Config: "sha256:" + strings.Repeat("a", 64), Compose: "sha256:" + strings.Repeat("b", 64)}
	other := "sha256:" + strings.Repeat("c", 64)
	for _, tt := range []struct {
		name, applied string
		want          []string
	}{
		{"equal", local.String(), nil},
		{"compose", (proxy.Identity{Config: local.Config, Compose: other}).String(), []string{"compose"}},
		{"config", (proxy.Identity{Config: other, Compose: local.Compose}).String(), []string{"config"}},
		{"both", (proxy.Identity{Config: other, Compose: other}).String(), []string{"config", "compose"}},
		{"legacy", local.Config, []string{"config", "compose"}},
		{"garbage", "deadbeef", []string{"config", "compose"}},
		{"empty", "", []string{"config", "compose"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := proxyDriftParts(tt.applied, local); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	if got := proxyDriftParts(" ", proxy.Identity{}); !reflect.DeepEqual(got, []string{"config", "compose"}) {
		t.Fatalf("invalid pair in sync: %v", got)
	}
}

func TestProxyStatusNamesComposeDrift(t *testing.T) {
	applied := ""
	e, _, out, localHash := statusProxyEngine(t, &applied, "", "healthy")
	local, ok := proxy.ParseIdentity(localHash)
	if !ok {
		t.Fatal("invalid staged identity")
	}
	applied = (proxy.Identity{Config: local.Config, Compose: "sha256:" + strings.Repeat("c", 64)}).String()
	if err := e.Status(context.Background()); err == nil {
		t.Fatal("expected divergence")
	}
	if s := out.String(); !strings.Contains(s, "DRIFTED ⚠ (compose)") || strings.Contains(s, "(config") {
		t.Fatalf("compose drift not named: %s", s)
	}
}

func TestProxyStatusShowsDigestPairInSync(t *testing.T) {
	applied := ""
	e, _, out, localHash := statusProxyEngine(t, &applied, "", "healthy")
	local, _ := proxy.ParseIdentity(localHash)
	if err := e.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "config " + strings.TrimPrefix(local.Config, "sha256:")[:8] + " compose " + strings.TrimPrefix(local.Compose, "sha256:")[:8] + " (in sync)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("missing %q: %s", want, out.String())
	}
}

func TestShortDigest(t *testing.T) {
	if got := shortDigest("sha256:0123456789abcdef"); got != "01234567" {
		t.Fatalf("short digest = %q", got)
	}
}
