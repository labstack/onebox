package app

import (
	"fmt"
	"strings"
	"testing"
)

const namesFixture = `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: ledger
spec:
  environments:
    production: {server: root@1.2.3.4}
    staging: {server: root@5.6.7.8, basePath: /mnt/data/ob}
  workloads:
    web:
      role: Application
      image: nginx
      replicas: 3
      routes:
        - {hostname: ledger.example.com, port: 8080}
        - {hostname: api.ledger.example.com, port: 8080}
      volumes: [{name: uploads, path: /var/lib/ledger/uploads}, {source: ./seed, path: /seed, mode: Ro}]
    worker:
      role: Worker
      image: nginx
    migrate:
      role: Job
      image: nginx
      dataEffect: Migration
  services:
    postgres: {version: 18, volumes: [data, wal]}
`

// TestDerivedNamesGolden pins every derived name. A change here renames a
// resource that may already exist on a target; for a volume that means an empty
// database behind a healthy-looking deploy. If this test fails, the question is
// whether a data migration exists, not whether to update the expectation.
func TestDerivedNamesGolden(t *testing.T) {
	p, err := loadFixtureBytes([]byte(namesFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ledger",
		"ledger-migrate-1",
		"ledger-migrate-new",
		"ledger-web-1",
		"ledger-web-2",
		"ledger-web-3",
		"ledger-web-new",
		"ledger-worker-1",
		"ledger-worker-new",
		"ledger_default",
		"onebox-postgres",
		"onebox_postgres",
		"onebox_postgres_data",
		"onebox_postgres_wal",
		"onebox_services",
		"onebox_web_uploads",
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
// onebox-a-b-c, and two resources would share one volume. The application is
// not part of these names — a host has one — so only the components vary.
func TestDerivationIsInjective(t *testing.T) {
	idents := []string{"a", "b", "a-b", "b-c", "c", "web", "web-1", "x-y-z"}
	seen := map[string]string{}
	n := Names{App: "shop", BasePath: DefaultBasePath}
	for _, svc := range idents {
		for _, vol := range idents {
			name := n.ServiceVolume(svc, vol)
			key := svc + "|" + vol
			if prev, dup := seen[name]; dup {
				t.Fatalf("collision: %q derived from both %s and %s", name, prev, key)
			}
			seen[name] = key
		}
	}
}

func TestBackupNamesEscapeHyphenatedSegments(t *testing.T) {
	idents := []string{"a", "a-b", "b", "b-c", "backup", "verify"}
	credentialNames := map[string]string{}
	jobNames := map[string]string{}
	unitNames := map[string]string{}
	n := Names{App: "shop", BasePath: DefaultBasePath}
	for _, service := range idents {
		job := n.ScheduledJobUnit(service)
		if previous, exists := jobNames[job]; exists {
			t.Fatalf("scheduled job collision: %q derives from both %s and %s", job, previous, service)
		}
		jobNames[job] = service
		for _, target := range idents {
			credential := n.BackupCredentialFile(service, target)
			source := service + "|" + target
			if previous, exists := credentialNames[credential]; exists {
				t.Fatalf("credential collision: %q derives from both %s and %s", credential, previous, source)
			}
			credentialNames[credential] = source
		}
	}
	for _, service := range idents {
		for _, operation := range idents {
			unit := n.BackupUnit(service, operation)
			source := service + "|" + operation
			if previous, exists := unitNames[unit]; exists {
				t.Fatalf("backup unit collision: %q derives from both %s and %s", unit, previous, source)
			}
			unitNames[unit] = source
		}
	}

	n = Names{App: "help-desk", BasePath: DefaultBasePath}
	if got := n.BackupCredentialFile("data-base", "off-site"); !strings.HasSuffix(got, "/data--base-off--site.env") {
		t.Fatalf("escaped credential path = %q", got)
	}
	if got := n.BackupUnit("data-base", "back-up"); got != "onebox-backup-data--base-back--up" {
		t.Fatalf("escaped backup unit = %q", got)
	}
	if got := n.ScheduledJobUnit("data-base"); got != "onebox-job-data-base" {
		t.Fatalf("scheduled job unit = %q", got)
	}
}

// TestHyphenJoinWouldCollide records why underscore was chosen, so the reason
// survives someone deciding hyphens look tidier.
func TestHyphenJoinWouldCollide(t *testing.T) {
	hyphen := func(app, svc string) string { return "onebox-" + app + "-" + svc }
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
	p, err := loadFixtureBytes([]byte(namesFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.NamesFor("production").ReleaseDir("r1"); got != "/var/lib/onebox/app/releases/r1" {
		t.Errorf("production release dir = %q", got)
	}
	if got := p.NamesFor("staging").ReleaseDir("r1"); got != "/mnt/data/ob/app/releases/r1" {
		t.Errorf("staging release dir = %q", got)
	}
	if got := p.NamesFor("production").HostDir(); got != "/var/lib/onebox/_host" {
		t.Errorf("host dir = %q", got)
	}
}

// TestEveryContainerHasAnOrdinal keeps the runtime grammar uniform for users,
// scripts, and language models.
func TestEveryContainerHasAnOrdinal(t *testing.T) {
	n := Names{App: "ledger"}
	if got := n.Container("web", 1); got != "ledger-web-1" {
		t.Errorf("single replica = %q, want ledger-web-1", got)
	}
	if got := n.Container("web", 2); got != "ledger-web-2" {
		t.Errorf("second replica = %q, want ledger-web-2", got)
	}
}

func TestContainerNamesEscapeSegmentHyphens(t *testing.T) {
	n := Names{App: "help-desk"}
	if got := n.Container("web-api", 1); got != "help--desk-web--api-1" {
		t.Errorf("hyphenated container = %q, want help--desk-web--api-1", got)
	}
	if got := n.TransientContainer("web-api"); got != "help--desk-web--api-new" {
		t.Errorf("hyphenated transient = %q, want help--desk-web--api-new", got)
	}
	if restore, workload := n.BackupRestoreContainer("database"), n.Container("database-restore", 1); restore == workload {
		t.Fatalf("restore container collides with declared workload: %q", restore)
	}
}

func TestRuntimeContainerDerivationIsInjective(t *testing.T) {
	idents := []string{"a", "a-b", "a--b", "b", "b-c", "restore", "web-1"}
	seen := map[string]string{}
	add := func(name, source string) {
		t.Helper()
		if previous, exists := seen[name]; exists && previous != source {
			t.Fatalf("runtime name %q derives from both %s and %s", name, previous, source)
		}
		seen[name] = source
	}
	for _, application := range idents {
		n := Names{App: application}
		for _, component := range idents {
			for replica := 1; replica <= 3; replica++ {
				add(n.Container(component, replica), fmt.Sprintf("container %s/%s/%d", application, component, replica))
			}
			add(n.TransientContainer(component), "transient "+application+"/"+component)
			// Managed containers do not carry the application: a host has one,
			// so only the component has to be distinct.
			add(n.ServiceContainer(component), "service "+component)
			add(n.BackupRestoreContainer(component), "restore "+component)
		}
	}
}

func TestManagedContainersAreOneboxSingletons(t *testing.T) {
	n := Names{App: "shop"}
	if got := n.ServiceContainer("postgres"); got != "onebox-postgres" {
		t.Errorf("service container = %q, want onebox-postgres", got)
	}
	if got := n.BackupRestoreContainer("postgres"); got != "onebox-postgres-restore" {
		t.Errorf("restore container = %q, want onebox-postgres-restore", got)
	}
	if got := n.ServiceContainer("pg-main"); got != "onebox-pg--main" {
		t.Errorf("hyphenated service container = %q, want onebox-pg--main", got)
	}
}

// TestNormalisedRoutesReturnsDeclaredRoutes keeps generation behind one route
// accessor even though the public contract now has only the explicit list form.
func TestNormalisedRoutesReturnsDeclaredRoutes(t *testing.T) {
	p, err := loadFixtureBytes([]byte(min), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	routes := p.Workloads["ledger"].NormalisedRoutes()
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	r := routes[0]
	if r.Hostname != "ledger.example.com" || r.Port != 8080 || r.Path != "/" ||
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
	if got := n.Router("web", 0); got != "onebox_web_r0" {
		t.Errorf("router = %q, want onebox_web_r0", got)
	}
}

// TestAllNamesAreUnique: All is the preflight collision set, so a duplicate in
// it would make one resource silently stand in for another.
func TestAllNamesAreUnique(t *testing.T) {
	p, err := loadFixtureBytes([]byte(namesFixture), "ob.yml")
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
	p, err := loadFixtureBytes([]byte(namesFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range p.All("production") {
		if name == ProxyProject || name == IngressNetwork {
			t.Fatalf("derived name %q collides with a host-scoped name", name)
		}
	}
}

// The onebox-* container namespace is Onebox's. An application called onebox
// would derive workload containers inside it, and a service called proxy or
// discovery would derive the host proxy's own container names.
func TestOneboxContainerNamespaceIsReserved(t *testing.T) {
	for label, body := range map[string]string{
		"application onebox": `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: onebox
spec:
  environments: {production: {server: root@203.0.113.10}}
  workloads:
    web: {image: nginx}
`,
		"service proxy": `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: shop
spec:
  environments: {production: {server: root@203.0.113.10}}
  workloads:
    web: {image: nginx}
  services:
    proxy: {driver: redis, version: 7}
`,
		"service ingress": `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: shop
spec:
  environments: {production: {server: root@203.0.113.10}}
  workloads:
    web: {image: nginx}
  services:
    ingress: {driver: redis, version: 7}
`,
		"service services": `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: shop
spec:
  environments: {production: {server: root@203.0.113.10}}
  workloads:
    web: {image: nginx}
  services:
    services: {driver: redis, version: 7}
`,
		"service discovery": `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: shop
spec:
  environments: {production: {server: root@203.0.113.10}}
  workloads:
    web: {image: nginx}
  services:
    discovery: {driver: postgres, version: 18}
`,
	} {
		if _, err := loadFixtureBytes([]byte(body), "ob.yml"); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: loaded, or refused for another reason: %v", label, err)
		}
	}
}
