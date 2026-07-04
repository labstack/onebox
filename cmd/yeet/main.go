package main

import (
	"os"
	"os/signal"
	"syscall"

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
	// an interrupt mid-spinner must not leave the terminal cursorless —
	// restore, then die with the conventional code (cleanup semantics are
	// unchanged: Ctrl-C was always an abrupt kill; resume/TTL handle it)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		ui.RestoreCursor(os.Stdout)
		os.Exit(130)
	}()
	if err := newRootCmd().Execute(); err != nil {
		// the one line every failure ends on — red where the terminal allows
		ui.New(os.Stderr, false).Failf("yeet: %v", err)
		os.Exit(1)
	}
}
