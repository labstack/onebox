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
// neither, so it accumulated codes nothing can raise while the reference page
// described it as "every typed failure code Onebox can emit".
//
// The capabilities behind reservedLifecycleFailures exist in the operations
// model but are wired to no command yet. They stay enumerated so the contract
// is stable when they land — but they are named here, and the page marks them,
// so nobody reads them as reachable.
func TestReservedLifecycleFailuresAreExactlyTheUnreachableOnes(t *testing.T) {
	emitted := emittedLifecycleCodes(t)
	if len(emitted) == 0 {
		t.Fatal("found no lifecycle codes in the package source; the scan is broken")
	}
	for code := range lifecycleFailureDefinitions {
		_, reserved := reservedLifecycleFailures[code]
		switch {
		case emitted[code] && reserved:
			t.Errorf("%q is emitted but listed as reserved: remove it from reservedLifecycleFailures", code)
		case !emitted[code] && !reserved:
			t.Errorf("%q is enumerated but nothing raises it: reserve it, or the table promises a failure that cannot happen", code)
		}
	}
	for code := range reservedLifecycleFailures {
		if _, ok := lifecycleFailureDefinitions[code]; !ok {
			t.Errorf("%q is reserved but not enumerated", code)
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
