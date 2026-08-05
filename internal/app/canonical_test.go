package app

import (
	"strings"
	"testing"
)

const canonicalProject = `api_version: onebox.run/v1
app: ledger
environments:
  production: {server: root@1.2.3.4}
  staging:
    server: root@5.6.7.8
    overrides: {workloads: {ledger: {replicas: 3}}}
build: .
domain: ledger.example.com
port: 8080
`

func originsFor(t *testing.T, env string) map[string]Origin {
	t.Helper()
	p, err := LoadBytes([]byte(canonicalProject), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Resolve(env)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Origin{}
	for _, kv := range r.OriginTable() {
		out[kv[0]] = Origin(kv[1])
	}
	return out
}

// TestOriginsDistinguishWhatWasWritten. A project file shows what was typed; it
// cannot show that a value is what it is because nobody said otherwise. That
// difference is the whole point of printing a canonical form.
func TestOriginsDistinguishWhatWasWritten(t *testing.T) {
	o := originsFor(t, "production")
	for path, want := range map[string]Origin{
		"app":                            OriginExplicit,
		"workloads.ledger.build.context": OriginExplicit,
		"workloads.ledger.domain":        OriginShorthand,
		"workloads.ledger.port":          OriginShorthand,
		"workloads.ledger.replicas":      OriginDefault,
		"workloads.ledger.strategy":      OriginDefault,
		"base_path":                      OriginDefault,
		"proxy.network":                  OriginDefault,
	} {
		if o[path] != want {
			t.Errorf("%s = %q, want %q", path, o[path], want)
		}
	}
}

// TestInjectedRoleIsNotClaimedAsTheAuthorsChoice. Normalisation inserts `role`
// so the schema can discriminate. Reporting it as explicit would tell someone
// they made a decision they never made.
func TestInjectedRoleIsNotClaimedAsTheAuthorsChoice(t *testing.T) {
	if got := originsFor(t, "production")["workloads.ledger.role"]; got != OriginDefault {
		t.Errorf("injected role reported as %q, want default", got)
	}
}

// TestOverrideOriginSurvivesResolution.
func TestOverrideOriginSurvivesResolution(t *testing.T) {
	if got := originsFor(t, "staging")["workloads.ledger.replicas"]; got != OriginOverride {
		t.Errorf("staging replicas = %q, want override", got)
	}
	if got := originsFor(t, "production")["workloads.ledger.replicas"]; got != OriginDefault {
		t.Errorf("production replicas = %q, want default", got)
	}
}

// TestCanonicalAnnotatesOnlyWhatWasNotWritten: annotating an explicit value
// would be noise on every line the author actually typed.
func TestCanonicalAnnotatesOnlyWhatWasNotWritten(t *testing.T) {
	p, _ := LoadBytes([]byte(canonicalProject), "ob.yml")
	r, _ := p.Resolve("staging")
	body, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)

	if !strings.Contains(out, "replicas: 3 # override") {
		t.Errorf("the override should be marked\n%s", out)
	}
	if !strings.Contains(out, "# default") || !strings.Contains(out, "# shorthand") {
		t.Errorf("defaults and shorthand should be marked\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "app: ledger") && strings.Contains(line, "#") {
			t.Errorf("an explicit value should carry no annotation: %q", line)
		}
	}
}

// Every declared default must appear in the canonical form, marked as derived.
//
// This is the defect the previous implementation had: a default on an optional
// field never materialised, so a value the contract documented as present was
// silently absent, and the canonical form — the thing people read to find out
// what Onebox understood — did not show it either.
func TestEveryDefaultAppearsAsDerived(t *testing.T) {
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web:
    role: application
    image: nginx
    routes: [{domain: shop.example.com, port: 80}]
    volumes: [{name: data, path: /data}]
    ports: [{host: 9000, container: 9000}]
    persistence: {}
    drain: {}
  job:
    role: job
    image: nginx
    data_effect: none
    schedule: {cron: "0 2 * * *"}
services:
  postgres: 17
notifications: {ops: {webhook: "https://example.invalid/hook"}}
runtime: {env_files: [{file: secrets.env, provider: sops}]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	origins := map[string]string{}
	for _, row := range r.OriginTable() {
		origins[row[0]] = row[1]
	}

	// Each of these is a value nobody wrote and every one of them decides
	// something about the runtime.
	for path, want := range map[string]string{
		"base_path":                          DefaultBasePath,
		"deployment.retain_releases":         "5",
		"deployment.migration_policy":        "manual",
		"proxy.kind":                         "traefik-docker",
		"proxy.network":                      IngressNetwork,
		"workloads.web.replicas":             "1",
		"workloads.web.strategy":             "rolling",
		"workloads.web.image.pull":           "missing",
		"workloads.web.drain.signal":         "TERM",
		"workloads.web.routes[0].path":       "/",
		"workloads.web.routes[0].entrypoint": "websecure",
		"workloads.web.routes[0].tls":        "terminate",
		"workloads.web.volumes[0].mode":      "rw",
		"workloads.web.ports[0].bind":        "127.0.0.1",
		"workloads.web.ports[0].protocol":    "tcp",
		"workloads.web.persistence.mode":     "durable",
		"workloads.job.run":                  "manual",
		"workloads.job.schedule.timezone":    "UTC",
		"notifications.ops.format":           "text",
	} {
		if origins[path] != string(OriginDefault) {
			t.Errorf("%s is %q, want %q — a default nobody can see is a default nobody can check (value should be %q)",
				path, origins[path], OriginDefault, want)
		}
	}

	// And a value the author did write is never reported as derived.
	for _, path := range []string{"app", "workloads.web.role", "workloads.job.data_effect"} {
		if origins[path] == string(OriginDefault) && path != "workloads.web.role" {
			t.Errorf("%s was written by the author and must not be reported as derived", path)
		}
	}
}
