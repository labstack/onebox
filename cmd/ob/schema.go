package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
)

// `ob schema` publishes the contract in the one format editors already read.
//
// It is generated from the same declarations the loader enforces and gated
// against the conformance corpus, so what an editor tells you while you type is
// what `ob validate` will tell you afterwards. A published schema that
// disagreed with the loader would be worse than none: it would teach the author
// something untrue and they would learn to ignore it.
func addSchemaCommand(root *cobra.Command, g *globalFlags) {
	var to string

	cmd := &cobra.Command{
		Use:   "schema",
		Short: "print the JSON Schema for the project file, for editors",
		Long: "Write the JSON Schema for the `onebox.run/v1` project file.\n\n" +
			"Reference it from the first line of a project so an editor can offer\n" +
			"completion, hover documentation and inline errors:\n\n" +
			"  # yaml-language-server: $schema=" + app.SchemaID + "\n\n" +
			"Or keep a copy in the repository with --to, which is what an editor\n" +
			"needs when the machine is offline.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := app.JSONSchema()
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			body = append(body, '\n')
			if isStructuredOutput(g) && to == "" {
				return writeFiniteSuccess(cmd, g, map[string]any{"schema": json.RawMessage(body)})
			}
			if to == "" {
				_, err := cmd.OutOrStdout().Write(body)
				return err
			}
			if err := os.WriteFile(to, body, 0o644); err != nil {
				return writeStructuredReadFailure(cmd, g, fmt.Errorf("cannot write %s: %w", to, err))
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"schema": json.RawMessage(body), "artifact_path": to})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", to)
			return nil
		},
	}
	cmd.Flags().StringVarP(&to, "out", "o", "", "write to this path instead of standard output")
	root.AddCommand(cmd)
}
