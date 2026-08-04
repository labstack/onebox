package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/compose"
)

// The contract must not move when its validator is replaced. This freezes what
// the loader does today — for every conformance case and every real project —
// so that a replacement can be held to it exactly.
//
// A divergence here is a defect in the replacement, never a decision. It is
// checked in so the comparison survives the change that removes the validator
// it was recorded against.

type verdict struct {
	Case   string `json:"case"`
	Loads  bool   `json:"loads"`
	Code   string `json:"code,omitempty"`
	Digest string `json:"digest,omitempty"`
}

const goldenPath = "testdata/contract-verdicts.json"

func recordVerdicts(t *testing.T) []verdict {
	t.Helper()
	var out []verdict

	for _, c := range conformanceCases() {
		v := verdict{Case: "conformance/" + c.name}
		spec, err := LoadBytes([]byte(c.yaml), "ob.yml")
		if err == nil {
			v.Loads = true
			v.Digest = renderDigest(t, spec)
		} else if e, ok := err.(*Error); ok {
			v.Code = e.Code
		} else {
			v.Code = "untyped"
		}
		out = append(out, v)
	}

	for _, path := range corpusProjects(t) {
		v := verdict{Case: "corpus/" + filepath.Base(path)}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := LoadBytes([]byte(strings.ReplaceAll(string(body), "root@TARGET", "root@1.2.3.4")), path)
		if err == nil {
			v.Loads = true
			v.Digest = renderDigest(t, spec)
		} else if e, ok := err.(*Error); ok {
			v.Code = e.Code
		} else {
			v.Code = "untyped"
		}
		out = append(out, v)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Case < out[j].Case })
	return out
}

// renderDigest is the generated runtime's identity, or the typed reason it
// could not be generated. Both are part of what must not move: a project that
// loads and then fails to render is as much a contract fact as one that loads.
func renderDigest(t *testing.T, spec *Spec) string {
	t.Helper()
	env := ""
	for _, name := range sortedKeys(spec.Environments) {
		env = name
		break
	}
	r, err := spec.Resolve(env)
	if err != nil {
		return "resolve:" + codeOf(err)
	}
	images := Images{}
	for _, name := range sortedKeys(spec.Workloads) {
		if spec.Workloads[name].Build != nil {
			images[name] = "example.invalid/pinned:1"
		}
	}
	rendered, err := r.Render(env, "frozen", images)
	if err != nil {
		return "render:" + codeOf(err)
	}
	// The runtime must be something the container engine can actually read.
	// Freezing only its digest let a generated mount with no target survive as
	// a stable hash of a file Compose refuses to parse.
	if _, err := compose.LoadBytes(context.Background(), rendered.Bytes, "frozen", t.TempDir()); err != nil {
		return "unparseable:" + firstLine(err.Error())
	}
	for _, name := range sortedKeys(rendered.Services) {
		if _, err := compose.LoadBytes(context.Background(), rendered.Services[name], "frozen-"+name, t.TempDir()); err != nil {
			return "unparseable-service:" + name + ":" + firstLine(err.Error())
		}
	}

	sum := sha256.Sum256(rendered.Bytes)
	parts := []string{hex.EncodeToString(sum[:])}
	for _, name := range sortedKeys(rendered.Services) {
		s := sha256.Sum256(rendered.Services[name])
		parts = append(parts, name+"="+hex.EncodeToString(s[:])[:16])
	}
	return strings.Join(parts, " ")
}

func codeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return "untyped"
}

// readFixture is a corpus project with its placeholder target resolved, as the
// harness and the schema gate both need it.
func readFixture(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(body), "root@TARGET", "root@1.2.3.4")
}

func corpusProjects(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{
		filepath.Join("..", "..", "e2e", "apps"),
		filepath.Join("..", "..", "openspec", "changes", "adopt-declarative-project-schema", "conversions"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("corpus %s: %v", dir, err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".yml") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestContractDidNotMove is the acceptance test for replacing the validator.
// Run with -update to record a new baseline, which is only correct when the
// contract itself was deliberately changed.
func TestContractDidNotMove(t *testing.T) {
	got := recordVerdicts(t)

	if os.Getenv("OB_UPDATE_VERDICTS") == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %d verdicts", len(got))
		return
	}

	body, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("no frozen verdict; record one with OB_UPDATE_VERDICTS=1: %v", err)
	}
	var want []verdict
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	byCase := map[string]verdict{}
	for _, v := range want {
		byCase[v.Case] = v
	}
	for _, g := range got {
		w, ok := byCase[g.Case]
		if !ok {
			t.Errorf("%s: new case with no frozen verdict", g.Case)
			continue
		}
		delete(byCase, g.Case)
		if g.Loads != w.Loads {
			t.Errorf("%s: loads = %v, frozen %v", g.Case, g.Loads, w.Loads)
		}
		if g.Code != w.Code {
			t.Errorf("%s: error code = %q, frozen %q", g.Case, g.Code, w.Code)
		}
		if g.Digest != w.Digest {
			t.Errorf("%s: runtime moved\n  now:    %s\n  frozen: %s", g.Case, g.Digest, w.Digest)
		}
	}
	for name := range byCase {
		t.Errorf("%s: frozen case is gone", name)
	}
}
