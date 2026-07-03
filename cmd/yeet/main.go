package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.0.1-m0"

type globalFlags struct {
	Verbose    bool
	Env        string
	ConfigPath string
}

func newRootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "yeet",
		Short:         "plan-before-apply, zero-downtime deploys for compose-first apps (M0 skeleton)",
		Long:          "yeet — plan-before-apply, zero-downtime deploys for compose-first apps.\nM0 walking skeleton: validate, render, deploy, rollback.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVarP(&g.Verbose, "verbose", "v", false, "print every remote command")
	root.PersistentFlags().StringVarP(&g.Env, "env", "e", "production", "environment name")
	root.PersistentFlags().StringVarP(&g.ConfigPath, "config", "c", "yeet.yml", "path to yeet.yml")
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "yeet:", err)
		os.Exit(1)
	}
}
