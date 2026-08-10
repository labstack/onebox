package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
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

	d1, err := LocalPayloadDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	// compose.yaml changes must NOT move the digest
	write("compose.yaml", "services: {changed: {}}\n")
	d2, err := LocalPayloadDigest(dir)
	if err != nil || d1 != d2 {
		t.Fatalf("compose.yaml must be excluded: %s vs %s (%v)", d1, d2, err)
	}
	// payload changes MUST move it
	write("server/.env", "KEY=two\n")
	d3, err := LocalPayloadDigest(dir)
	if err != nil || d3 == d1 {
		t.Fatalf("payload change must move the digest: %v", err)
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
	for _, want := range []string{"! -name compose.yaml", "! -name '.job-*-result'", "LC_ALL=C sort"} {
		if !strings.Contains(seq, want) {
			t.Fatalf("remote pipeline missing %q:\n%s", want, seq)
		}
	}
}
