package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/ui"

	"github.com/labstack/onebox/internal/app"
)

var version = buildinfo.Read().Version

const (
	defaultProjectFile   = "ob.yml"
	alternateProjectFile = "ob.yaml"
)

type globalFlags struct {
	Verbose    bool
	Env        string
	ConfigPath string
	NoRollback bool
	Output     string
	// Images resolves build-sourced workloads. Parsed from --image by the
	// verbs that accept it.
	Images app.Images
}

func newRootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "ob",
		Short:         "onebox — one application, one server",
		Long:          "onebox (ob) — plan-before-apply production operations for one application on one server.\n\nYou describe what the application is in ob.yml (or ob.yaml); Onebox generates\nthe Compose runtime, the names, the routing and the supporting services.\nAgentless over SSH, health-gated, journaled and fenced.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE:          showCommandHelp,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputMode(cmd, g); err != nil {
				return err
			}
			resolveDefaultConfigPath(cmd, g)
			return nil
		},
	}
	root.PersistentFlags().BoolVarP(&g.Verbose, "verbose", "v", false, "print every remote command")
	root.PersistentFlags().StringVarP(&g.Env, "env", "e", "production", "environment name")
	root.PersistentFlags().StringVarP(&g.ConfigPath, "config", "c", defaultProjectFile, "path to the project YAML file")
	root.PersistentFlags().StringVar(&g.Output, "output", "human", "output mode for supported commands: human|json|ndjson (see the CLI reference)")
	addVersionCommand(root, g)
	addDoctorCommand(root, g)
	addCommands(root, g)
	addJobCommand(root, g)
	addInitCommand(root, g)
	addOpsCommands(root, g)
	addBackupCommands(root, g)
	addPreviewCommand(root, g)
	addSchemaCommand(root, g)
	addPreflightCommand(root, g)
	addEjectCommand(root, g)
	addConfigCommand(root, g)
	return root
}

// resolveDefaultConfigPath keeps ob.yml canonical while allowing the other
// standard YAML spelling without a flag. An explicit -c is exact authority:
// it must never be silently redirected to a different file.
func resolveDefaultConfigPath(cmd *cobra.Command, g *globalFlags) {
	if g.ConfigPath != defaultProjectFile || cmd.Flags().Changed("config") {
		return
	}
	if _, err := os.Stat(defaultProjectFile); err == nil || !errors.Is(err, os.ErrNotExist) {
		return
	}
	if info, err := os.Stat(alternateProjectFile); err == nil && !info.IsDir() {
		g.ConfigPath = alternateProjectFile
	}
}

// showCommandHelp makes command groups participate in Cobra's execution
// lifecycle. Persistent validation therefore runs before their human help is
// rendered, so an unsupported machine-output request cannot succeed with text.
func showCommandHelp(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
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
		// the one line every failure ends on — red where the terminal allows
		ui.New(os.Stderr, false).Failf("ob: %v", err)
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			os.Exit(2)
		}
		var coded *cliExitError
		if errors.As(err, &coded) {
			os.Exit(coded.ExitCode())
		}
		os.Exit(1)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		os.Exit(2)
	}
}
