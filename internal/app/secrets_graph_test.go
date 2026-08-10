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
	if len(graph) != 2 {
		t.Fatalf("graph = %+v", graph)
	}
	if graph[0].SourceFile != "shared.enc.env" || graph[0].Provider != "sops" || graph[0].Order != 0 ||
		graph[0].Scope != "runtime-default" || graph[0].ID == "" || !reflect.DeepEqual(graph[0].AffectedWorkloads, []string{"web", "worker"}) {
		t.Fatalf("first declaration = %+v", graph[0])
	}
	if graph[1].Order != 1 || graph[1].SourceFile != "later.enc.env" {
		t.Fatalf("second declaration = %+v", graph[1])
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
	if len(first) != 1 || first[0].ID != second[0].ID || len(first[0].ID) != len("secret_")+12 || first[0].ID[:7] != "secret_" {
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
	want := secretGraphProject(t, base).SecretDeclarationGraph()
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
