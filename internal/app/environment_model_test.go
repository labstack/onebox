package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One test per scenario of the contract's environment model. The delta's
// scenarios are the specification; a scenario without a test is a behaviour
// nobody checked.

func envModelProject(t *testing.T, body string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if d := filepath.Dir(name); d != "." {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "ob.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func resolvedFor(t *testing.T, path, env string) *Resolved {
	t.Helper()
	p, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r, err := p.Resolve(env)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return r
}

func listFor(t *testing.T, r *Resolved, workload string) []string {
	t.Helper()
	var out []string
	for _, e := range r.Spec.EnvFilesFor(r.Spec.Workloads[workload]) {
		out = append(out, e.File)
	}
	return out
}

const envModelBody = `api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@203.0.113.10}
  staging:
    server: root@203.0.113.20
    env_files: [.env.staging]
    overrides:
      workloads:
        db: {env_files: [.env.staging]}
runtime:
  env_files: [.env]
workloads:
  web:    {role: application, image: nginx, domain: s.example.com, port: 3000}
  cron:   {role: job, image: nginx, command: ["true"], data_effect: none}
  quiet:  {role: worker, image: nginx, env_files: []}
  own:    {role: worker, image: nginx, env_files: [.env.own]}
  db:     {role: daemon, image: postgres:16}
`

var envModelFiles = map[string]string{".env": "A=1\n", ".env.staging": "B=2\n", ".env.own": "C=3\n"}

// The default reaches the application's own workloads and not a daemon.
func TestTheDefaultReachesTheApplicationsOwnWorkloads(t *testing.T) {
	r := resolvedFor(t, envModelProject(t, envModelBody, envModelFiles), "production")
	for _, w := range []string{"web", "cron"} {
		if got := listFor(t, r, w); len(got) != 1 || got[0] != ".env" {
			t.Errorf("%s resolved %v, want the project's list", w, got)
		}
	}
	if got := listFor(t, r, "db"); len(got) != 0 {
		t.Errorf("daemon resolved %v, want none", got)
	}
}

// A workload's own list beats the default, and an explicit empty list declines.
func TestTheMostSpecificDeclarationWins(t *testing.T) {
	r := resolvedFor(t, envModelProject(t, envModelBody, envModelFiles), "production")
	if got := listFor(t, r, "own"); len(got) != 1 || got[0] != ".env.own" {
		t.Errorf("own resolved %v, want its own list", got)
	}
	if got := listFor(t, r, "quiet"); len(got) != 0 {
		t.Errorf("a declared empty list resolved %v, want none", got)
	}
}

// Environments carry different values, and an override reaches a daemon.
func TestEnvironmentsCarryDifferentValues(t *testing.T) {
	path := envModelProject(t, envModelBody, envModelFiles)
	staging := resolvedFor(t, path, "staging")
	for _, w := range []string{"web", "cron"} {
		if got := listFor(t, staging, w); len(got) != 1 || got[0] != ".env.staging" {
			t.Errorf("%s on staging resolved %v, want the environment's list", w, got)
		}
	}
	if got := listFor(t, staging, "db"); len(got) != 1 || got[0] != ".env.staging" {
		t.Errorf("the daemon's override resolved %v, want the override's list", got)
	}
	production := resolvedFor(t, path, "production")
	if got := listFor(t, production, "db"); len(got) != 0 {
		t.Errorf("the override leaked into production: %v", got)
	}
}

// Two entries never share a staged file.
func TestTwoEntriesNeverShareAStagedFile(t *testing.T) {
	a := EnvFile{File: "a/s.env", Provider: "sops"}
	b := EnvFile{File: "a-s.env", Provider: "sops"}
	if a.StagedPath() == b.StagedPath() {
		t.Fatalf("both entries stage to %q — one would replace the other", a.StagedPath())
	}
	if plain := (EnvFile{File: "a/s.env"}).StagedPath(); plain != "a/s.env" {
		t.Errorf("a plaintext entry is staged at its own path, got %q", plain)
	}
}

// The withdrawn block is refused with direction, not as an unknown field.
func TestTheWithdrawnSecretsBlockIsRefusedWithDirection(t *testing.T) {
	_, err := Load(envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
image: nginx
secrets: {production: s.yaml}
`, nil))
	if err == nil {
		t.Fatal("the withdrawn block must be refused")
	}
	var e *Error
	if !asError(err, &e) || e.Code != "secrets_withdrawn" {
		t.Fatalf("want secrets_withdrawn, got %v", err)
	}
	if !strings.Contains(e.Next, "provider: sops") {
		t.Errorf("the refusal must name the replacement form: %q", e.Next)
	}
}

// An authored value may not claim a name a connection supplies.
func TestAuthoredValuesCannotClaimAConnectionVariable(t *testing.T) {
	_, err := Load(envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web:
    role: application
    image: nginx
    domain: s.example.com
    port: 3000
    needs: [postgres]
    env: {POSTGRES_PASSWORD: mine}
services:
  postgres: 17
`, nil))
	if err == nil {
		t.Fatal("an inline env claiming a connection variable must be refused")
	}
	var e *Error
	if !asError(err, &e) || e.Code != "connection_variable_claimed" {
		t.Fatalf("want connection_variable_claimed, got %v", err)
	}
}

// A compose-sourced application receives what an image-sourced one receives,
// and ejecting then generating does not duplicate the projection.
func TestComposeSourcedWorkloadsAreNotASpecialCase(t *testing.T) {
	path := envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@203.0.113.10}}
runtime:
  env_files: [.env]
workloads:
  legacy:
    role: application
    compose: legacy.yml#legacy
    domain: s.example.com
    port: 80
  web:
    role: worker
    image: nginx
`, map[string]string{
		".env":       "A=1\n",
		"legacy.yml": "services:\n  legacy:\n    image: nginx\n    env_file: [own.env]\n",
		"own.env":    "B=2\n",
	})
	r := resolvedFor(t, path, "production")
	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := string(rendered.Bytes)
	if !strings.Contains(body, "own.env") || !strings.Contains(body, ".env") {
		t.Fatalf("the referenced entry and the projection must both appear:\n%s", body)
	}

	if _, err := r.Eject("compose.yaml", "R1", nil, false); err != nil {
		t.Fatal(err)
	}
	ejected, err := os.ReadFile(filepath.Join(filepath.Dir(path), "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The generated workload is the one ejected; its env_file was entirely
	// projected, so nothing of it should survive.
	if strings.Contains(string(ejected), ".env") {
		t.Errorf("ejection left a projected entry behind, so generating again would duplicate it:\n%s", ejected)
	}
}

// An HTTP probe with no port of its own probes the port the workload routes on.
//
// Without this the shorthand `health: /healthz` — which carries a path and
// nothing else — generated a check against port 0. It could never pass, so a
// rolling release waited out its entire budget and then reported the container
// unhealthy, naming the container and saying nothing about the port.
func TestAnHTTPProbeInheritsTheRoutedPort(t *testing.T) {
	r := resolvedFor(t, envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@203.0.113.10}}
image: nginx
domain: s.example.com
port: 3000
health: /healthz
`, nil), "production")
	if got := r.Spec.Workloads["shop"].Health.Port; got != 3000 {
		t.Fatalf("probe port = %d, want the routed port 3000", got)
	}
	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered.Bytes), "127.0.0.1:0/") {
		t.Error("the generated probe still targets port 0")
	}
}
