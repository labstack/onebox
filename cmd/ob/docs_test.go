package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Every command appears in the CLI reference, and every command explains
// itself.
//
// Documentation drifts by addition, not by editing: someone adds a verb, the
// page keeps describing the ones that were there before, and nothing says the
// page is now incomplete. An agent reading it then believes it has the whole
// surface. This is the check that says so.
func TestEveryCommandIsDocumented(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatalf("the CLI reference must exist: %v", err)
	}
	reference := string(page)

	var walk func(*cobra.Command, string)
	walk = func(c *cobra.Command, prefix string) {
		for _, sub := range c.Commands() {
			name := strings.Fields(sub.Use)[0]
			full := strings.TrimSpace(prefix + " " + name)
			// Cobra generates these two; they are not ours to document.
			if name == "completion" || name == "help" {
				continue
			}
			if !strings.Contains(reference, "`ob "+full+"`") {
				t.Errorf("`ob %s` is not mentioned in docs/cli.md", full)
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

func TestStructuredOutputMatrixIsDocumented(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	for command := range structuredOutputCommands {
		if !strings.Contains(string(page), "`"+command+"`") {
			t.Errorf("structured command %q is absent from docs/cli.md", command)
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
		if !strings.Contains(string(page), version) {
			t.Errorf("structured schema %q is absent from docs/cli.md", version)
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
