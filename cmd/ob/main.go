package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/ui"
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
		Use:           "ob",
		Short:         "onebox — MCP-first production operations for one box",
		Long:          "onebox (ob) — MCP-first, plan-before-apply production operations for Compose applications on one server.\nAgentless over SSH, health-gated, journaled, and fenced; Compose is the runtime contract and one box is the product scope.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVarP(&g.Verbose, "verbose", "v", false, "print every remote command")
	root.PersistentFlags().StringVarP(&g.Env, "env", "e", "production", "environment name")
	root.PersistentFlags().StringVarP(&g.ConfigPath, "config", "c", "ob.yml", "path to ob.yml")
	addCommands(root, g)
	addInitCommand(root, g)
	addOpsCommands(root, g)
	addMCPCommand(root, g)
	return root
}

func main() {
	// Cancel first so command defers can close SSH sessions and erase staging
	// payloads. A second signal uses the OS default and remains the force-exit.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		<-ctx.Done()
		// stderr is still the same terminal for interactive commands, while MCP
		// reserves stdout exclusively for protocol frames.
		ui.RestoreCursor(os.Stderr)
		stopSignals()
	}()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			os.Exit(130)
		}
		// the one line every failure ends on — red where the terminal allows
		ui.New(os.Stderr, false).Failf("ob: %v", err)
		os.Exit(1)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		os.Exit(130)
	}
}
