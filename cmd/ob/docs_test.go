package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const cliReferencePage = "site/src/content/docs/reference/cli.mdx"

const policiesPage = "site/src/content/docs/reference/policies.mdx"

// readDocsPage reads a documentation page from the site. The remediation hint
// belongs at the call site: only some of these pages are generated, and telling
// someone to regenerate an authored page sends them somewhere with no answer.
func readDocsPage(t *testing.T, page, hint string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(page)))
	if err != nil {
		t.Fatalf("%s must exist — %s: %v", page, hint, err)
	}
	return string(body)
}

func sectionOf(t *testing.T, page, start, end string) string {
	t.Helper()
	from := strings.Index(page, start)
	if from < 0 {
		t.Fatalf("%q not found in the page; the section was renamed and this test now proves nothing", start)
	}
	rest := page[from:]
	if to := strings.Index(rest, end); to >= 0 {
		return rest[:to]
	}
	t.Fatalf("%q not found after %q", end, start)
	return ""
}

// Every command reaches the CLI reference.
//
// The page is generated from this binary, so it cannot describe a command that
// does not exist; this is the other direction. Documentation drifts by addition:
// someone adds a verb, the page keeps describing the ones that came before, and
// nothing says it is now incomplete.
//
// A failure here usually means `just docs-generate` has not been run. It can
// also mean the renderer cannot reach the command — it recurses, so that would
// be a generator defect rather than a stale file.
func TestEveryCommandIsDocumented(t *testing.T) {
	reference := readDocsPage(t, cliReferencePage, "run `just docs-generate`")

	var walk func(*cobra.Command, string)
	walk = func(c *cobra.Command, prefix string) {
		for _, sub := range c.Commands() {
			name := strings.Fields(sub.Use)[0]
			full := strings.TrimSpace(prefix + " " + name)
			// Cobra generates these two; they are not ours to document.
			if name == "completion" || name == "help" {
				continue
			}
			// The generated page gives each command its own heading.
			if !strings.Contains(reference, "\n## ob "+full+"\n") &&
				!strings.Contains(reference, "\n### ob "+full+"\n") {
				t.Errorf("`ob %s` has no section in %s — run `just docs-generate`", full, cliReferencePage)
			}
			if sub.Long == "" && sub.HasSubCommands() {
				t.Errorf("`ob %s` groups other commands but does not explain what they share", full)
			}
			if sub.Long == "" && !sub.HasSubCommands() {
				t.Errorf("`ob %s` has no long help: an operator running it blind gets one line", full)
			}
			// A long help that merely restates the summary tells nobody
			// anything they did not already have.
			if sub.Long != "" && strings.TrimSpace(sub.Long) == strings.TrimSpace(sub.Short) {
				t.Errorf("`ob %s` long help only repeats its summary", full)
			}
			walk(sub, full)
		}
	}
	walk(newRootCmd(), "")
}

func TestEveryFlagAndAliasIsUsable(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		seenAliases := map[string]bool{}
		for _, alias := range cmd.Aliases {
			if alias == cmd.Name() {
				t.Errorf("`%s` repeats its own name as an alias", cmd.CommandPath())
			}
			if seenAliases[alias] {
				t.Errorf("`%s` repeats alias %q", cmd.CommandPath(), alias)
			}
			seenAliases[alias] = true
		}
		cmd.NonInheritedFlags().VisitAll(func(flag *pflag.Flag) {
			if strings.TrimSpace(flag.Usage) == "" {
				t.Errorf("`%s --%s` has no help text", cmd.CommandPath(), flag.Name)
			}
		})
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(newRootCmd())
}

// The structured-output matrix is authored rather than generated, because which
// commands carry machine output — and under which schema identity — is a promise
// to an agent, not a fact derivable from the command tree. This keeps the
// promise honest.
//
// It reads the matrix section rather than the whole page. Searching the page for
// a command name let ordinary prose stand in for the row: `ob doctor` and
// `ob validate` are named elsewhere on the page, so either could have been
// deleted from the matrix with this test still green.
func TestStructuredOutputMatrixIsDocumented(t *testing.T) {
	page := readDocsPage(t, policiesPage, "it is authored, not generated")
	// Only the list itself, not the paragraph after it: that sentence names
	// `ob version` for its `--json` report, which is a different contract.
	matrix := sectionOf(t, page, "### Commands that accept", "Anything not listed")

	documented := map[string]bool{}
	for _, command := range regexp.MustCompile("`(ob [a-z-]+(?: [a-z-]+)*)`").FindAllStringSubmatch(matrix, -1) {
		documented[command[1]] = true
	}

	for command := range structuredOutputCommands {
		if !documented[command] {
			t.Errorf("%q carries structured output but is absent from the matrix in %s", command, policiesPage)
		}
	}
	for command := range documented {
		if !structuredOutputCommands[command] {
			t.Errorf("the matrix in %s lists %q, which does not carry structured output", policiesPage, command)
		}
	}
	for _, version := range []string{
		cliValidateSchemaVersion,
		cliCanonicalSchemaVersion,
		cliPreviewSchemaVersion,
		cliEjectSchemaVersion,
		cliStatusSchemaVersion,
		cliOperationSchemaVersion,
		cliRecordSchemaVersion,
		doctorReportSchemaVersion,
		versionReportSchemaVersion,
	} {
		if !strings.Contains(page, version) {
			t.Errorf("structured schema %q is absent from %s", version, policiesPage)
		}
	}
}

// The summary line is what `ob --help` shows, and it is the only description
// most readers see. It must not be empty, and it must not have been pasted
// twice — which is exactly the kind of thing nobody notices in a list of 25.
func TestEverySummaryIsUsable(t *testing.T) {
	var walk func(*cobra.Command, string)
	walk = func(c *cobra.Command, prefix string) {
		for _, sub := range c.Commands() {
			name := strings.Fields(sub.Use)[0]
			full := strings.TrimSpace(prefix + " " + name)
			if name == "completion" || name == "help" {
				continue
			}
			short := strings.TrimSpace(sub.Short)
			if short == "" {
				t.Errorf("`ob %s` has no summary", full)
			}
			if fields := strings.Fields(short); len(fields) > 1 && fields[0] == fields[1] {
				t.Errorf("`ob %s` summary repeats its first word: %q", full, short)
			}
			walk(sub, full)
		}
	}
	walk(newRootCmd(), "")
}
