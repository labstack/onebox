package app

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

func TestEjectRepointsTheExplicitProjectPathOnly(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "deploy.project.yml")
	siblingDefault := filepath.Join(dir, "ob.yml")
	if err := os.WriteFile(explicit, []byte(ejectProject), 0o600); err != nil {
		t.Fatal(err)
	}
	const untouched = "this is deliberately not a project\n"
	if err := os.WriteFile(siblingDefault, []byte(untouched), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(explicit)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := p.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.Eject("compose.yaml", "r1", nil, false); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(explicit)
	if err != nil || !strings.Contains(string(body), "compose: compose.yaml#web") {
		t.Fatalf("explicit project was not repointed: %v\n%s", err, body)
	}
	defaultBody, err := os.ReadFile(siblingDefault)
	if err != nil || string(defaultBody) != untouched {
		t.Fatalf("sibling ob.yml changed: %v\n%s", err, defaultBody)
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
    volumes: [{name: uploads, path: /var/lib/ledger/uploads}]
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

// TestEjectDefaultAvoidsAReferencedFile. Several real projects reference
// compose.yaml already; defaulting on top of it turned an ordinary ejection
// into a refusal the author had done nothing to deserve.
func TestEjectDefaultAvoidsAReferencedFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  db: {image: postgres}\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(`api_version: onebox.run/v1
app: ledger
environments: {production: {server: root@1.2.3.4}}
workloads:
  web: {role: application, image: nginx, domain: d.example.com, port: 80}
  db:  {role: daemon, compose: "compose.yaml#db"}
`), 0o600)

	p, err := Load(filepath.Join(dir, "ob.yml"))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := p.Resolve("production")
	res, err := r.Eject("", "r1", nil, false)
	if err != nil {
		t.Fatalf("ejection should pick a free name, not refuse: %v", err)
	}
	if res.Runtime == "compose.yaml" {
		t.Fatal("ejection chose a file the project already references")
	}
	if _, err := Load(filepath.Join(dir, "ob.yml")); err != nil {
		t.Fatalf("the project should still load: %v", err)
	}
}

// Ejection writes the runtime and renames it into place before the project is
// repointed, so an interruption between the two leaves the project pointing at
// the generator rather than at a file that may not exist.
//
// Re-running after that interruption completes: the workloads still declare
// their own sources, so the runtime is regenerated over the orphan and the
// project is repointed. What must not happen is a refusal the author cannot
// act on, or a project referencing a file that was never placed.
func TestEjectAfterAnInterruptionCompletes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ob.yml")
	body := `api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@203.0.113.10}
workloads:
  web: {role: application, image: nginx, domain: shop.example.com, port: 3000}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The state a crash between rename and repoint leaves behind: the runtime
	// is on disk, the project still declares its own source.
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
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
	res, err := r.Eject("compose.yaml", "r1", nil, true)
	if err != nil {
		t.Fatalf("re-running after an interruption must complete: %v", err)
	}
	if len(res.Workloads) != 1 || res.Workloads[0] != "web" {
		t.Fatalf("the workload was not handed over: %+v", res.Workloads)
	}
	// And no temporary file survives to be mistaken for the runtime.
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml.ob-tmp")); !os.IsNotExist(err) {
		t.Error("a temporary runtime was left behind")
	}
	// The project now references the file that is actually on disk.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if ref := reloaded.Workloads["web"].Compose; ref != "compose.yaml#web" {
		t.Fatalf("project was not repointed at the placed file: %q", ref)
	}
}
