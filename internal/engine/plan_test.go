package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

func planFake() *transport.Fake {
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R0\n"}, true
		case strings.Contains(cmd, "service='web'") && strings.Contains(cmd, "docker ps"):
			return transport.Result{Stdout: "OLD1\n"}, true
		case strings.Contains(cmd, "service='worker'") && strings.Contains(cmd, "docker ps"):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(cmd, "service='migrate'") && strings.Contains(cmd, "docker ps"):
			return transport.Result{Stdout: "\n"}, true
		case strings.Contains(cmd, "{{.Image}}"):
			return transport.Result{Stdout: "sha256:aaaa\n"}, true
		case strings.Contains(cmd, "imagetools inspect"):
			return transport.Result{Stdout: "sha256:" + strings.Repeat("ab", 32) + "\n"}, true
		case strings.Contains(cmd, "cat "):
			return transport.Result{Stdout: "services: {}\n"}, true
		}
		return transport.Result{}, false
	}
	return f
}

func TestRefreshCollectsDriftSet(t *testing.T) {
	f := planFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	hs, err := e.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hs.CurrentRelease != "R0" {
		t.Fatalf("current: %q", hs.CurrentRelease)
	}
	if hs.ImageIDs["web"] != "sha256:aaaa" || hs.ImageIDs["worker"] != "sha256:aaaa" {
		t.Fatalf("image ids: %+v", hs.ImageIDs)
	}
	if _, ok := hs.ImageIDs["migrate"]; ok {
		t.Fatalf("stopped job service must not appear: %+v", hs.ImageIDs)
	}
}

func TestPinImagesRewritesToDigest(t *testing.T) {
	f := planFake()
	p := testProject(t)
	e := New(testConfig(), p, f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	pins, err := e.PinImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/x/app@sha256:" + strings.Repeat("ab", 32)
	for _, service := range []string{"web", "worker", "migrate"} {
		if pins[service] != want {
			t.Fatalf("%s pin: %q want %q", service, pins[service], want)
		}
		if p.Services[service].Image != want {
			t.Fatalf("%s project image not rewritten: %q", service, p.Services[service].Image)
		}
	}
	inspections := 0
	for _, command := range f.Commands {
		if strings.Contains(command, "imagetools inspect") {
			inspections++
		}
	}
	if inspections != 1 {
		t.Fatalf("identical image references must resolve once, got %d inspections:\n%s", inspections, strings.Join(f.Commands, "\n"))
	}
}

func TestPinImagesFailsClosedWhenRegistryCannotResolveDigest(t *testing.T) {
	f := planFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "imagetools inspect") {
			return transport.Result{ExitCode: 1, Stderr: "buildx: not found"}, true
		}
		return base(cmd)
	}
	p := testProject(t)
	e := New(testConfig(), p, f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	_, err := e.PinImages(context.Background())
	var resolution *ImageResolutionError
	if !errors.As(err, &resolution) {
		t.Fatalf("error = %v, want ImageResolutionError", err)
	}
	if resolution.Code() != "image_unresolved" || resolution.Workload == "" ||
		!strings.Contains(resolution.ResolvingCommand, "ob deploy --image "+resolution.Workload+"=") {
		t.Fatalf("typed resolution error = %#v", resolution)
	}
}

