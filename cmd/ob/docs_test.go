package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The CLI reference is generated from this binary by cmd/ob-docgen, so its
// pages cannot describe a command that does not exist. This is the other
// direction: that no command exists without reaching the page.
//
// Documentation drifts by addition, not by editing: someone adds a verb, the
// page keeps describing the ones that were there before, and nothing says the
// page is now incomplete. An agent reading it then believes it has the whole
// surface. A failure here means `just docs-generate` has not been run.
const cliReferencePage = "site/src/content/docs/reference/cli.mdx"

const policiesPage = "site/src/content/docs/reference/policies.mdx"

func readDocsPage(t *testing.T, page string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(page)))
	if err != nil {
		t.Fatalf("%s must exist — run `just docs-generate`: %v", page, err)
	}
	return string(body)
}

func TestEveryCommandIsDocumented(t *testing.T) {
	reference := readDocsPage(t, cliReferencePage)

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

// The structured-output matrix is authored rather than generated, because
// which commands carry machine output — and under which schema identity — is a
// promise to an agent, not a fact derivable from the command tree. This keeps
// the promise honest.
func TestStructuredOutputMatrixIsDocumented(t *testing.T) {
	page := readDocsPage(t, policiesPage)
	for command := range structuredOutputCommands {
		if !strings.Contains(page, "`"+command+"`") {
			t.Errorf("structured command %q is absent from %s", command, policiesPage)
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
