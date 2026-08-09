package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
        mixed: {env_files: [.env.staging]}
        silent: {env_files: []}
        quiet: {replicas: 2}
runtime:
  env_files: [.env]
workloads:
  web:    {role: application, image: nginx, domain: s.example.com, port: 3000}
  cron:   {role: job, image: nginx, command: ["true"], data_effect: none}
  quiet:  {role: worker, image: nginx, env_files: []}
  own:    {role: worker, image: nginx, env_files: [.env.own]}
  mixed:  {role: worker, image: nginx, env_files: [.env.own]}
  silent: {role: worker, image: nginx, env_files: [.env.own]}
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
	// Assert the list, not substrings of it. `strings.Contains(body, ".env")`
	// is satisfied by "own.env", so deleting the entire projection would leave
	// it green. A test for a projection has to look at what was projected, in
	// order.
	legacy := composeServiceEnvFiles(t, rendered.Bytes, "legacy")
	if len(legacy) != 2 || legacy[0] != "own.env" || legacy[1] != ".env" {
		t.Fatalf("env_file = %v, want the referenced entry first then the projection", legacy)
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

// No value from any entry reaches an artifact, whatever the entry's kind.
//
// Plaintext is not less sensitive than encrypted — it is less protected. The
// redaction rules were written when only the encrypted mechanism existed, and
// a contract treating "how it is stored" as "who may see it" would let the
// commoner form leak.
func TestNoEntryValueReachesAnArtifact(t *testing.T) {
	path := envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@203.0.113.10}}
runtime:
  env_files: [.env]
image: nginx
domain: s.example.com
port: 3000
`, map[string]string{".env": "API_TOKEN=super-secret-value\n"})

	r := resolvedFor(t, path, "production")
	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered.Bytes), "super-secret-value") {
		t.Error("a plaintext entry's value reached the generated runtime")
	}
	canonical, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "super-secret-value") {
		t.Error("a plaintext entry's value reached the canonical form")
	}
	// Referenced, never inlined: that is what keeps the value in one place
	// rather than copied into everything that describes the release.
	if !strings.Contains(string(rendered.Bytes), ".env") {
		t.Error("the entry should be referenced by path")
	}
}

// An http probe with nothing to probe is refused, not generated against port 0.
//
// The port default covers a workload that routes or publishes. A worker or a
// job using the bare path shorthand has neither, and the earlier fix left those
// generating `http://127.0.0.1:0/` — a check that can never pass, which a
// rolling release waits out in full before reporting the container unhealthy
// without naming a port.
func TestAProbeWithNoPortIsRefused(t *testing.T) {
	_, err := Load(envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  worker:
    role: worker
    image: nginx
    health: /healthz
`, nil))
	if err == nil {
		t.Fatal("a probe with no port to probe must be refused")
	}
	var e *Error
	if !asError(err, &e) || e.Code != "health_port_unknown" {
		t.Fatalf("want health_port_unknown, got %v", err)
	}
}

// composeServiceEnvFiles reads one service's env_file list out of a generated
// runtime, so a test can assert the list and its order rather than that some
// substring appears somewhere in the document.
func composeServiceEnvFiles(t *testing.T, runtime []byte, service string) []string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			EnvFile []string `yaml:"env_file"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(runtime, &doc); err != nil {
		t.Fatalf("generated runtime does not parse: %v", err)
	}
	return doc.Services[service].EnvFile
}

// The projection is what a compose-sourced workload receives, and its order is
// part of the contract: the author's own entries first, then what the overlay
// adds. Both halves were unguarded — deleting the projection outright left the
// suite green.
func TestTheProjectionAppendsAndPreservesOrder(t *testing.T) {
	path := envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@203.0.113.10}}
runtime:
  env_files: [.env.one, .env.two]
workloads:
  legacy:
    role: application
    compose: legacy.yml#legacy
    domain: s.example.com
    port: 80
