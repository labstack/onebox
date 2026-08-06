package app

import (
	"strings"
	"testing"
)

const namesFixture = `api_version: onebox.run/v1
app: ledger
environments:
  production: {server: root@1.2.3.4}
  staging: {server: root@5.6.7.8, base_path: /mnt/data/ob}
workloads:
  web:
    role: application
    image: nginx
    replicas: 3
    routes:
      - {domain: ledger.example.com, port: 8080}
      - {domain: api.ledger.example.com, port: 8080}
    volumes: [{name: uploads, path: /var/lib/ledger/uploads}, {source: ./seed, target: /seed}]
  worker:
    role: worker
    image: nginx
  migrate:
    role: job
    image: nginx
    data_effect: migration
services:
  postgres: {version: 18, volumes: [data, wal]}
`

// TestDerivedNamesGolden pins every derived name. A change here renames a
// resource that may already exist on a target; for a volume that means an empty
// database behind a healthy-looking deploy. If this test fails, the question is
// whether a data migration exists, not whether to update the expectation.
func TestDerivedNamesGolden(t *testing.T) {
	p, err := LoadBytes([]byte(namesFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ledger",
		"ledger_migrate",
		"ledger_migrate_new",
		"ledger_postgres",
		"ledger_web",
		"ledger_web_2",
		"ledger_web_3",
		"ledger_web_new",
		"ledger_worker",
		"ledger_worker_new",
		"ob_ledger_postgres",
		"ob_ledger_postgres_data",
		"ob_ledger_postgres_wal",
		"ob_ledger_web_uploads",
	}
	got := p.All("production")
	if len(got) != len(want) {
		t.Fatalf("derived %d names, want %d:\n got %q\nwant %q", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDerivationIsInjective is the property the naming contract rests on. The
// obvious hyphen-joined pattern fails it: (a-b, c) and (a, b-c) both derive
// ob-a-b-c, and two resources would share one volume.
func TestDerivationIsInjective(t *testing.T) {
	idents := []string{"a", "b", "a-b", "b-c", "c", "web", "web-1", "x-y-z"}
	seen := map[string]string{}
	for _, app := range idents {
		n := Names{App: app, BasePath: DefaultBasePath}
		for _, svc := range idents {
			for _, vol := range idents {
				name := n.ServiceVolume(svc, vol)
				key := app + "|" + svc + "|" + vol
				if prev, dup := seen[name]; dup {
					t.Fatalf("collision: %q derived from both %s and %s", name, prev, key)
				}
				seen[name] = key
			}
		}
	}
}

// TestHyphenJoinWouldCollide records why underscore was chosen, so the reason
// survives someone deciding hyphens look tidier.
func TestHyphenJoinWouldCollide(t *testing.T) {
	hyphen := func(app, svc string) string { return "ob-" + app + "-" + svc }
	if hyphen("a-b", "c") != hyphen("a", "b-c") {
		t.Skip("hyphen joining no longer ambiguous; the underscore rule may be revisited")
	}
	if (Names{App: "a-b"}).ServiceProject("c") == (Names{App: "a"}).ServiceProject("b-c") {
		t.Fatal("underscore joining is ambiguous too; the naming contract is broken")
	}
}

// TestBasePathPerEnvironment: environments commonly place state on different
// mounted volumes, so the base path resolves per environment.
func TestBasePathPerEnvironment(t *testing.T) {
	p, err := LoadBytes([]byte(namesFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.NamesFor("production").ReleaseDir("r1"); got != "/var/lib/ob/ledger/releases/r1" {
		t.Errorf("production release dir = %q", got)
	}
	if got := p.NamesFor("staging").ReleaseDir("r1"); got != "/mnt/data/ob/ledger/releases/r1" {
		t.Errorf("staging release dir = %q", got)
	}
	if got := p.NamesFor("production").HostDir(); got != "/var/lib/ob/_host" {
		t.Errorf("host dir = %q", got)
	}
}

// TestSingleReplicaContainerHasNoIndex keeps the common case readable.
func TestSingleReplicaContainerHasNoIndex(t *testing.T) {
	n := Names{App: "ledger"}
	if got := n.Container("web", 1); got != "ledger_web" {
		t.Errorf("single replica = %q, want ledger_web", got)
	}
	if got := n.Container("web", 2); got != "ledger_web_2" {
		t.Errorf("second replica = %q, want ledger_web_2", got)
	}
}

// TestScalarRoutingNormalises: the domain/port shorthand becomes one route with
// documented defaults, so generation never sees two shapes.
func TestScalarRoutingNormalises(t *testing.T) {
	p, err := LoadBytes([]byte(min), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	routes := p.Workloads["ledger"].NormalisedRoutes()
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	r := routes[0]
	if r.Domain != "ledger.example.com" || r.Port != 8080 || r.Path != "/" ||
		r.Protocol != "http" || r.Scheme != "http" || r.TLS != "terminate" {
		t.Fatalf("normalised route = %+v", r)
	}
}

// TestRouterDoesNotLookLikeAReplica records the second collision the golden test
// caught: router 2 and replica 2 derived the same string.
func TestRouterDoesNotLookLikeAReplica(t *testing.T) {
	n := Names{App: "ledger"}
	if n.Router("web", 2) == n.Container("web", 2) {
		t.Fatalf("router and replica derive the same name: %q", n.Router("web", 2))
	}
	if got := n.Router("web", 0); got != "ledger_web_r0" {
		t.Errorf("router = %q, want ledger_web_r0", got)
	}
}

// TestAllNamesAreUnique: All is the preflight collision set, so a duplicate in
// it would make one resource silently stand in for another.
func TestAllNamesAreUnique(t *testing.T) {
	p, err := LoadBytes([]byte(namesFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, name := range p.All("production") {
		if seen[name] {
			t.Fatalf("duplicate derived name %q", name)
		}
		seen[name] = true
	}
}

// TestNoDerivedNameCollidesWithHostScoped guards the reserved hyphenated names.
func TestNoDerivedNameCollidesWithHostScoped(t *testing.T) {
	p, err := LoadBytes([]byte(namesFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range p.All("production") {
		if name == ProxyProject || name == IngressNetwork {
			t.Fatalf("derived name %q collides with a host-scoped name", name)
		}
		if strings.HasPrefix(name, "ob-") {
			t.Fatalf("derived name %q entered the reserved hyphenated namespace", name)
		}
	}
}
