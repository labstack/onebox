package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/labstack/onebox/internal/onebox"
	"github.com/spf13/cobra"
)

func addExecutionCommands(root *cobra.Command, g *globalFlags) {
	executionCmd := &cobra.Command{
		Use: "execution", Short: "inspect and recover durable job executions",
		Long: "Inspect durable job checkpoints saved on the managed host. Resume uses the original inputs and successful step outputs, and requires the original release and compatible runtime. Applications remain responsible for idempotency when an interrupted attempt is repeated.",
		Args: cobra.NoArgs, RunE: showCommandHelp,
	}
	var count int
	listCmd := &cobra.Command{
		Use: "list", Short: "list durable executions, newest first", Args: cobra.NoArgs,
		Long: "List the newest durable executions saved on the managed host, including their job, state, original release and provisional resume eligibility. Reads checkpoint files independently of journal retention and the current job declarations.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if count < 1 || count > 1000 {
				return writeStructuredCommandFailure(cmd, g, "execution_count_invalid", "execution count must be between 1 and 1000", fmt.Errorf("count must be between 1 and 1000"))
			}
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			defer cleanup()
			records, err := e.ExecutionList(cmd.Context(), count)
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "execution_list_failed", "durable executions could not be listed", err)
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"executions": records})
			}
			if len(records) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "no durable executions recorded")
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "EXECUTION\tJOB\tSTATE\tRELEASE\tRESUMABLE")
			for _, record := range records {
				fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\n", record["id"], record["job"], record["state"], record["release"], record["resumable"])
			}
			return w.Flush()
		},
	}
	listCmd.Flags().IntVarP(&count, "count", "n", 20, "number of newest executions to show (1–1000)")
	inspectCmd := &cobra.Command{
		Use: "inspect <id>", Short: "show saved inputs, step outcomes, and resume eligibility", Args: cobra.ExactArgs(1),
		Long: "Read a durable execution by its stable ID, including original inputs, committed outputs, step attempts and systemd invocation IDs. Works independently of the current job declaration. Resume eligibility is provisional: the runner rechecks compatibility and ownership under locks before running.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			defer cleanup()
			record, err := e.ExecutionInspect(cmd.Context(), args[0])
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "execution_inspect_failed", "durable execution could not be read", err)
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"execution": record})
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(record)
		},
	}
	var wait, resumeBreakLock bool
	resumeCmd := &cobra.Command{
		Use: "resume <id>", Short: "resume an unsuccessful execution from saved checkpoints",
		Long: "Resume an unsuccessful, unexpired execution with its original inputs and saved outputs. Completed steps are skipped. The host refuses resume while prior work is active or the original release, workflow definition, image, or service runtime is incompatible. An interrupted step may repeat effects; use the stable execution and step IDs for application deduplication.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindExecutionResume, ExecutionID: args[0], Wait: wait, BreakLock: resumeBreakLock}, "execution resume")
		},
	}
	resumeCmd.Flags().BoolVar(&wait, "wait", false, "wait for the resumed activation to finish")
	resumeCmd.Flags().BoolVar(&resumeBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	var abandonBreakLock bool
	abandonCmd := &cobra.Command{
		Use: "abandon <id>", Short: "end resumability and release an execution's retention hold",
		Long: "Mark an inactive execution abandoned. It can no longer be resumed and stops protecting its release from cleanup. Saved execution evidence remains available for inspection. Active work must stop before it can be abandoned.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindExecutionAbandon, ExecutionID: args[0], BreakLock: abandonBreakLock}, "execution abandon")
		},
	}
	abandonCmd.Flags().BoolVar(&abandonBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	executionCmd.AddCommand(listCmd, inspectCmd, resumeCmd, abandonCmd)
	root.AddCommand(executionCmd)
}