`, map[string]string{
		".env.one":   "A=1\n",
		".env.two":   "B=2\n",
		"legacy.yml": "services:\n  legacy:\n    image: nginx\n    env_file: [own.env]\n",
		"own.env":    "C=3\n",
	})
	r := resolvedFor(t, path, "production")
	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := composeServiceEnvFiles(t, rendered.Bytes, "legacy")
	want := []string{"own.env", ".env.one", ".env.two"}
	if len(got) != len(want) {
		t.Fatalf("env_file = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("env_file = %v, want %v — declared order is the contract", got, want)
		}
	}
}

// A managed-service connection is applied after every declared entry, so it
// cannot be shadowed by one. Emitting them in the other order passed every
// test.
func TestConnectionFilesComeAfterDeclaredEntries(t *testing.T) {
	path := envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@203.0.113.10}}
runtime:
  env_files: [.env]
workloads:
  web:
    role: application
    image: nginx
    domain: s.example.com
    port: 3000
    needs: [postgres]
services:
  postgres: 17
`, map[string]string{".env": "A=1\n"})
	r := resolvedFor(t, path, "production")
	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := composeServiceEnvFiles(t, rendered.Bytes, "web")
	if len(got) < 2 || got[0] != ".env" {
		t.Fatalf("env_file = %v, want the declared entry first", got)
	}
	if !strings.Contains(got[len(got)-1], "postgres") {
		t.Fatalf("env_file = %v, want the connection file last so nothing authored shadows it", got)
	}
}

// A referenced service claiming a connection variable is refused. The inline
// half was tested; this half is a scenario stated twice in the contract and had
// no test — making the check unconditionally return nil passed everything.
func TestAReferencedServiceCannotClaimAConnectionVariable(t *testing.T) {
	_, err := resolvedForErr(t, envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@203.0.113.10}}
workloads:
  legacy:
    role: application
    compose: legacy.yml#legacy
    domain: s.example.com
    port: 80
    needs: [postgres]
services:
  postgres: 17
`, map[string]string{
		"legacy.yml": "services:\n  legacy:\n    image: nginx\n    environment:\n      POSTGRES_PASSWORD: mine\n",
	}))
	if err == nil {
		t.Fatal("a referenced service claiming a connection variable must be refused")
	}
	if !strings.Contains(err.Error(), "POSTGRES_PASSWORD") {
		t.Errorf("the refusal must name the variable: %v", err)
	}
}

// resolvedForErr renders and returns the error rather than failing the test.
func resolvedForErr(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	p, err := Load(path)
	if err != nil {
		return nil, err
	}
	r, err := p.Resolve("production")
	if err != nil {
		return nil, err
	}
	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		return nil, err
	}
	return rendered.Bytes, nil
}

// A project declaring an encrypted entry and referencing no Compose file needs
// no interpolation, and must load.
//
// Refusing the load whenever a document-scope entry is encrypted — on the
// reasoning that interpolation cannot read it — would be eager: nothing has
// asked for interpolation. Stopping a correct project from loading is a worse
// failure than the one it would prevent.
func TestAnEncryptedEntryDoesNotBlockAProjectThatNeedsNoInterpolation(t *testing.T) {
	path := envModelProject(t, `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@203.0.113.10}}
runtime:
  env_files: [{file: s.enc, provider: sops}]
