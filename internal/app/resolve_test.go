package app

import (
	"strings"
	"testing"
)

const overrideFixture = `api_version: onebox.run/v1
app: ledger
environments:
  production:
    server: root@prod
  staging:
    server: root@stage
    overrides:
      workloads:
        web:
          replicas: 1
          resources: {memory: 256MB}
          env: {LOG_LEVEL: debug, TRACING: null}
      services:
        postgres:
          resources: {memory: 512MB}
workloads:
  web:
    role: application
    image: nginx
    replicas: 4
    resources: {memory: 2GB, cpus: "2"}
    env: {LOG_LEVEL: info, TRACING: on, REGION: eu}
services:
  postgres: {version: 18, resources: {memory: 4GB, cpus: "4"}}
`

func resolve(t *testing.T, yaml, env string) *Resolved {
	t.Helper()
	p, err := LoadBytes([]byte(yaml), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Resolve(env)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestOverrideBeatsExplicit is the inversion a review caught in the design: a
// project value of 4 replicas must not defeat an environment override to 1.
func TestOverrideBeatsExplicit(t *testing.T) {
	if got := resolve(t, overrideFixture, "staging").Workloads["web"].Replicas; got != 1 {
		t.Errorf("staging replicas = %d, want 1", got)
	}
	if got := resolve(t, overrideFixture, "production").Workloads["web"].Replicas; got != 4 {
		t.Errorf("production replicas = %d, want 4", got)
	}
}

// TestMappingMergesAndNullRemoves: a single setting changes without restating
// the rest, and null takes one away.
func TestMappingMergesAndNullRemoves(t *testing.T) {
	env := resolve(t, overrideFixture, "staging").Workloads["web"].Env
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("LOG_LEVEL = %v, want debug", env["LOG_LEVEL"])
	}
	if env["REGION"] != "eu" {
		t.Errorf("REGION = %v, want the project value to survive", env["REGION"])
	}
	if _, present := env["TRACING"]; present {
		t.Error("a null override should remove the key")
	}
}

// TestPartialResourceOverrideKeepsTheRest guards the one-level merge.
func TestPartialResourceOverrideKeepsTheRest(t *testing.T) {
	res := resolve(t, overrideFixture, "staging").Workloads["web"].Resources
	if res.Memory != "256MB" {
		t.Errorf("memory = %q, want the override", res.Memory)
	}
	if res.CPUs != "2" {
		t.Errorf("cpus = %q, want the project value to survive a partial override", res.CPUs)
	}
}

// TestServiceOverride covers the other side of the closed set.
func TestServiceOverride(t *testing.T) {
	svc := resolve(t, overrideFixture, "staging").Services["postgres"]
	if svc.Resources.Memory != "512MB" {
		t.Errorf("memory = %q, want the override", svc.Resources.Memory)
	}
	if svc.Resources.CPUs != "4" {
		t.Errorf("cpus = %q, want the project value to survive", svc.Resources.CPUs)
	}
}

// TestResolveDoesNotLeakBetweenEnvironments: resolving staging must not change
// what production sees.
func TestResolveDoesNotLeakBetweenEnvironments(t *testing.T) {
	p, err := LoadBytes([]byte(overrideFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve("staging"); err != nil {
		t.Fatal(err)
	}
	prod, err := p.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	if got := prod.Workloads["web"].Replicas; got != 4 {
		t.Fatalf("production replicas = %d after resolving staging; overrides leaked", got)
	}
	if got := p.Workloads["web"].Replicas; got != 4 {
		t.Fatalf("the source project was mutated: replicas = %d", got)
	}
}

// TestOriginsRecordOverrides so a reader can tell a staging value from a
// project-level one without diffing two files.
func TestOriginsRecordOverrides(t *testing.T) {
	o := resolve(t, overrideFixture, "staging").Origins
	if o["workloads.web.replicas"] != OriginOverride {
		t.Errorf("replicas origin = %q", o["workloads.web.replicas"])
	}
	if o["services.postgres.resources.memory"] != OriginOverride {
		t.Errorf("service memory origin = %q", o["services.postgres.resources.memory"])
	}
	if _, recorded := o["workloads.web.image"]; recorded {
		t.Error("an untouched field should not be recorded as overridden")
	}
	// The override sets memory and says nothing about cpus. Recording the block
	// and expanding it while printing made `ob canonical` label the project's
	// own cpus an environment override — the one command that exists to say
	// where a value came from, saying the wrong thing.
	if _, recorded := o["services.postgres.resources.cpus"]; recorded {
		t.Error("a sibling the override did not set is recorded as overridden")
	}
	if _, recorded := o["workloads.web.resources.cpus"]; recorded {
		t.Error("a sibling the override did not set is recorded as overridden")
	}
}

// TestUnknownOverrideTargetRefused: silently ignoring a typo would leave someone
// convinced staging was scaled down when it was not.
func TestUnknownOverrideTargetRefused(t *testing.T) {
	y := strings.Replace(overrideFixture, "        web:\n", "        wbe:\n", 1)
	p, err := LoadBytes([]byte(y), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Resolve("staging")
	var e *Error
	if !asError(err, &e) || e.Code != "override_unknown_workload" {
		t.Fatalf("got %v, want override_unknown_workload", err)
	}
}

// TestOverrideOutsideClosedSetRefused: an override may change how much runs,
// never what runs.
func TestOverrideOutsideClosedSetRefused(t *testing.T) {
	p := &Spec{
		Name:         "ledger",
		Environments: map[string]Environment{"staging": {Overrides: &Overrides{Workloads: map[string]map[string]any{"web": {"image": "other"}}}}},
		Workloads:    map[string]Workload{"web": {Role: "application"}},
	}
	_, err := p.Resolve("staging")
	var e *Error
	if !asError(err, &e) || e.Code != "override_not_permitted" {
		t.Fatalf("got %v, want override_not_permitted", err)
	}
	if !strings.Contains(e.Message, "replicas") {
		t.Errorf("the refusal should list what may be overridden: %s", e.Message)
	}
}

// TestRenderUsesResolvedValues ties resolution to generation.
func TestRenderUsesResolvedValues(t *testing.T) {
	p, err := LoadBytes([]byte(overrideFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	staging, err := p.Resolve("staging")
	if err != nil {
		t.Fatal(err)
	}
	r, err := staging.Render("staging", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Bytes), "mem_limit: 256MB") {
		t.Errorf("rendered runtime should carry the staging override\n%s", r.Bytes)
	}
}

// TestRenderResolvesAutomatically closes the footgun: rendering a project
// without resolving it first would silently ignore every override.
func TestRenderResolvesAutomatically(t *testing.T) {
	p, err := LoadBytes([]byte(overrideFixture), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Render("staging", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Bytes), "mem_limit: 256MB") {
		t.Errorf("Render must apply overrides without being asked\n%s", r.Bytes)
	}
	prod, err := p.Render("production", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prod.Bytes), "mem_limit: 2GB") {
		t.Errorf("production should keep its own value\n%s", prod.Bytes)
	}
}

// A field withdrawn from the contract cannot be overridden. Leaving it in the
// permitted set would accept an override naming something no service has, and
// accepting it silently is how an operator comes to believe a setting applied.
func TestOverridingAWithdrawnFieldIsRefused(t *testing.T) {
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@h}
  staging:
    server: root@h2
    overrides:
      services:
        postgres: {backup: {schedule: {cron: "0 2 * * *"}}}
workloads: {web: {role: application, image: x:1}}
services: {postgres: 17}
`), "ob.yml")
	if err != nil {
		// Refused at load is equally correct: the override block is closed too.
		if !strings.Contains(err.Error(), "backup") {
			t.Fatalf("the refusal must name the withdrawn field: %v", err)
		}
		return
	}
	if _, err := spec.Resolve("staging"); err == nil {
		t.Fatal("an override of a withdrawn field must be refused")
	} else if !strings.Contains(err.Error(), "backup") {
		t.Fatalf("the refusal must name the field: %v", err)
	}
}
