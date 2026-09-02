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

func TestWorkloadContractsTrackRelativeBindMountContent(t *testing.T) {
	staging := t.TempDir()
	for name, body := range map[string]string{"api-conf/app.yml": "mode: api\n", "worker-conf/app.yml": "mode: worker\n"} {
		path := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := app.LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api: {role: application, image: example/api, volumes: [{source: ./api-conf, path: /conf, mode: ro}]}
  worker: {role: worker, image: example/worker, volumes: [{source: ./worker-conf, path: /conf, mode: ro}]}
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
	if before["api"].StartupRevision == "" {
		t.Fatal("a workload with a relative bind mount has no startup revision")
	}
	if err := os.WriteFile(filepath.Join(staging, "api-conf", "app.yml"), []byte("mode: changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := workloadContracts(resolved, staging, staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before["api"].StartupRevision == after["api"].StartupRevision {
		t.Fatal("changed bind-mount content retained its startup revision")
	}
	if before["worker"].StartupRevision != after["worker"].StartupRevision {
		t.Fatal("api bind-mount change affected the worker startup revision")
	}
}

func TestBindMountContractIsIndependentOfWhereTheReleaseIsStaged(t *testing.T) {
	spec, err := app.LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api: {role: application, image: example/api, volumes: [{source: ./conf, path: /conf, mode: ro}]}
deployment: {order: [api]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	revision := func() string {
		t.Helper()
		staging := t.TempDir()
		if err := os.MkdirAll(filepath.Join(staging, "conf"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staging, "conf", "app.yml"), []byte("mode: api\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		contracts, err := workloadContracts(resolved, staging, staging, nil)
		if err != nil {
			t.Fatal(err)
		}
		return contracts["api"].StartupRevision
	}
	if first, second := revision(), revision(); first != second {
		t.Fatalf("identical bind content staged twice produced different revisions: %s vs %s", first, second)
	}
}

func TestWorkloadContractsIgnoreVolumesOnAnAdoptedComposeService(t *testing.T) {
	staging := t.TempDir()
	spec, err := app.LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api: {role: application, compose: docker-compose.yml#api, volumes: [{source: ./api-conf, path: /conf, mode: ro}]}
deployment: {order: [api]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workloadContracts(resolved, staging, staging, nil); err != nil {
		t.Fatalf("an adopted Compose service renders verbatim, so its declared volumes stage nothing: %v", err)
	}
}

func TestBindMountContractNoticesAnAddedEmptyDirectory(t *testing.T) {
	spec, err := app.LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: deploy@example.test}}
workloads:
  api: {role: application, image: example/api, volumes: [{source: ./conf, path: /conf, mode: ro}]}
deployment: {order: [api]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staging, "conf"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "conf", "app.yml"), []byte("mode: api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := workloadContracts(resolved, staging, staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staging, "conf", "flows"), 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := workloadContracts(resolved, staging, staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before["api"].StartupRevision == after["api"].StartupRevision {
		t.Fatal("a directory added to a bind source retained its startup revision")
	}
}
