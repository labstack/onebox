package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/labstack/yeet/internal/ui"
)

var version = "0.0.1-m0"

type globalFlags struct {
	Verbose    bool
	Env        string
	ConfigPath string
	NoRollback bool
	Force      bool
}

func newRootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "yeet",
		Short:         "plan-before-apply, zero-downtime deploys for compose-first apps (M0 skeleton)",
		Long:          "yeet — plan-before-apply, zero-downtime deploys for compose-first apps.\nAgentless (SSH), journaled, fenced; your compose file is the contract.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVarP(&g.Verbose, "verbose", "v", false, "print every remote command")
	root.PersistentFlags().StringVarP(&g.Env, "env", "e", "production", "environment name")
	root.PersistentFlags().StringVarP(&g.ConfigPath, "config", "c", "yeet.yml", "path to yeet.yml")
	addCommands(root, g)
	addInitCommand(root, g)
	addOpsCommands(root, g)
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// the one line every failure ends on — red where the terminal allows
		ui.New(os.Stderr, false).Failf("yeet: %v", err)
		os.Exit(1)
	}
}
