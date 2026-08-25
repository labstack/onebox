package onebox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

func TestWorkloadContractsScopePlainEnvironmentChanges(t *testing.T) {
	staging := t.TempDir()
	for name, body := range map[string]string{"api.env": "MODE=api\n", "worker.env": "MODE=worker\n"} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := app.LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api: {role: application, image: example/api, env_files: [api.env]}
  worker: {role: worker, image: example/worker, env_files: [worker.env]}
deployment: {order: [api, worker]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	before, err := workloadContracts(resolved, staging, staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "api.env"), []byte("MODE=changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := workloadContracts(resolved, staging, staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before["api"].StartupRevision == after["api"].StartupRevision {
		t.Fatal("changed api environment file retained its startup revision")
	}
	if before["worker"].StartupRevision != after["worker"].StartupRevision {
		t.Fatal("api environment change affected the worker startup revision")
	}
}

func TestSecretSourceSnapshotIsSharedAcrossConsumers(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "shared.enc.env")
	if err := os.WriteFile(file, []byte("cipher-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots := map[string][]byte{}
	first, err := secretSourceSnapshot(root, "shared.enc.env", snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("cipher-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := secretSourceSnapshot(root, "shared.enc.env", snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "cipher-before" || string(second) != string(first) {
		t.Fatalf("snapshot changed across consumers: first=%q second=%q", first, second)
	}
}
