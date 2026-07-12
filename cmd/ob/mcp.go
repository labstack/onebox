package main

import (
	"github.com/spf13/cobra"

	obmcp "github.com/labstack/onebox/internal/mcp"
	"github.com/labstack/onebox/internal/onebox"
)

func addMCPCommand(root *cobra.Command, g *globalFlags) {
	root.AddCommand(&cobra.Command{
		Use:   "mcp",
		Short: "serve read-only Onebox tools over MCP stdio",
		Long: "Start the local Onebox MCP server over stdin/stdout. An MCP-capable agent client launches this command automatically; stdout is reserved for protocol messages.\n\n" +
			"This first milestone exposes observation and deployment proposals only. It cannot mutate production.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service := onebox.New(onebox.Options{ConfigPath: g.ConfigPath, Environment: g.Env})
			return obmcp.Run(cmd.Context(), service, version)
		},
	})
}
