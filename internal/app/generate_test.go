package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A decent-size project of the shape people actually build: a web application,
// a background worker, a migration job, and a database they still author.
const appFixture = `api_version: onebox.run/v1
app: ledger
environments:
  production: {server: root@1.2.3.4}
workloads:
  web:
    role: application
    image: ghcr.io/acme/ledger:1.4.0
    replicas: 2
    domain: ledger.example.com
    port: 8080
    health: {http: /healthz, port: 8080, interval: 10s, retries: 3}
    drain: {grace: 30s}
    needs: [db]
    volumes: [uploads]
    resources: {memory: 1GB}
  worker:
    role: worker
    image: ghcr.io/acme/ledger:1.4.0
    command: [./ledger, worker]
    needs: [db]
  migrate:
    role: job
    image: ghcr.io/acme/ledger:1.4.0
    command: [./ledger, migrate]
    run: pre_release
    data_effect: migration
    needs: [{name: db, condition: healthy}]
  db:
    role: daemon
    image: postgres:16-alpine
    health: {exec: "pg_isready -U ledger", interval: 5s}
    volumes: [{source: /data/postgres, target: /var/lib/postgresql/data}]
runtime:
  env_files: [.env.production]
`

func digestOf(t *testing.T, yaml string) string {
	t.Helper()
	p, err := LoadBytes([]byte(yaml), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Render("production", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.Digest
}

func render(t *testing.T, yaml string) []byte {
	t.Helper()
	p, err := LoadBytes([]byte(yaml), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Render("production", "20260802-120000-abc1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.Bytes
}

// TestRenderIsDeterministic is the property the plan digest rests on. Map
// iteration order is the easiest way to break it by accident.
func TestRenderIsDeterministic(t *testing.T) {
	for i := 0; i < 20; i++ {
		a, b := digestOf(t, appFixture), digestOf(t, appFixture)
		if a != b {
			t.Fatalf("run %d: digests differ", i)
		}
	}
}

// TestDigestChangesWithRuntimeAffectingInput and its sibling below draw the line
// the spec draws: the digest tracks the runtime, not the file.
func TestDigestChangesWithRuntimeAffectingInput(t *testing.T) {
	before := digestOf(t, appFixture)
	after := digestOf(t, strings.Replace(appFixture, "ledger:1.4.0", "ledger:1.5.0", 1))
	if before == after {
		t.Fatal("changing an image reference must change the digest")
	}
}

func TestDigestIgnoresNonRuntimeInput(t *testing.T) {
	before := digestOf(t, appFixture)
	after := digestOf(t, appFixture+"x-note: irrelevant\n")
	if before != after {
		t.Fatalf("an extension key must not change the runtime")
	}
}

// A supporting service is part of the runtime, so its version binds into the
// digest. A plan that did not notice a major Postgres upgrade because the
// application was untouched would be a plan that lied.
func TestDigestChangesWithServiceVersion(t *testing.T) {
	before := digestOf(t, appFixture+"services: {postgres: 16}\n")
	after := digestOf(t, appFixture+"services: {postgres: 17}\n")
	if before == after {
		t.Fatal("changing a service version must change the digest")
	}
	if plain := digestOf(t, appFixture); plain == before {
		t.Fatal("declaring a service must change the digest")
	}
}

func TestRenderedRuntime(t *testing.T) {
	out := string(render(t, appFixture))

	for _, want := range []string{
		"name: ledger",
		"image: ghcr.io/acme/ledger:1.4.0",
		"ob.app: ledger",
		"ob.workload: web",
		"ob.release: 20260802-120000-abc1234",
		"traefik.enable:",
		"Host(`ledger.example.com`)",
		"traefik.http.services.ledger_web.loadbalancer.server.port:",
		"condition: service_healthy",
		"stop_grace_period: 30s",
		"mem_limit: 1GB",
		"ob_ledger_web_uploads",
		"/data/postgres:/var/lib/postgresql/data",
		"pg_isready -U ledger",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered runtime is missing %q\n%s", want, out)
		}
	}

	// Onebox owns container naming; a fixed name forbids the two containers a
	// rolling handover needs.
	if strings.Contains(out, "container_name") {
		t.Error("rendered runtime must not set container_name")
	}
}

// TestEnvFilesAreNotProjectedIntoDaemons is the rule seven real projects forced:
// a database must not receive the application's secrets.
func TestEnvFilesAreNotProjectedIntoDaemons(t *testing.T) {
	p, err := LoadBytes([]byte(appFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.envFilesFor(p.Workloads["web"]); len(got) != 1 || got[0] != ".env.production" {
		t.Errorf("application env files = %v, want the project list", got)
	}
	if got := p.envFilesFor(p.Workloads["worker"]); len(got) != 1 {
		t.Errorf("worker env files = %v, want the project list", got)
	}
	if got := p.envFilesFor(p.Workloads["db"]); len(got) != 0 {
		t.Errorf("daemon env files = %v, want none", got)
	}
}

// TestWorkloadEnvFilesOverrideProjectList: a workload with its own list gets
// only its own, so one service's secrets stay out of another's container.
func TestWorkloadEnvFilesOverrideProjectList(t *testing.T) {
	y := strings.Replace(appFixture,
		"    role: worker\n", "    role: worker\n    env_files: [worker/.env]\n", 1)
	p, err := LoadBytes([]byte(y), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	got := p.envFilesFor(p.Workloads["worker"])
	if len(got) != 1 || got[0] != "worker/.env" {
		t.Fatalf("worker env files = %v, want only its own", got)
	}
}

// TestBuildWithoutResolvedImageFailsClosed: the release pipeline resolves this
// later; until then generation must refuse rather than emit a broken runtime.
func TestBuildWithoutResolvedImageFailsClosed(t *testing.T) {
	y := strings.Replace(appFixture, "    image: ghcr.io/acme/ledger:1.4.0\n    replicas: 2\n",
		"    build: .\n    replicas: 2\n", 1)
	p, err := LoadBytes([]byte(y), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Render("production", "r1", nil); err == nil {
		t.Fatal("expected generation to fail without a resolved image")
	} else {
		var e *Error
		if !asError(err, &e) || e.Code != "image_unresolved" {
			t.Fatalf("got %v, want image_unresolved", err)
		} else if e.Next == "" {
			t.Error("the failure should name the command that resolves it")
		}
	}
	if _, err := p.Render("production", "r1", Images{"web": "ghcr.io/acme/ledger:1.4.0"}); err != nil {
		t.Fatalf("resolved image should render: %v", err)
	}
}

// With nothing routing, there is no label and no network to add. The fixture
// drops its route first, because declaring one under `kind: none` is refused
// at load — a route nobody would serve is not a runtime question.
func TestNoProxyAddsNothing(t *testing.T) {
	y := strings.Replace(appFixture, "    domain: ledger.example.com\n", "", 1)
	y = strings.Replace(y, "    port: 8080\n", "", 1)
	out := string(render(t, y+"proxy: {kind: none}\n"))
	if strings.Contains(out, "traefik") {
		t.Error("no proxy must not add routing labels")
	}
	if strings.Contains(out, "ob-ingress") {
		t.Error("no proxy must not attach an ingress network")
	}
}

// TestUDPPortRendered covers the protocol a real project needed.
func TestUDPPortRendered(t *testing.T) {
	y := strings.Replace(appFixture, "    volumes: [uploads]\n",
		"    volumes: [uploads]\n    ports: [{host: 8555, container: 8555, protocol: udp}]\n", 1)
	out := string(render(t, y))
	if !strings.Contains(out, "127.0.0.1:8555:8555/udp") {
		t.Errorf("expected a loopback-bound UDP publish\n%s", out)
	}
}

// TestJobsDoNotRestartOrAutoStart: a job runs to completion at a release phase.
// Restarting it forever would be wrong, and `compose up` must not start it.
func TestJobsDoNotRestartOrAutoStart(t *testing.T) {
	p, err := LoadBytes([]byte(appFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	n := p.NamesFor("production")
	svc, _, _, err := p.renderWorkload(n, "migrate", p.Workloads["migrate"], "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if svc["restart"] != "no" {
		t.Errorf("job restart = %v, want no", svc["restart"])
	}
	if svc["profiles"] == nil {
		t.Error("a job must sit behind a profile so compose up does not start it")
	}
	web, _, _, _ := p.renderWorkload(n, "web", p.Workloads["web"], "r1", nil)
	if web["restart"] != "unless-stopped" {
		t.Errorf("application restart = %v, want unless-stopped", web["restart"])
	}
	if web["profiles"] != nil {
		t.Error("an application must not sit behind a profile")
	}
}

// TestTLSTerminationNamesAResolver: terminating TLS without one yields a router
// that never obtains a certificate.
func TestTLSTerminationNamesAResolver(t *testing.T) {
	y := appFixture + "proxy: {cert_resolver: le}\n"
	out := string(render(t, y))
	if !strings.Contains(out, "tls.certresolver: le") {
		t.Errorf("expected a certificate resolver on the terminating router\n%s", out)
	}
	if strings.Contains(string(render(t, appFixture)), "certresolver") {
		t.Error("no resolver declared, so none should be emitted")
	}
}

// TestEveryDraftRenders runs generation over the real conversion drafts.
func TestEveryDraftRenders(t *testing.T) {
	dir := filepath.Join("..", "..", "openspec", "changes",
		"adopt-declarative-project-schema", "conversions")
	files, _ := filepath.Glob(filepath.Join(dir, "*.yml"))
	if len(files) == 0 {
		t.Skip("no conversion drafts")
	}
	for _, f := range files {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".yml"), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			p, err := LoadBytes(b, f)
			if err != nil {
				t.Fatal(err)
			}
			env := "production"
			if _, ok := p.Environments[env]; !ok {
				for k := range p.Environments {
					env = k
					break
				}
			}
			images := Images{}
			for name, w := range p.Workloads {
				if w.Build != nil {
					images[name] = "example.invalid/" + name + ":test"
				}
			}
			r, err := p.Render(env, "r1", images)
			if err != nil {
				// The drafts are conversion evidence; the Compose files they
				// reference live in the real repositories, not beside them.
				var e *Error
				if asError(err, &e) && e.Code == "compose_file_unreadable" {
					t.Skipf("references a Compose file kept in its own repository: %v", e.Message)
				}
				t.Fatalf("render: %v", err)
			}
			if len(r.Bytes) == 0 || r.Digest == "" {
				t.Fatal("render produced nothing")
			}
		})
	}
}

// TestPassthroughFields covers the nine fields a survey of 276 real projects
// showed standing between two thirds of services and the declaration. Each
// carries no Onebox semantics: it is declared, and it appears.
func TestPassthroughFields(t *testing.T) {
	y := `api_version: onebox.run/v1
app: ledger
environments:
  production: {server: root@1.2.3.4}
workloads:
  web:
    role: application
    image: nginx
    entrypoint: [/bin/sh, -c, "exec app"]
    user: "1000:1000"
    hostname: web-1
    working_dir: /srv
    init: true
    tty: false
    stdin_open: true
    extra_hosts: ["db:10.0.0.5"]
    labels: {com.example.team: platform, ofelia.enabled: "true"}
`
	out := string(render(t, y))
	for _, want := range []string{
		"entrypoint:", "user: 1000:1000", "hostname: web-1", "working_dir: /srv",
		"init: true", "stdin_open: true", "db:10.0.0.5",
		"com.example.team: platform", "ofelia.enabled:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
	// tty: false is declared and must survive as false rather than be dropped.
	if !strings.Contains(out, "tty: false") {
		t.Errorf("an explicit false must not be dropped\n%s", out)
	}
	// Onebox's own labels still land alongside the user's.
	if !strings.Contains(out, "ob.app: ledger") {
		t.Error("identity labels must survive user labels")
	}
}

// TestUserLabelsCannotClaimOneboxNamespaces: the two namespaces Onebox
// generates into are reserved, so a user label can never silently win.
func TestUserLabelsCannotClaimOneboxNamespaces(t *testing.T) {
	for _, bad := range []string{"ob.app", "traefik.enable"} {
		y := `api_version: onebox.run/v1
app: ledger
environments: {production: {server: h}}
workloads: {web: {role: application, image: nginx, labels: {"` + bad + `": x}}}
`
		if _, err := LoadBytes([]byte(y), "ob.yml"); err == nil {
			t.Errorf("label %q should be refused", bad)
		}
	}
}

// TestReplicaCountIsBound: Onebox runs replicas itself under derived slot
// names, so the count is not a Compose concern — but a plan binds the runtime
// digest, and a scale change that renders identically would slip past it.
func TestReplicaCountIsBound(t *testing.T) {
	one := digestOf(t, appFixture)
	three := digestOf(t, strings.Replace(appFixture, "    replicas: 2\n", "    replicas: 5\n", 1))
	if one == three {
		t.Fatal("changing the replica count must change the runtime digest")
	}
}

// TestVolumeNamesArePinned: Compose prefixes the project name onto a volume
// unless the name is pinned, so the volume Docker creates would not be the one
// the naming contract promises — and preflight, which looks for the contract
// name, would never see a collision that exists. Found by deploying.
func TestVolumeNamesArePinned(t *testing.T) {
	out := string(render(t, appFixture))
	if !strings.Contains(out, "name: ob_ledger_web_uploads") {
		t.Errorf("the derived volume name must be pinned\n%s", out)
	}
}

// An image built from scratch has no shell. A health check it cannot run is a
// workload that can never be released, so the exec form must reach the runtime
// as CMD rather than CMD-SHELL.
func TestExecListHealthRunsWithoutAShell(t *testing.T) {
	out := render(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web:
    role: application
    image: scratch-built:1
    health: {exec: ["/app", "health"], interval: 2s}
`)
	body := string(out)
	if !strings.Contains(body, "- CMD\n") {
		t.Fatalf("an exec list must run directly, not through a shell:\n%s", body)
	}
	if strings.Contains(body, "CMD-SHELL") {
		t.Fatalf("an exec list must not be wrapped in a shell:\n%s", body)
	}
}

// The string form still runs through a shell, which is what makes `pg_isready
// -U x && test -f /ready` work.
func TestExecStringHealthKeepsItsShell(t *testing.T) {
	out := render(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1, health: {exec: "test -f /ready && echo ok"}}
`)
	if !strings.Contains(string(out), "CMD-SHELL") {
		t.Fatalf("a shell-form check must keep its shell:\n%s", out)
	}
}

// The rollout takes a container out of rotation by making its health check
// fail, before anything sends a signal. Without the guard the container reports
// healthy right up to the moment it dies, and the requests in flight then are
// lost — a zero-downtime deploy that is not one.
func TestShellHealthChecksCarryTheDrainGuard(t *testing.T) {
	for _, form := range []string{
		`health: {http: /, port: 80}`,
		`health: {tcp: true, port: 5432}`,
		`health: {exec: "test -f /ready"}`,
	} {
		out := string(render(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1, `+form+`}
`))
		if !strings.Contains(out, DrainFile) {
			t.Errorf("%s is not drain-guarded:\n%s", form, out)
		}
	}
}
