package project

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
