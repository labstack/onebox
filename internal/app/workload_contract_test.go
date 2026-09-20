package app

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkloadRevisionIgnoresSecretStorageGeneration(t *testing.T) {
	const first = "sg-111111111111111111111111"
	const second = "sg-222222222222222222222222"
	graph := []SecretDeclaration{{OutputPath: "app.env", AffectedWorkloads: []string{"worker"}}}
	compose := []byte(`services:
  worker:
    image: example/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    env_file: [.ob-secret-generations/` + first + `/app.env]
    labels: {ob.app: sample, ob.release: R1, ob.secret-generation: ` + first + `}
`)
	contract := map[string]WorkloadContract{"worker": {
		SecretRevision:      first,
		SecretInputRevision: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	firstCompose, err := ApplyWorkloadContracts(compose, graph, contract)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := ApplySecretGeneration(firstCompose, graph, second)
	if err != nil {
		t.Fatal(err)
	}
	secondCompose, err := ApplyWorkloadContracts(moved, graph, contract)
	if err != nil {
		t.Fatal(err)
	}
	if workloadRevision(t, firstCompose, "worker") != workloadRevision(t, secondCompose, "worker") {
		t.Fatal("moving identical secret inputs to another storage generation changed the workload revision")
	}
	contract["worker"] = WorkloadContract{SecretRevision: second, SecretInputRevision: contract["worker"].SecretInputRevision}
	changed, err := ApplyWorkloadContracts(moved, graph, contract)
	if err != nil {
		t.Fatal(err)
	}
	if workloadRevision(t, firstCompose, "worker") == workloadRevision(t, changed, "worker") {
		t.Fatal("changed secret identity did not change the workload revision")
	}
}

func TestSecretInputRevisionsAreScopedByWorkload(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{"api.enc.env": "cipher-api", "worker.enc.env": "cipher-worker"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api: {role: application, image: example/api, env_files: [{file: api.enc.env, provider: sops}]}
  worker: {role: worker, image: example/worker, env_files: [{file: worker.enc.env, provider: sops}]}
deployment: {order: [api, worker]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	before, err := resolved.SecretInputRevisions(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "api.enc.env"), []byte("cipher-api-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := resolved.SecretInputRevisions(root)
	if err != nil {
		t.Fatal(err)
	}
	if before["api"] == after["api"] {
		t.Fatal("changed api source retained its secret input revision")
	}
	if before["worker"] != after["worker"] {
		t.Fatal("api source change affected the worker revision")
	}
}

func TestSecretInputRevisionsUseTheProvidedSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "api.enc.env")
	if err := os.WriteFile(path, []byte("cipher-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api: {role: application, image: example/api, env_files: [{file: api.enc.env, provider: sops}]}
deployment: {order: [api]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := map[string][]byte{"api.enc.env": []byte("cipher-before")}
	before, err := resolved.SecretInputRevisionsFromSources(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("cipher-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := resolved.SecretInputRevisionsFromSources(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if before["api"] != after["api"] {
		t.Fatal("mutable source path changed the revision of an immutable snapshot")
	}
}

func workloadRevision(t *testing.T, body []byte, workload string) string {
	t.Helper()
	var document struct {
		Services map[string]struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	return document.Services[workload].Labels[WorkloadRevisionLabel]
}
