package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// `ob preflight` is the first command on the new contract that talks to a
// server. It asks whether the project could be deployed and changes nothing:
// every command it runs is a read, which the package's own tests assert.
func addPreflightCommand(root *cobra.Command, g *globalFlags) {
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "ask the server whether this project could be deployed (changes nothing)",
		Long: "Render the project locally, then ask the server what would stand in the way:\n" +
			"a missing container runtime, a base path this account cannot write, a derived\n" +
			"name already held by something Onebox does not own, a missing ingress network.\n\n" +
			"Every problem is reported at once rather than the first one, and nothing is\n" +
			"created, renamed or removed.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := app.Load(g.ConfigPath)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			resolved, err := p.Resolve(g.Env)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}

			env, ok := p.Environments[g.Env]
			if !ok {
				return writeStructuredReadFailure(cmd, g, fmt.Errorf("environment %q is not declared", g.Env))
			}
			// One authority for the address. Building it here from Host and
			// User dropped a declared port and connected to 22 instead — a
			// silent success against the wrong server, which is worse than
			// any failure this command reports.
			addr := env.Route().String()

			t, err := transport.NewSSHRoute(cmd.Context(), env.Route())
			if err != nil {
				return writeStructuredReadFailure(cmd, g, fmt.Errorf("cannot reach %s: %w", addr, err))
			}
			defer t.Close()

			report, err := resolved.Preflight(cmd.Context(), t)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			report.Server = addr

			if !isStructuredOutput(g) {
				writeReport(cmd, report)
			}
			if !report.OK() {
				// A non-zero exit is what makes this usable in a script or a
				// pipeline without parsing anything.
				preflightErr := fmt.Errorf("%d of %d checks failed", len(report.Failures()), len(report.Checks))
				if isStructuredOutput(g) {
					publicErr := &cliPublicError{Code: "preflight_failed", SafeMessage: safeMessageForCode("preflight_failed", "one or more preflight checks failed"), Details: report}
					if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); err != nil {
						return err
					}
					return withExitCode(preflightErr, 1)
				}
				return preflightErr
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"report": report})
			}
			return nil
		},
	}
	root.AddCommand(cmd)
}

func writeReport(cmd *cobra.Command, r *app.Report) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s  environment %s\n\n", r.Server, r.Env)
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(out, "  %s  %-18s %s\n", mark, c.Name, c.Detail)
		if !c.OK && c.Remedy != "" {
			for _, line := range wrap(c.Remedy, 66) {
				fmt.Fprintf(out, "        %s\n", line)
			}
		}
	}
}

// wrap keeps a remedy readable in a terminal without depending on the UI
// package, which still belongs to the other contract.
func wrap(s string, width int) []string {
	var lines []string
	var cur string
	for _, word := range strings.Fields(s) {
		if cur == "" {
			cur = word
			continue
		}
		if len(cur)+1+len(word) > width {
			lines = append(lines, cur)
			cur = word
			continue
		}
		cur += " " + word
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