func TestPinImagesFailsClosedForBuildOnlyRuntimeService(t *testing.T) {
	p := testProject(t)
	service := p.Services["migrate"]
	service.Image = ""
	p.Services["migrate"] = service
	e := New(testConfig(), p, planFake(), Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	_, err := e.PinImages(context.Background())
	var resolution *ImageResolutionError
	if !errors.As(err, &resolution) || resolution.Workload != "migrate" {
		t.Fatalf("error = %v, want migrate ImageResolutionError", err)
	}
	if !strings.Contains(resolution.Detail, "build source") {
		t.Fatalf("error does not explain the unresolved build input: %#v", resolution)
	}
}

func TestPinImagesAcceptsAlreadyImmutableRuntimeWithoutRegistry(t *testing.T) {
	p := testProject(t)
	digest := "sha256:" + strings.Repeat("cd", 32)
	for name, service := range p.Services {
		service.Image = "ghcr.io/x/app@" + digest
		p.Services[name] = service
	}
	f := &transport.Fake{Err: func(command string) error {
		if strings.Contains(command, "imagetools inspect") {
			return errors.New("registry must not be consulted")
		}
		return nil
	}}
	e := New(testConfig(), p, f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	pins, err := e.PinImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != len(testConfig().Workloads) {
		t.Fatalf("pins = %#v", pins)
	}
}

func TestArtifactRoundtripAndBinding(t *testing.T) {
	a := &Artifact{
		ID: "R1", App: "sample", Env: "production",
		ConfigHash:      HashBytes([]byte("cfg")),
		HostState:       HostState{CurrentRelease: "R0", ImageIDs: map[string]string{"web": "sha256:aaaa"}},
		RenderedCompose: "services: {}\n",
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := a.Save(path); err != nil {
		t.Fatal(err)
	}
	b, err := LoadArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "R1" || b.HostState.ImageIDs["web"] != "sha256:aaaa" {
		t.Fatalf("roundtrip: %+v", b)
	}

	// binding: config changed
	if err := b.VerifyBinding("production", []byte("cfg-CHANGED"), b.HostState); err == nil {
		t.Fatal("config change must refuse")
	}
	// binding: drift
	drifted := HostState{CurrentRelease: "R0", ImageIDs: map[string]string{"web": "sha256:bbbb"}}
	if err := b.VerifyBinding("production", []byte("cfg"), drifted); err == nil {
		t.Fatal("host drift must refuse")
	}
	// binding: env mismatch
	if err := b.VerifyBinding("staging", []byte("cfg"), b.HostState); err == nil {
		t.Fatal("env mismatch must refuse")
	}
	// binding: clean
	if err := b.VerifyBinding("production", []byte("cfg"), b.HostState); err != nil {
		t.Fatalf("clean binding refused: %v", err)
	}
}

func TestDescribeShowsBranchesAndHooks(t *testing.T) {
	cfg := testConfig()
	cfg.Hooks["pre_release"] = app.Command{Run: "bun run build", Local: true}
	// A bootstrap hook belongs to `ob bootstrap`, not deploy — it must NOT
	// appear in the deploy plan (that would claim a step that never runs).
	cfg.Hooks["bootstrap"] = app.Command{Run: "scripts/bootstrap.sh", Local: true}
	e := New(cfg, testProject(t), planFake(), Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	lines := strings.Join(e.Describe("<release>/compose.yaml"), "\n")
	for _, want := range []string{
		"--scale web=<+1>",
		"unhealthy/timeout →",                  // the branch
		"--force-recreate --timeout 30 worker", // recreate role
		"job migrate (gated",                   // the migrate job, auto-run + gated
		"hook pre_release (local, unplannable", // a real hook still flagged
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("describe missing %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "hook bootstrap") {
		t.Fatalf("bootstrap hook must not appear in a deploy plan:\n%s", lines)
	}
}

func TestOnlyReleaseLabelsChanged(t *testing.T) {
	live := "services:\n  server:\n    labels:\n      ob.app: sample\n      ob.release: 20260704-203351-f65179e\n    image: x:1\n"
	relabel := "services:\n  server:\n    labels:\n      ob.app: sample\n      ob.release: 20260704-214927-f65179e\n    image: x:1\n"
	changed := "services:\n  server:\n    labels:\n      ob.app: sample\n      ob.release: 20260704-214927-f65179e\n    image: x:2\n"

	if !OnlyReleaseLabelsChanged(live, relabel) {
		t.Fatal("label-only change must be detected as content-identical")
	}
	if OnlyReleaseLabelsChanged(live, changed) {
		t.Fatal("an image change must NOT read as label-only")
	}
	// first deploy: empty live is a real change
	if OnlyReleaseLabelsChanged("", relabel) {
		t.Fatal("empty live vs planned must not read as label-only")
	}
	// identical bytes: caller handles diff=="" first, but the helper must not lie
	if !OnlyReleaseLabelsChanged(live, live) {
		t.Fatal("identical input is trivially label-invariant")
	}
}

func TestPayloadDigests(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yaml", "services: {}\n") // excluded: compared label-invariantly
	write("ob.snapshot.yml", "app: sample\n")
	write("server/.env", "KEY=one\n")

	d1, err := LocalPayloadDigest(testConfig(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// compose.yaml changes must NOT move the digest
	write("compose.yaml", "services: {changed: {}}\n")
	d2, err := LocalPayloadDigest(testConfig(), dir)
	if err != nil || d1 != d2 {
		t.Fatalf("compose.yaml must be excluded: %s vs %s (%v)", d1, d2, err)
	}
	// payload changes MUST move it
	write("server/.env", "KEY=two\n")
	d3, err := LocalPayloadDigest(testConfig(), dir)
	if err != nil || d3 == d1 {
		t.Fatalf("payload change must move the digest: %v", err)
	}
	write(".job-migrate-result/result", "changed=true\n")
	d4, err := LocalPayloadDigest(testConfig(), dir)
	if err != nil || d4 != d3 {
		t.Fatalf("ephemeral job results must be excluded: %s vs %s (%v)", d3, d4, err)
	}

	// the remote side runs the equivalent shell pipeline; assert the command
	// shape and that the fake's answer is returned verbatim
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "sha256sum") && strings.Contains(cmd, "releases/R7") {
			return transport.Result{Stdout: "abc123  -\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	got, err := e.RemotePayloadDigest(context.Background(), "R7")
	if err != nil || got != "abc123" {
		t.Fatalf("remote digest: %q %v", got, err)
	}
	seq := strings.Join(f.Commands, "\n")
	for _, want := range []string{
		"! -path './compose.yaml'", "! -path './manifest.json'",
		"! -path './.job-migrate-result'", "! -path './.job-migrate-result/*'", "LC_ALL=C sort",
	} {
		if !strings.Contains(seq, want) {
			t.Fatalf("remote pipeline missing %q:\n%s", want, seq)
		}
	}
	// Decrypted secrets are staged into this directory, so excluding it would
	// drop every secret byte from the digest on both sides.
	if strings.Contains(seq, app.SecretGenerationDirectory) {
		t.Fatalf("remote pipeline excludes staged secret payload:\n%s", seq)
	}
}

// A release directory is a staging directory plus what the lifecycle writes to
// it afterwards. If those extras count as payload the two digests can never be
// equal and no deploy is ever a no-op. The inverse matters just as much:
// .ob-secret-generations IS staged, so excluding it would make a rotated secret
// hash identically and deploy as a no-op.
func TestPayloadDigestSpansStagedSecretsButNotReleaseMetadata(t *testing.T) {
	spec := testConfig()
	write := func(dir, rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const generation = "sg-000000000000000000000000"
	secretPath := app.SecretGenerationPath(generation, ".ob-decrypted-sops-app.enc.env")

	staging := t.TempDir()
	write(staging, "compose.yaml", "services: {}\n")
	write(staging, "ob.snapshot.yml", "app: sample\n")
	write(staging, secretPath, "TOKEN=value\n")

	released := t.TempDir()
	for _, rel := range []string{"compose.yaml", "ob.snapshot.yml", secretPath} {
		body, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		write(released, rel, string(body))
	}
	// Written to the release directory after upload; staging never has these.
	write(released, release.ManifestFileName, `{"id":"R1","state":"serving"}`+"\n")
	write(released, ".job-migrate-result/result", "changed=true\n")

	stagingDigest, err := LocalPayloadDigest(spec, staging)
	if err != nil {
		t.Fatal(err)
	}
	releasedDigest, err := LocalPayloadDigest(spec, released)
	if err != nil {
		t.Fatal(err)
	}
	if stagingDigest != releasedDigest {
		t.Fatalf("release-store metadata moved the payload digest: staging %s, released %s", stagingDigest, releasedDigest)
	}

	// The regression that matters most: a rotated secret under the same
	// generation must move the digest, or the deploy short-circuits and the new
	// value never reaches the host.
	write(staging, secretPath, "TOKEN=rotated\n")
	rotated, err := LocalPayloadDigest(spec, staging)
	if err != nil {
		t.Fatal(err)
	}
	if rotated == stagingDigest {
		t.Fatal("a changed secret produced an identical payload digest; the deploy would be reported as a no-op")
	}
}

// The local matcher and the remote find arguments are two renderings of one
// table, so the only way to know they select the same set is to run both.
func TestLocalAndRemotePayloadSelectionAgree(t *testing.T) {
	if _, err := exec.LookPath("find"); err != nil {
		t.Skip("find is unavailable")
	}
	dir := t.TempDir()
	// Adversarial names: a directory that a `.job-*-result` glob would swallow
	// because find's -path lets * cross a slash, a near-miss suffix, and a
	// regular file carrying a directory's reserved name.
	for _, rel := range []string{
		"ob.snapshot.yml",
		"compose.yaml",
		"nested/compose.yaml",
		"manifest.json",
		"nested/manifest.json",
		".job-migrate-result/result",
		".job-migrate-result-extra/f",
		".job-alpha/beta-result/f",
		".job-alpha/gamma-result",
		app.SecretGenerationDirectory + "/sg-000000000000000000000000/app.env",
	} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	exclusions := payloadExclusionsFor(testConfig())
	shell := "cd " + q(dir) + " && find . -type f " + payloadFindArgs(exclusions) + " | LC_ALL=C sort"
	out, err := exec.Command("/bin/sh", "-c", shell).Output()
	if err != nil {
		t.Fatalf("run find: %v", err)
	}
	remote := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			remote[strings.TrimPrefix(line, "./")] = true
		}
	}

	local := map[string]bool{}
	if err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if slash := filepath.ToSlash(rel); isPayloadMember(exclusions, slash) {
			local[slash] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for rel := range remote {
		if !local[rel] {
			t.Errorf("%s is hashed remotely but not locally", rel)
		}
	}
	for rel := range local {
		if !remote[rel] {
			t.Errorf("%s is hashed locally but not remotely", rel)
		}
	}
}
