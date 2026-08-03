package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
)

// `ob config` answers "what did you make of this". A project file shows what
// was typed; it cannot show that a value is what it is because nobody said
// otherwise, or because an environment override says so.
func addConfigCommand(root *cobra.Command, g *globalFlags) {
	var origins bool
	cmd := &cobra.Command{
		// Named apart from `config`, which still serves the contract the engine
		// executes today. At the cutover that one goes and this takes the name.
		Use:   "canonical",
		Short: "print the canonical form Onebox understood, with where each value came from",
		Long: "Print the project as Onebox normalised it for an environment: shorthand\n" +
			"expanded, defaults filled, overrides applied.\n\n" +
			"Values you did not write are marked with their origin, because the difference\n" +
			"between a value someone chose and one that appeared by default is what a\n" +
			"person checking a production configuration needs to see.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := app.Load(g.ConfigPath)
			if err != nil {
				return explain(err)
			}
			resolved, err := p.Resolve(g.Env)
			if err != nil {
				return explain(err)
			}
			out := cmd.OutOrStdout()

			if origins {
				if g.Output == "json" {
					rows := map[string]string{}
					for _, kv := range resolved.OriginTable() {
						rows[kv[0]] = kv[1]
					}
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					return enc.Encode(rows)
				}
				w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				for _, kv := range resolved.OriginTable() {
					fmt.Fprintf(w, "%s\t%s\n", kv[0], kv[1])
				}
				return w.Flush()
			}

			body, err := resolved.Canonical()
			if err != nil {
				return explain(err)
			}
			_, err = out.Write(body)
			return err
		},
	}
	cmd.Flags().BoolVar(&origins, "origins", false, "list every value's origin instead of the document")
	root.AddCommand(cmd)
}