image: nginx
domain: s.example.com
port: 3000
health: {http: /healthz, port: 3000}
`, map[string]string{"s.enc": "A=1\n"})
	// Through the function the callers use. `Load` does not reach it, so a test
	// that only loads would pass while the behaviour this names is broken.
	r := resolvedFor(t, path, "production")
	values, err := r.Spec.InterpolationEnv()
	if err != nil {
		t.Fatalf("a project needing no interpolation must not be refused: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("an encrypted entry must contribute nothing here, got %v", values)
	}
	// And the fact travels, so a caller can name it if a variable does go
	// unsupplied.
	if hidden := r.Spec.EncryptedDocumentEntries(); len(hidden) != 1 || hidden[0] != "s.enc" {
		t.Errorf("the unreadable entry must be reportable, got %v", hidden)
	}
}

// A workload's own list beats the environment's.
//
// The rule is one ordering — override, own, environment, project — and the
// middle pair is the one an author is most likely to have both of. Reading the
// environment first would hand every workload the same list and silently
// discard the one it declared for itself.
func TestAWorkloadsOwnListBeatsItsEnvironments(t *testing.T) {
	staging := resolvedFor(t, envModelProject(t, envModelBody, envModelFiles), "staging")
	if got := listFor(t, staging, "own"); len(got) != 1 || got[0] != ".env.own" {
		t.Errorf("own resolved %v on staging, want its own list, not the environment's", got)
	}
}

// An override replaces the list it overrides; it does not extend it.
//
// Concatenating instead would leave the workload still reading the file the
// override exists to stop it reading — and because a later entry wins key by
// key, whether the old values survive would depend on which keys they share.
// There would be no way to say "this and nothing else".
func TestAnOverrideReplacesRatherThanExtends(t *testing.T) {
	staging := resolvedFor(t, envModelProject(t, envModelBody, envModelFiles), "staging")
	got := listFor(t, staging, "mixed")
	if len(got) != 1 || got[0] != ".env.staging" {
		t.Errorf("the override resolved %v, want only the override's list", got)
	}
}

// An override declaring an empty list means none, and survives the patch.
//
// The list travels through a JSON round trip on the way to being applied, and
// `omitempty` drops an empty one — so the workload fell back to its own list
// and read the very file the override declined. "Receives nothing" has to be
// expressible, or there is no way to withdraw a credential in one environment.
func TestAnOverrideDeclaringNoneIsPreserved(t *testing.T) {
	staging := resolvedFor(t, envModelProject(t, envModelBody, envModelFiles), "staging")
	if got := listFor(t, staging, "silent"); len(got) != 0 {
		t.Errorf("the override declined every entry, but the workload resolved %v", got)
	}
	// And the environment it was not declared in is untouched.
	production := resolvedFor(t, envModelProject(t, envModelBody, envModelFiles), "production")
	if got := listFor(t, production, "silent"); len(got) != 1 || got[0] != ".env.own" {
		t.Errorf("production resolved %v, want the workload's own list", got)
	}
}

// An entry naming a file that is not there is refused at load.
//
// Compose fails on a missing env_file at the far end of a deploy, on the host,
// after the release is staged and the old one is coming down. The name is in
// the document; there is no reason to find out there.
func TestAnEntryNamingAMissingFileIsRefused(t *testing.T) {
	body := `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
runtime:
  env_files: [.env.absent]
workloads:
  web: {image: nginx, domain: s.example.com, port: 3000}
`
	_, err := Load(envModelProject(t, body, nil))
	if err == nil {
		t.Fatal("an entry naming a file that does not exist must be refused")
	}
	if !strings.Contains(err.Error(), "env_file_missing") {
		t.Errorf("the refusal must carry a code to branch on, got %v", err)
	}
	if !strings.Contains(err.Error(), ".env.absent") {
		t.Errorf("the refusal must name the file, got %v", err)
	}
}

// Overriding some other field leaves a declared empty list declared.
//
// The list survives the patch as JSON, where `omitempty` drops an empty one.
// So raising the replica count in one environment handed the workload back the
// project's list, and a workload deliberately holding no credentials was given
// them — by an edit that says nothing about credentials at all.
func TestOverridingAnotherFieldKeepsADeclaredEmptyList(t *testing.T) {
	staging := resolvedFor(t, envModelProject(t, envModelBody, envModelFiles), "staging")
	if got := listFor(t, staging, "quiet"); len(got) != 0 {
		t.Errorf("overriding replicas resolved %v, but the workload declared none", got)
	}
	if n := staging.Spec.Workloads["quiet"].Count(); n != 2 {
		t.Errorf("the override itself must still apply, replicas = %d", n)
	}
}
