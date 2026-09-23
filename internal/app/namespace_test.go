package app

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cliNames are the only names that keep the command's own prefix: the project
// file, and the artifacts an operator saves, passes between commands, and reads
// on their own machine. Everything Onebox names on a host, inside a container,
// or for itself is in the onebox namespace — see reference/naming.
var cliNames = map[string]bool{
	"ob":                        true,
	"ob.yml":                    true,
	"ob.yaml":                   true,
	"ob.yml.tmpl":               true,
	"ob.exe":                    true,
	"ob-docgen":                 true,
	"ob-plan.json":              true,
	"ob-approval.json":          true,
	"ob-job-plan.json":          true,
	"ob-job-approval.json":      true,
	"ob-detached-job-plan.json": true,
	"ob-backup-report.json":     true,
}

var obName = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(\.?ob[-_.][A-Za-z0-9][A-Za-z0-9_.-]*)`)

// TestNoNewNamesOutsideTheOneboxNamespace fails when a string literal in the
// product introduces an ob-, ob_ or ob. name that is not one of cliNames.
// Comments are not checked; names are.
func TestNoNewNamesOutsideTheOneboxNamespace(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "_test.py") {
				return nil
			}
			var literals []string
			switch filepath.Ext(path) {
			case ".go":
				literals = goStringLiterals(t, path)
			case ".py", ".sh":
				literals = uncommentedLines(t, path)
			default:
				return nil
			}
			for _, literal := range literals {
				for _, match := range obName.FindAllStringSubmatch(literal, -1) {
					name := strings.TrimRight(match[1], ".")
					if !cliNames[name] {
						t.Errorf("%s: %q is outside the onebox namespace; only the CLI's own files keep the ob prefix", path, name)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func goStringLiterals(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s scanner.Scanner
	fset := token.NewFileSet()
	s.Init(fset.AddFile(path, fset.Base(), len(src)), src, nil, 0)
	var out []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.STRING || tok == token.CHAR {
			out = append(out, lit)
		}
	}
}

func uncommentedLines(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(src), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
		}
	}
	return out
}
