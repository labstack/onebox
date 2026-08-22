package app

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Generation is a pure function of the project text, the environment name, the
// release identity and the image map. Nothing else.
//
// These tests exist because every impurity in a generator is a difference
// between what was reviewed and what is deployed: a clock makes two renders of
// one commit disagree, entropy makes a digest meaningless, and an environment
// variable makes the result depend on whose shell ran it.

const purityProject = `api_version: onebox.run/v1
app: shop
environments:
  production:
    server: root@203.0.113.10
  staging:
    server: root@203.0.113.20
    overrides:
      workloads:
        web: {replicas: 1}
runtime:
  env_files: [.env.production]
workloads:
  web:
    role: application
    image: nginx:1.27
    health: /healthz
    replicas: 2
    routes:
      - {domain: shop.example.com, path: /, port: 3000}
      - {domain: shop.example.com, path: /api, port: 3001}
      - {domain: grpc.example.com, port: 9000, entrypoint: grpc, scheme: h2c}
      - {domain: db.example.com, port: 5432, protocol: tcp, tls: passthrough, entrypoint: pg}
  worker:
    role: worker
    image: nginx:1.27
    needs: [postgres]
  migrate:
    role: job
    image: nginx:1.27
    data_effect: migration
    when: pre_release
services:
  postgres: 16
`

func purityFixture(t *testing.T) *Spec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/.env.production", []byte("TOKEN=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := dir + "/ob.yml"
	if err := os.WriteFile(path, []byte(purityProject), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 7.8 — the same inputs produce the same bytes, however often and in whatever
// environment. Map ordering is included: Go randomises map iteration, so a
// generator that emits from a map without sorting fails this intermittently,
// which is worse than failing always.
func TestGenerationIsDeterministic(t *testing.T) {
	p := purityFixture(t)
	first, err := p.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		again, err := purityFixture(t).Render("production", "R1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(again.Bytes) != string(first.Bytes) {
			t.Fatalf("render %d differs from the first", i)
		}
		if again.Digest != first.Digest {
			t.Fatalf("render %d has a different digest: %s vs %s", i, again.Digest, first.Digest)
		}
		for name, doc := range first.Services {
			if string(again.Services[name]) != string(doc) {
				t.Fatalf("render %d: service %s differs", i, name)
			}
		}
	}
}

// 7.8 — no environment variable reaches the result. A generator that consults
// the process environment produces a runtime that depends on whose shell ran
// it, and the digest stops meaning anything.
func TestGenerationIgnoresTheProcessEnvironment(t *testing.T) {
	before, err := purityFixture(t).Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"HOME", "USER", "PATH", "PWD", "SHELL", "TZ", "LANG",
		"OB_BASE_DIR", "OB_APP", "OB_ENV", "OB_IMAGE", "OB_RELEASE",
		"COMPOSE_PROJECT_NAME", "DOCKER_HOST", "TOKEN", "API_TOKEN",
	} {
		t.Setenv(key, "generation-must-not-see-this")
	}
	after, err := purityFixture(t).Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Bytes) != string(before.Bytes) {
		t.Error("the process environment changed the generated runtime")
	}
	if strings.Contains(string(after.Bytes), "generation-must-not-see-this") {
		t.Error("a process environment value was written into the runtime")
	}
}

// 7.8 — the release identity is an input, so two releases differ only where
// the release is named. Anything else moving means something derived it from
// the clock.
func TestOnlyTheReleaseIdentityDistinguishesTwoRenders(t *testing.T) {
	one, err := purityFixture(t).Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := purityFixture(t).Render("production", "R2", nil)
	if err != nil {
		t.Fatal(err)
	}
	normalised := strings.ReplaceAll(string(two.Bytes), "R2", "R1")
	if normalised != string(one.Bytes) {
		t.Error("two renders differ by more than the release they name")
	}
}

// 7.7 — generation contacts nothing.
//
// `ob preview`, `ob canonical`, `ob validate` and `ob eject` are documented as
// touching no target, and a generator that resolved a registry or read a host
// would make each of them a network operation: failing on a plane, succeeding
// differently in CI, and producing a runtime nobody can reproduce.
//
// The package deliberately contains one file that does reach a target —
// preflight is the phase that needs it, and says so. So the assertion is per
// file rather than per package: everything that generates must be unable to
// reach a target, and adding that ability has to be a visible edit here.
func TestGenerationCannotReachATarget(t *testing.T) {
	const targetFacing = "preflight.go" // the one phase that may

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == targetFacing {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checked++
		for _, imported := range file.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			switch {
			case strings.HasSuffix(path, "/internal/transport"),
				strings.HasSuffix(path, "/internal/engine"),
				strings.HasSuffix(path, "/internal/release"),
				strings.HasSuffix(path, "/internal/journal"),
				path == "net", path == "net/http":
				t.Errorf("%s generates, and imports %s, which reaches a target", name, path)
			}
		}
	}
	if checked < 10 {
		t.Fatalf("only %d files examined; the walk is not finding the package", checked)
	}
}

// And the same for the failure paths: a refusal must be reachable without a
// target too, or an operator cannot find out what is wrong until they can
// connect to production.
func TestEveryGenerationFailureIsReachableOffline(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":      "api_version: onebox.run/v1\napp: shop\nenvironments: {p: {server: h}}\nimage: nginx\nreplicaz: 3\n",
		"no source":          "api_version: onebox.run/v1\napp: shop\nenvironments: {p: {server: h}}\nworkloads: {web: {role: application}}\n",
		"two sources":        "api_version: onebox.run/v1\napp: shop\nenvironments: {p: {server: h}}\nworkloads: {web: {role: application, image: nginx, build: .}}\n",
		"job without effect": "api_version: onebox.run/v1\napp: shop\nenvironments: {p: {server: h}}\nworkloads: {j: {role: job, image: nginx}}\n",
		"unknown driver":     "api_version: onebox.run/v1\napp: shop\nenvironments: {p: {server: h}}\nimage: nginx\nservices: {weird: {driver: nosuchthing, version: \"1\"}}\n",
		"route collision":    "api_version: onebox.run/v1\napp: shop\nenvironments: {p: {server: h}}\nworkloads:\n  a: {role: application, image: nginx, domain: x.example.com, port: 1}\n  b: {role: application, image: nginx, domain: x.example.com, port: 2}\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := dir + "/ob.yml"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			p, err := Load(path)
			if err != nil {
				return // refused at load, which is offline by construction
			}
			if _, err = p.Render("p", "R1", nil); err == nil {
				t.Fatal("expected a refusal, and it must not need a target to produce one")
			}
		})
	}
}
