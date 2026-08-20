package onebox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var lifecycleCodeLiteral = regexp.MustCompile(`"([a-z][a-z0-9_]{4,})"`)

// A code is a promise. The loader's table is guarded in both directions
// (app.TestEveryErrorCodeIsEnumerated); the lifecycle table was guarded in
// neither, so it accumulated codes nothing could raise while the reference page
// described it as "every typed failure code Onebox can emit". Nineteen of them
// were carried as "reserved" — enumerated, documented, and unreachable — for
// operations that were never built. They are gone; this is the guard that keeps
// the table honest.
func TestEveryEnumeratedLifecycleFailureIsRaisedBySomePath(t *testing.T) {
	emitted := emittedLifecycleCodes(t)
	if len(emitted) == 0 {
		t.Fatal("found no lifecycle codes in the package source; the scan is broken")
	}
	for code := range lifecycleFailureDefinitions {
		if !emitted[code] {
			t.Errorf("%q is enumerated but nothing raises it: delete it, or the table promises a failure that cannot happen", code)
		}
	}
}

func emittedLifecycleCodes(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, dir := range []string{".", "../engine", "../app"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "lifecycle_errors.go" {
				continue
			}
			body, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range lifecycleCodeLiteral.FindAllStringSubmatch(string(body), -1) {
				if _, ok := lifecycleFailureDefinitions[m[1]]; ok {
					out[m[1]] = true
				}
			}
		}
	}
	return out
}
