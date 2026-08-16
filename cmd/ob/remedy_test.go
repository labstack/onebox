package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/onebox"
)

// Every failure names a command the operator can actually run.
//
// lifecycle validation checks guidance is shell-safe and starts with `ob `,
// which is a claim about its form, not its truth. Eighteen of thirty-five
// lifecycle codes passed that guard while naming verbs the CLI has never had —
// `ob backup inspect`, `ob protection enable`, `ob assurance status`. A code is
// read at the moment something is broken, so a remedy that exits with
// `unknown command` costs the operator a round trip and their trust in the
// rest of the page.
func TestEveryLifecycleRemedyNamesARealCommand(t *testing.T) {
	root := newRootCmd()
	for _, code := range onebox.LifecycleFailureCodes() {
		failure, err := onebox.NewLifecycleFailure(code)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		command := failure.GuidanceCommand()
		if command == "" {
			continue
		}
		fields := strings.Fields(command)
		if fields[0] != "ob" {
			t.Errorf("%s: guidance %q does not start with ob", code, command)
			continue
		}
		var path []string
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "-") {
				break
			}
			path = append(path, field)
		}
		want := "ob " + strings.Join(path, " ")
		cmd, _, err := root.Find(path)
		if err != nil || cmd == nil || cmd.CommandPath() != want {
			t.Errorf("%s: guidance %q names %q, which is not a command", code, command, want)
		}
	}
}

// A remedy that names a flag must name one the command has.
//
// The earlier check resolved only the command path, so renaming `ob abort
// --force` to `--break-migration-gate` left four error strings telling an
// operator to run a flag that no longer existed — two of them in HALT-AND-PAGE
// guidance, read at the moment a migration has already run and the release is
// stuck. Found by running the product, not by any test.
func TestEveryFlagNamedInAnErrorStringExistsOnThatCommand(t *testing.T) {
	root := newRootCmd()
	pattern := regexp.MustCompile("`ob ([a-z][a-z -]*?) (--[a-z-]+)`")
	for _, dir := range []string{"../../internal/engine", "../../internal/app", "../../internal/onebox", "."} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range pattern.FindAllStringSubmatch(string(body), -1) {
				path, flag := strings.Fields(m[1]), strings.TrimPrefix(m[2], "--")
				cmd, _, findErr := root.Find(path)
				if findErr != nil || cmd == nil {
					t.Errorf("%s/%s: `ob %s` is not a command", dir, name, m[1])
					continue
				}
				if cmd.Flags().Lookup(flag) == nil && cmd.InheritedFlags().Lookup(flag) == nil {
					t.Errorf("%s/%s: `ob %s` has no --%s flag, but an error string tells the operator to use it", dir, name, m[1], flag)
				}
			}
		}
	}
}

// Loader guidance is authored per error code, so unlike the lifecycle table it
// carries no structural guarantee that it is a command at all. It is published
// in next_command/diagnostic_command/resolving_command, which the contract
// documents as safe Onebox commands an agent may run — so every value has to be
// one, or be dropped before it reaches the envelope.
func TestEveryLoaderGuidanceValueIsASafeCommand(t *testing.T) {
	// Next is authored at each errf call site, not per code, so the contract has
	// to be checked against the source.
	call := regexp.MustCompile(`errf\(\s*"([a-z_]+)"\s*,\s*(?:"(?:[^"\\]|\\.)*"|[^,\n]+)\s*,\s*"((?:[^"\\]|\\.)*)"`)
	root := filepath.Join("..", "..", "internal", "app")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range call.FindAllStringSubmatch(string(body), -1) {
			next := match[2]
			if next == "" {
				continue
			}
			checked++
			if !onebox.SafeGuidanceCommand(next) {
				t.Errorf("%s: code %q publishes %q as guidance, which is not a safe command", entry.Name(), match[1], next)
			}
		}
	}
	if checked == 0 {
		t.Fatal("scanned no guidance values; the pattern no longer matches errf call sites")
	}
}

func TestUnsafeGuidanceIsDroppedRatherThanPublished(t *testing.T) {
	result := &cliPublicError{Code: "secrets_withdrawn"}
	setCommandGuidance(result, "runtime.env_files: [{file: <path>, provider: sops}]")
	if result.NextCommand != "" || result.DiagnosticCommand != "" || result.ResolvingCommand != "" {
		t.Fatalf("prose reached a guidance field: %+v", result)
	}
	setCommandGuidance(result, "ob validate")
	if result.DiagnosticCommand != "ob validate" {
		t.Fatalf("a real command was not published: %+v", result)
	}
}
