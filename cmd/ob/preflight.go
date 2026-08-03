package main

import (
	"encoding/json"
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
		Short: "ask the target whether this project could be deployed (changes nothing)",
		Long: "Render the project locally, then ask the target what would stand in the way:\n" +
			"a missing container runtime, a base path this account cannot write, a derived\n" +
			"name already held by something Onebox does not own, a missing ingress network.\n\n" +
			"Every problem is reported at once rather than the first one, and nothing is\n" +
			"created, renamed or removed.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := app.Load(g.ConfigPath)
			if err != nil {
				return explain(err)
			}
			resolved, err := p.Resolve(g.Env)
			if err != nil {
				return explain(err)
			}

			env, ok := p.Environments[g.Env]
			if !ok {
				return fmt.Errorf("environment %q is not declared", g.Env)
			}
			addr := env.Server.Host
			if env.Server.User != "" {
				addr = env.Server.User + "@" + env.Server.Host
			}

			t, err := transport.NewSSHContext(cmd.Context(), addr)
			if err != nil {
				return fmt.Errorf("cannot reach %s: %w", addr, err)
			}
			defer t.Close()

			report, err := resolved.Preflight(cmd.Context(), t)
			if err != nil {
				return explain(err)
			}
			report.Target = addr

			out := cmd.OutOrStdout()
			if g.Output == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			} else {
				writeReport(cmd, report)
			}
			if !report.OK() {
				// A non-zero exit is what makes this usable in a script or a
				// pipeline without parsing anything.
				return fmt.Errorf("%d of %d checks failed", len(report.Failures()), len(report.Checks))
			}
			return nil
		},
	}
	root.AddCommand(cmd)
}

func writeReport(cmd *cobra.Command, r *app.Report) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s  environment %s\n\n", r.Target, r.Env)
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
