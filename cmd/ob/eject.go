package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
)

// `ob eject` is the exit. Generated configuration you cannot leave is the thing
// people refuse to adopt, so this hands the runtime over permanently.
func addEjectCommand(root *cobra.Command, g *globalFlags) {
	var (
		dest       string
		overwrite  bool
		imageFlags []string
	)
	cmd := &cobra.Command{
		Use:   "eject",
		Short: "write the generated runtime into the repository and hand it over for good",
		Long: "Write the runtime Onebox generates into your repository as ordinary Compose,\n" +
			"and repoint the affected workloads at it.\n\n" +
			"This is one way. Onebox will not regenerate or reconcile those services\n" +
			"afterwards. The written file carries none of the identity or routing keys\n" +
			"Onebox adds, so it is a file you own rather than one it half-owns, and your\n" +
			"project file keeps its comments and ordering.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := app.Load(g.ConfigPath)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, cliEjectSchemaVersion, err)
			}
			resolved, err := p.Resolve(g.Env)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, cliEjectSchemaVersion, err)
			}
			images, err := parseImages(imageFlags)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, cliEjectSchemaVersion, err)
			}
			res, err := resolved.Eject(dest, "ejected", images, overwrite)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, cliEjectSchemaVersion, err)
			}
			out := cmd.OutOrStdout()
			if isStructuredOutput(g) {
				return writeCLIJSON(out, cliEjectEnvelope{
					SchemaVersion: cliEjectSchemaVersion,
					Runtime:       res.Runtime,
					Workloads:     res.Workloads,
				}, g.Output == "json")
			}
			fmt.Fprintf(out, "wrote %s\n", res.Runtime)
			fmt.Fprintf(out, "handed over: %s\n", strings.Join(res.Workloads, ", "))
			fmt.Fprintf(out, "\nthese are yours now — Onebox will not regenerate them\n")
			return nil
		},
	}
	// Empty by default so the package can pick a name that does not collide
	// with a Compose file the project already references. A flag default here
	// would defeat that and refuse a perfectly ordinary ejection.
	cmd.Flags().StringVarP(&dest, "out", "o", "", "repository path to write the runtime to (default: a free name beside the project)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing file at the destination")
	cmd.Flags().StringArrayVar(&imageFlags, "image", nil, "resolved image as workload=reference (repeatable)")
	root.AddCommand(cmd)
}
