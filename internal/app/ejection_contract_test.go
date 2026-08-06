package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ejectContractProject = `api_version: onebox.run/v1
app: shop
environments:
  production:
    server: root@203.0.113.10
runtime:
  env_files: [.env.production]
workloads:
  web:
    role: application
    image: nginx:1.27
    health: /healthz
    env:
      API_TOKEN: super-secret-value
    routes:
      - {domain: shop.example.com, path: /, port: 3000}
      - {domain: shop.example.com, path: /api, port: 3001}
  worker:
    role: worker
    image: nginx:1.27
    needs: [postgres]
services:
  postgres: 16
`

func ejectContractFixture(t *testing.T) (string, *Resolved) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env.production"), []byte("TOKEN=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ob.yml")
	if err := os.WriteFile(path, []byte(ejectContractProject), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	return dir, r
}

// 10.4 — the ejected runtime is something this contract can consume.
//
// Ejection strips Onebox's overlay, and the Compose-reference path refuses a
// service still carrying it. If stripping missed anything, ejection would hand
// back a project that can never generate again — and the author would find out
// on their next deploy, having already lost the generator.
func TestGenerationSucceedsImmediatelyAfterEjection(t *testing.T) {
	dir, r := ejectContractFixture(t)
	if _, err := r.Eject("compose.yaml", "r1", nil, false); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(filepath.Join(dir, "ob.yml"))
	if err != nil {
		t.Fatalf("the ejected project does not load: %v", err)
	}
	again, err := reloaded.Resolve("production")
	if err != nil {
		t.Fatalf("the ejected project does not resolve: %v", err)
	}
	if _, err := again.Render("production", "r2", nil); err != nil {
		t.Fatalf("the ejected project does not generate: %v", err)
	}
}

// 10.6 — an ejected service is used as written, not regenerated.
//
// Ejection is a handover: from that point the file is the author's. Silently
// re-deriving any part of it would mean their edits vanished on the next
// deploy, which is the one thing a handover must not do.
func TestEjectedServicesAreUsedAsAuthored(t *testing.T) {
	dir, r := ejectContractFixture(t)
	if _, err := r.Eject("compose.yaml", "r1", nil, false); err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(dir, "compose.yaml")
	body, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	// An edit only the author would make.
	edited := strings.ReplaceAll(string(body), "nginx:1.27", "nginx:1.29-alpine")
	if edited == string(body) {
		t.Fatal("fixture no longer contains the image; update this test")
	}
	if err := os.WriteFile(runtimePath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(filepath.Join(dir, "ob.yml"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := reloaded.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := again.Render("production", "r2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered.Bytes), "nginx:1.29-alpine") {
		t.Error("the author's edit did not survive: the ejected service was regenerated")
	}
	if strings.Contains(string(rendered.Bytes), "nginx:1.27") {
		t.Error("the pre-ejection image came back")
	}
}

// 10.7 — neither rendered nor ejected output puts a declared value on a
// terminal or in a file by accident.
//
// Redaction is only meaningful if it covers every form the value can take, so
// this checks the runtime, the per-service documents, and the file ejection
// writes into the repository.
func TestRedactionCoversRenderedAndEjectedOutput(t *testing.T) {
	dir, r := ejectContractFixture(t)
	rendered, err := r.Render("production", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The generated runtime carries values: it is what gets deployed. What
	// must never carry them is what a person or a pipeline reads, which the
	// CLI redacts — asserted there. Here: the credential of a managed service
	// is never in the runtime at all, because it is generated on the target.
	if strings.Contains(string(rendered.Bytes), "POSTGRES_PASSWORD=") {
		t.Error("a service credential appeared in the generated runtime")
	}
	for name, doc := range rendered.Services {
		if strings.Contains(string(doc), "$pw") && !strings.Contains(string(doc), "${") {
			t.Errorf("service %s: a credential placeholder leaked as a literal", name)
		}
	}

	// Ejection writes into the repository, where a secret would be committed.
	if _, err := r.Eject("compose.yaml", "r1", nil, false); err != nil {
		t.Fatal(err)
	}
	ejected, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ejected), "POSTGRES_PASSWORD=") {
		t.Error("a service credential was written into the repository by ejection")
	}
	// The env file is referenced, not inlined: its contents stay in one place.
	if strings.Contains(string(ejected), "TOKEN=v") {
		t.Error("an environment file's contents were inlined into the ejected runtime")
	}
}
