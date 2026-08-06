package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var errfCall = regexp.MustCompile(`errf\("([a-z_]+)"`)

// emittedCodes is every code the package can produce, read from the source
// rather than from a list someone maintains beside it.
func emittedCodes(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range errfCall.FindAllStringSubmatch(string(body), -1) {
			out[m[1]] = true
		}
	}
	return out
}

// A code is a promise an agent branches on. One that appears without being
// enumerated is a promise nobody made, and one that is enumerated without being
// emitted is a promise nobody keeps.
func TestEveryErrorCodeIsEnumerated(t *testing.T) {
	emitted := emittedCodes(t)
	if len(emitted) == 0 {
		t.Fatal("found no error codes in the package source; the scan is broken")
	}
	for code := range emitted {
		if _, ok := errorCodes[code]; !ok {
			t.Errorf("%q is emitted but not enumerated: add it to errorCodes with what it means", code)
		}
	}
	for code := range errorCodes {
		if !emitted[code] {
			t.Errorf("%q is enumerated but nothing emits it: remove it, or the enumeration promises a failure that cannot happen", code)
		}
	}
}

// And no failure path may produce a code outside the set, checked against every
// case in the corpus rather than against the source alone.
func TestNoCorpusFailureEscapesTheEnumeration(t *testing.T) {
	check := func(name string, err error) {
		if err == nil {
			return
		}
		e, ok := err.(*Error)
		if !ok {
			t.Errorf("%s: failure is not typed: %T", name, err)
			return
		}
		if _, known := errorCodes[e.Code]; !known {
			t.Errorf("%s: emitted %q, which is not in the enumeration", name, e.Code)
		}
	}
	for _, c := range conformanceCases() {
		_, err := LoadBytes([]byte(c.yaml), "ob.yml")
		check("conformance/"+c.name, err)
	}
	for _, path := range corpusProjects(t) {
		_, err := LoadBytes([]byte(readFixture(t, path)), path)
		check("corpus/"+filepath.Base(path), err)
	}
}
