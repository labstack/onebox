package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/onebox"
)

type unavailableScheduledExecutor struct{}

func (unavailableScheduledExecutor) ExecuteScheduledLifecycle(context.Context, onebox.ScheduledLifecycleExecution) error {
	return errors.New("scheduled lifecycle backend is not available for this operation in the current build")
}

func executeEnvelope(ctx context.Context, path string) error {
	envelope, err := onebox.LoadScheduledOperationEnvelope(path)
	if err != nil {
		return fmt.Errorf("load scheduled envelope: %w", err)
	}
	service := onebox.New(onebox.Options{ScheduledLifecycleExecutor: unavailableScheduledExecutor{}})
	runner := onebox.ScheduledRunner{Executor: service}
	return runner.ExecuteRecurring(ctx, envelope, rand.Reader)
}

func newRootCmd(runEnvelope func(context.Context, string) error) *cobra.Command {
	if runEnvelope == nil {
		runEnvelope = executeEnvelope
	}
	root := &cobra.Command{
		Use:           "ob-scheduled-runner",
		Short:         "short-lived Onebox scheduled lifecycle runner",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
	}
	run := &cobra.Command{
		Use:   "run <sealed-envelope>",
		Short: "execute one sealed scheduled operation and exit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvelope(cmd.Context(), args[0])
		},
	}
	version := &cobra.Command{
		Use:   "version",
		Short: "print runner and protocol versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Read()
			compatibility := onebox.CurrentScheduledRunnerCompatibility()
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s runner_protocol=%d envelope_protocols=%d-%d cli_protocols=%d-%d\n",
				info.Version, compatibility.RunnerProtocol,
				compatibility.EnvelopeProtocols.Minimum, compatibility.EnvelopeProtocols.Maximum,
				compatibility.CLIProtocols.Minimum, compatibility.CLIProtocols.Maximum)
			return err
		},
	}
	root.AddCommand(run, version)
	return root
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCmd(nil).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ob-scheduled-runner:", err)
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
