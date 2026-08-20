package app

import (
	"reflect"
	"testing"
)

func secretGraphProject(t *testing.T, body string) *Resolved {
	t.Helper()
	spec, err := LoadBytes([]byte(body), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestSecretDeclarationGraphCapturesOrderScopeAndAffectedWorkloads(t *testing.T) {
	resolved := secretGraphProject(t, `
api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example}}
runtime:
  env_files:
    - {file: shared.enc.env, provider: sops}
    - {file: later.enc.env, provider: sops}
workloads:
  web: {role: application, image: nginx}
  worker: {role: worker, image: nginx}
`)
	graph := resolved.SecretDeclarationGraph()
	want := []SecretDeclaration{
		{ID: "secret_bb87e5bf4bf6", SourceFile: "shared.enc.env", Provider: "sops", OutputPath: ".ob-decrypted-sops-shared.enc.env", Scope: "runtime-default", Order: 0, AffectedWorkloads: []string{"web", "worker"}},
		{ID: "secret_dbdbbfa277bf", SourceFile: "later.enc.env", Provider: "sops", OutputPath: ".ob-decrypted-sops-later.enc.env", Scope: "runtime-default", Order: 1, AffectedWorkloads: []string{"web", "worker"}},
	}
	if !reflect.DeepEqual(graph, want) {
		t.Fatalf("graph = %#v, want %#v", graph, want)
	}
}

func TestSecretDeclarationIDsAreStableAndValueFree(t *testing.T) {
	resolved := secretGraphProject(t, `
api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example}}
runtime: {env_files: [{file: secrets.enc.env, provider: sops}]}
workloads: {web: {role: application, image: nginx}}
`)
	first := resolved.SecretDeclarationGraph()
	second := resolved.SecretDeclarationGraph()
	if len(first) != 1 || first[0].ID != "secret_ee5df50c58b5" || !reflect.DeepEqual(first, second) {
		t.Fatalf("unstable declaration IDs: first=%+v second=%+v", first, second)
	}
}

func TestSecretDeclarationGraphChangesForEveryRuntimeRelevantDrift(t *testing.T) {
	base := `
api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example}}
runtime:
  env_files:
    - {file: first.enc.env, provider: sops}
    - {file: second.enc.env, provider: sops}
workloads:
  web: {role: application, image: nginx}
  worker: {role: worker, image: nginx}
`
	want := []SecretDeclaration{
		{ID: "secret_a87617a41c5a", SourceFile: "first.enc.env", Provider: "sops", OutputPath: ".ob-decrypted-sops-first.enc.env", Scope: "runtime-default", Order: 0, AffectedWorkloads: []string{"web", "worker"}},
		{ID: "secret_e21b0c3c5c76", SourceFile: "second.enc.env", Provider: "sops", OutputPath: ".ob-decrypted-sops-second.enc.env", Scope: "runtime-default", Order: 1, AffectedWorkloads: []string{"web", "worker"}},
	}
	if got := secretGraphProject(t, base).SecretDeclarationGraph(); !reflect.DeepEqual(got, want) {
		t.Fatalf("base graph = %#v, want literal %#v", got, want)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "reordered", body: `
api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example}}
runtime: {env_files: [{file: second.enc.env, provider: sops}, {file: first.enc.env, provider: sops}]}
workloads: {web: {role: application, image: nginx}, worker: {role: worker, image: nginx}}
`},
		{name: "scope changed", body: `
api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example}}
runtime: {env_files: [{file: first.enc.env, provider: sops}, {file: second.enc.env, provider: sops}]}
workloads:
  web: {role: application, image: nginx, env_files: [{file: first.enc.env, provider: sops}, {file: second.enc.env, provider: sops}]}
  worker: {role: worker, image: nginx}
`},
		{name: "provider removed", body: `
api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example}}
runtime: {env_files: [first.enc.env, {file: second.enc.env, provider: sops}]}
workloads: {web: {role: application, image: nginx}, worker: {role: worker, image: nginx}}
`},
		{name: "affected workload removed", body: `
api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example}}
runtime: {env_files: [{file: first.enc.env, provider: sops}, {file: second.enc.env, provider: sops}]}
workloads: {web: {role: application, image: nginx}}
`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := secretGraphProject(t, test.body).SecretDeclarationGraph()
			if reflect.DeepEqual(got, want) {
				t.Fatalf("drift did not change graph: %+v", got)
			}
		})
	}
}

func TestSecretDeclarationGraphIncludesSortedExternalProjection(t *testing.T) {
	resolved := secretGraphProject(t, `
api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example}}
workloads:
  web:
    image: nginx
    needs:
      - name: database
        condition: healthy
        env: {Z_DATABASE_URL: url, A_DATABASE_URL: url}
external_services:
  database:
    driver: postgres
    connection:
      source: {file: secrets/database.env, provider: sops}
      entries: {url: DATABASE_URL}
    backup_owner: platform-team/rds
    probe: {}
`)
	want := []SecretDeclaration{{
		ID: "secret_84b31ed35a16", SourceFile: "secrets/database.env", Provider: "sops",
		OutputPath: ".ob-external-database_web.env", Scope: "external:web", Order: 0,
		AffectedWorkloads: []string{"web"},
		ProjectionEntries: []SecretProjectionEntry{
			{Destination: "A_DATABASE_URL", Source: "DATABASE_URL"},
			{Destination: "Z_DATABASE_URL", Source: "DATABASE_URL"},
		},
	}}
	if got := resolved.SecretDeclarationGraph(); !reflect.DeepEqual(got, want) {
		t.Fatalf("external graph = %#v, want %#v", got, want)
	}
}
