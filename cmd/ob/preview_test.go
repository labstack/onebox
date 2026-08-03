package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const previewProject = `api_version: onebox.run/v1
app: demo
environments:
  production: {server: root@1.2.3.4}
  staging: {server: root@5.6.7.8, overrides: {workloads: {web: {replicas: 1}}}}
workloads:
  web:
    role: application
    image: nginx:1.27
    replicas: 3
    domain: demo.example.com
    port: 8080
    env: {API_TOKEN: super-secret-value, LOG_LEVEL: info}
`

func TestPreviewRendersAndRedacts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ob.yml", previewProject)

	out, err := run(t, dir, "preview")
	if err != nil {
		t.Fatalf("preview failed: %v\n%s", err, out)
	}
	for _, want := range []string{"# digest ", "name: demo", "nginx:1.27", "ob.app: demo"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
	// A preview must never put a declared secret on a terminal.
	if strings.Contains(out, "super-secret-value") {
		t.Errorf("preview leaked an environment value:\n%s", out)
	}
	if !strings.Contains(out, "redacted") {
		t.Errorf("expected redaction to be visible:\n%s", out)
	}
}

func TestPreviewRawShowsValues(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ob.yml", previewProject)

	out, err := run(t, dir, "preview", "--raw")
	if err != nil {
		t.Fatalf("preview --raw failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "super-secret-value") {
		t.Errorf("--raw should show values:\n%s", out)
	}
}

// TestPreviewAppliesEnvironmentOverrides: previewing staging must show staging.
func TestPreviewAppliesEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ob.yml", previewProject)

	prod, err := run(t, dir, "preview", "--digest")
	if err != nil {
		t.Fatal(err)
	}
	stage, err := run(t, dir, "preview", "--digest", "-e", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(prod) == strings.TrimSpace(stage) {
		t.Fatal("staging and production rendered the same runtime despite an override")
	}
}

// TestPreviewFailureIsActionable: an agent reading this must learn what is
// wrong, where, and what to run.
func TestPreviewFailureIsActionable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ob.yml", `api_version: onebox.run/v1
app: demo
environments: {production: {server: h}}
workloads: {web: {role: application, build: ., domain: d.example.com, port: 80}}
`)
	out, err := run(t, dir, "preview")
	if err == nil {
		t.Fatal("expected a build-sourced workload with no image to fail")
	}
	combined := out + err.Error()
	for _, want := range []string{"image_unresolved", "workloads.web", "ob release"} {
		if !strings.Contains(combined, want) {
			t.Errorf("failure should carry %q:\n%s", want, combined)
		}
	}
}

// TestPreviewTouchesNothing guards the promise in the command's own help.
func TestPreviewTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ob.yml", previewProject)
	before := dirEntries(t, dir)

	if _, err := run(t, dir, "preview"); err != nil {
		t.Fatal(err)
	}
	if after := dirEntries(t, dir); after != before {
		t.Fatalf("preview wrote to the project directory: %d entries before, %d after", before, after)
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirEntries(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// TestEjectPicksAFreeName. A flag default of "compose.yaml" would refuse an
// ordinary ejection in any project that already references that file — which
// several real ones do.
func TestEjectPicksAFreeName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "compose.yaml", "services:\n  db: {image: postgres}\n")
	writeFile(t, dir, "ob.yml", `api_version: onebox.run/v1
app: ledger
environments: {production: {server: root@1.2.3.4}}
workloads:
  web: {role: application, image: nginx, domain: d.example.com, port: 80}
  db:  {role: daemon, compose: "compose.yaml#db"}
`)
	out, err := run(t, dir, "eject")
	if err != nil {
		t.Fatalf("eject should choose a free name: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.ob.yaml")); err != nil {
		t.Fatalf("expected a non-colliding file to be written: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if !strings.Contains(string(body), "postgres") {
		t.Error("the referenced file must be left alone")
	}
}
