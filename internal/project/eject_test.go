package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ejectProject = `# Ledger's production contract.
api_version: onebox.run/v1
app: ledger

environments:
  # The only host that matters.
  production: {server: root@1.2.3.4}

workloads:
  web:
    role: application
    image: nginx:1.27   # pinned deliberately
    domain: ledger.example.com
    port: 8080
`

func ejectInto(t *testing.T, body string) (dir string, res *EjectResult) {
	t.Helper()
	dir = t.TempDir()
	path := filepath.Join(dir, "ob.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
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
	res, err = r.Eject("compose.yaml", "r1", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return dir, res
}

// TestEjectedRuntimeIsOrdinaryCompose. The overlay must not survive: on the
// next generation these workloads take the Compose-reference path, and that
// path refuses a service already carrying Onebox's own keys. Without stripping,
// ejection produces a project that can never generate again.
func TestEjectedRuntimeIsOrdinaryCompose(t *testing.T) {
	dir, res := ejectInto(t, ejectProject)
	body, err := os.ReadFile(filepath.Join(dir, res.Runtime))
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	for _, forbidden := range []string{"ob.app", "ob.release", "ob.workload", "traefik.", "ob-ingress"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("ejected runtime still carries %q\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "nginx:1.27") {
		t.Errorf("the workload itself should survive\n%s", out)
	}
}

// TestEjectedProjectStillGenerates is the property the stripping exists for.
func TestEjectedProjectStillGenerates(t *testing.T) {
	dir, _ := ejectInto(t, ejectProject)

	p, err := Load(filepath.Join(dir, "ob.yml"))
	if err != nil {
		t.Fatalf("the project should still load after ejection: %v", err)
	}
	if got := p.Workloads["web"].Compose; got != "compose.yaml#web" {
		t.Fatalf("the workload should now reference the ejected file, got %q", got)
	}
	if _, err := p.Render("production", "r2", nil); err != nil {
		t.Fatalf("an ejected project must still generate: %v", err)
	}
}

// TestEjectPreservesComments. The project file is maintained by hand and its
// comments say why things are the way they are. A decode-and-re-encode would
// discard every one of them silently.
func TestEjectPreservesComments(t *testing.T) {
	dir, _ := ejectInto(t, ejectProject)
	body, err := os.ReadFile(filepath.Join(dir, "ob.yml"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	for _, comment := range []string{
		"Ledger's production contract.",
		"The only host that matters.",
		"pinned deliberately",
	} {
		if !strings.Contains(out, comment) {
			t.Errorf("ejection discarded the comment %q\n%s", comment, out)
		}
	}
}

// TestEjectRefusesToClobber: a file already at the destination is somebody's
// work.
func TestEjectRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ob.yml")
	os.WriteFile(path, []byte(ejectProject), 0o600)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o600)

	p, _ := Load(path)
	r, _ := p.Resolve("production")
	_, err := r.Eject("compose.yaml", "r1", nil, false)
	var e *Error
	if !asError(err, &e) || e.Code != "eject_destination_exists" {
		t.Fatalf("got %v, want eject_destination_exists", err)
	}

	// The project must be untouched by a refused ejection.
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "image: nginx:1.27") {
		t.Error("a refused ejection must not modify the project")
	}
}

// TestEjectIsOneWay: a second ejection has nothing left to move.
func TestEjectIsOneWay(t *testing.T) {
	dir, _ := ejectInto(t, ejectProject)
	p, _ := Load(filepath.Join(dir, "ob.yml"))
	r, _ := p.Resolve("production")

	_, err := r.Eject("other.yaml", "r1", nil, false)
	var e *Error
	if !asError(err, &e) || e.Code != "eject_nothing_to_do" {
		t.Fatalf("got %v, want eject_nothing_to_do", err)
	}
}

// TestEjectCarriesTheAuthorsNote. The source key is replaced, and any comment
// on it described that key — deleting it with the line it sat on loses the
// author's reasoning silently, which is the failure ejection exists to avoid.
func TestEjectCarriesTheAuthorsNote(t *testing.T) {
	dir, _ := ejectInto(t, ejectProject)
	body, _ := os.ReadFile(filepath.Join(dir, "ob.yml"))
	out := string(body)
	if !strings.Contains(out, "pinned deliberately") {
		t.Fatalf("the note on the replaced key was lost\n%s", out)
	}
	if !strings.Contains(out, "compose: compose.yaml#web") {
		t.Fatalf("the workload was not repointed\n%s", out)
	}
}

// TestEjectRemovesWhatTheComposeFileNowOwns. A Compose-referenced workload is
// shaped by the file. Leaving a health check or a volume in the project would
// let someone edit it, see no effect, and get no error.
func TestEjectRemovesWhatTheComposeFileNowOwns(t *testing.T) {
	dir, _ := ejectInto(t, `api_version: onebox.run/v1
app: ledger
environments: {production: {server: root@1.2.3.4}}
workloads:
  web:
    role: application
    image: nginx
    domain: ledger.example.com
    port: 8080
    health: {http: /healthz, port: 8080}
    volumes: [uploads]
    env: {LOG_LEVEL: info}
`)
	body, _ := os.ReadFile(filepath.Join(dir, "ob.yml"))
	out := string(body)
	for _, gone := range []string{"health:", "volumes:", "env:", "image:"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q still in the project after ejection, where it no longer has effect\n%s", gone, out)
		}
	}
	// What the overlay still derives must stay.
	for _, kept := range []string{"role: application", "domain:", "port:", "compose:"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q should have been kept\n%s", kept, out)
		}
	}
}
