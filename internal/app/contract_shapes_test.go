package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadText(t *testing.T, body string) *Spec {
	t.Helper()
	dir := t.TempDir()
	// The loader now refuses an entry naming a file that is not on disk, so a
	// fixture exercising env_files has to put one there.
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ob.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatalf("%s\n%v", body, err)
	}
	return p
}

func canonicalOf(t *testing.T, body string) string {
	t.Helper()
	r, err := loadText(t, body).Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

const shapeHead = "api_version: onebox.run/v1\napp: shop\n"

// 3.4 — a scalar shorthand and its object form are the same project.
//
// The contract promises a scalar form accepted once is accepted forever. That
// promise is only worth anything if the two forms mean the same thing, and the
// canonical output is where that becomes checkable: if they normalise
// identically, no downstream consumer can tell them apart.
func TestEveryShorthandEqualsItsObjectForm(t *testing.T) {
	for name, pair := range map[string][2]string{
		"image": {
			"environments: {production: {server: root@h}}\nimage: nginx\n",
			"environments: {production: {server: root@h}}\nimage: {reference: nginx}\n",
		},
		"health": {
			"environments: {production: {server: root@h}}\nimage: nginx\ndomain: x\nport: 8080\nhealth: /healthz\n",
			"environments: {production: {server: root@h}}\nimage: nginx\ndomain: x\nport: 8080\nhealth: {http: /healthz}\n",
		},
		"server": {
			"environments: {production: {server: root@203.0.113.10}}\nimage: nginx\n",
			"environments: {production: {server: {user: root, host: 203.0.113.10}}}\nimage: nginx\n",
		},
		// `needs` is not a top-level shorthand — the scalar form is the list
		// element, so this pair exercises it where it actually appears.
		"needs element": {
			"environments: {production: {server: root@h}}\nworkloads: {web: {role: application, image: nginx, needs: [postgres]}}\nservices: {postgres: 16}\n",
			"environments: {production: {server: root@h}}\nworkloads: {web: {role: application, image: nginx, needs: [{name: postgres}]}}\nservices: {postgres: 16}\n",
		},
		"service version": {
			"environments: {production: {server: root@h}}\nimage: nginx\nservices: {postgres: 16}\n",
			"environments: {production: {server: root@h}}\nimage: nginx\nservices: {postgres: {version: 16}}\n",
		},
		"hook": {
			"environments: {production: {server: root@h}}\nimage: nginx\nhooks: {post_deploy: \"echo hi\"}\n",
			"environments: {production: {server: root@h}}\nimage: nginx\nhooks: {post_deploy: {run: \"echo hi\"}}\n",
		},
		// An env_files entry is a path or an object naming the same path.
		"env file entry": {
			"environments: {production: {server: root@h}}\nimage: nginx\nruntime: {env_files: [.env]}\n",
			"environments: {production: {server: root@h}}\nimage: nginx\nruntime: {env_files: [{file: .env}]}\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			scalar := canonicalOf(t, shapeHead+pair[0])
			object := canonicalOf(t, shapeHead+pair[1])
			if scalar != object {
				t.Errorf("the two forms normalise differently:\n--- scalar\n%s\n--- object\n%s", scalar, object)
			}
		})
	}
}

// 5.6 — normalising the same text repeatedly yields the same bytes.
//
// Go randomises map iteration, so a canonical form emitted from a map without
// sorting passes most runs and fails some. An intermittent failure in the
// thing that is supposed to show an operator what will be deployed is worse
// than no canonical form at all.
func TestCanonicalOutputIsStableAcrossRuns(t *testing.T) {
	body := shapeHead + `environments:
  production: {server: root@h}
  staging: {server: root@h2}
workloads:
  zebra: {role: worker, image: nginx}
  alpha: {role: application, image: nginx, health: /healthz, domain: a.example.com, port: 1}
  middle: {role: worker, image: nginx}
services:
  redis: "7.4"
  postgres: 16
notifications:
  slack: {webhook: "https://hooks.example.com/x"}
  email: {webhook: "https://mail.example.com/y"}
`
	first := canonicalOf(t, body)
	for i := range 30 {
		if again := canonicalOf(t, body); again != first {
			t.Fatalf("canonical output changed on run %d", i)
		}
	}
}

// 12.7 — validation and rendering are idempotent and change nothing.
//
// A verb documented as having no side effects must leave the project file
// exactly as it found it. Anything that rewrites in place turns inspection
// into a mutation, and a reader into an author.
func TestInspectionChangesNothingOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ob.yml")
	body := shapeHead + "environments: {production: {server: root@h}}\nimage: nginx\ndomain: shop.example.com\nport: 3000\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var digests []string
	for range 3 {
		p, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.Resolve("production")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Canonical(); err != nil {
			t.Fatal(err)
		}
		rendered, err := r.Render("production", "R1", nil)
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, rendered.Digest)
	}
	for i, d := range digests {
		if d != digests[0] {
			t.Errorf("render %d produced a different digest: %s vs %s", i, d, digests[0])
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("inspecting the project rewrote it")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture writes ob.yml only; anything else present was left by
	// inspection, which is the thing this asserts.
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("inspection left files behind: %s", strings.Join(names, ", "))
	}
}
