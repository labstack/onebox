package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/labstack/onebox/internal/onebox"
	"github.com/spf13/cobra"
)

// addScheduleCommands wires `ob schedule`: apply reconciles the host units,
// list reads what the host recorded. The read command connects directly,
// like `ob status`; it holds no lock and writes nothing.
func addScheduleCommands(root *cobra.Command, g *globalFlags) {
	addExecutionCommands(root, g)
	scheduleCmd := &cobra.Command{Use: "schedule", Short: "manage host timers for scheduled jobs",
		Long: "Manage the systemd timers generated for scheduled jobs.\n\n" +
			"Timers outlive the Onebox process and the package installed on the operator\n" +
			"workstation. `apply` explicitly reconciles their units after a runner or\n" +
			"configuration change without deploying a release. `list` reads timer state;\n" +
			"job history and logs live under `ob job`.",
		Args: cobra.NoArgs, RunE: showCommandHelp}

	var scheduleBreakLock bool
	scheduleApplyCmd := &cobra.Command{
		Use:   "apply",
		Short: "reconcile scheduled-job units without deploying a release",
		Long:  "Converge every declared scheduled-job timer, service, runner, and failure notifier to what the current Onebox runner generates.\n\nTaken under the application lock and fence so a deploy or host-fired job cannot modify the same runtime concurrently. This is the explicit post-upgrade path; upgrading the local package never mutates a remote host by itself.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindScheduleApply, BreakLock: scheduleBreakLock,
			}, "schedule apply")
		},
	}
	scheduleApplyCmd.Flags().BoolVar(&scheduleBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	scheduleCmd.AddCommand(scheduleApplyCmd)

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "declared scheduled jobs with timer state and next elapse",
		Long:  "List every job that declares a schedule beside what the host's timer says: whether it is active, when it fires next, and when it last fired. A job an operator stopped with `ob schedule pause` shows its timer as paused; who paused it and why are in `ob status` and in the JSON output. Reads only.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			defer cleanup()
			jobs, err := e.ScheduleList(cmd.Context())
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "schedule_list_failed", "scheduled jobs could not be listed", err)
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"jobs": jobs})
			}
			if len(jobs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no scheduled jobs declared")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "JOB\tDEPLOYMENT\tOPERATOR\tCRON\tTZ\tTIMER\tNEXT\tLAST TRIGGER\tPOLICY\tTIMEOUT\tRETRY BUDGET")
			for _, j := range jobs {
				// A paused timer and a broken one are both "inactive" to
				// systemd. Only one of them is somebody's decision, and this
				// is the table people survey jobs in.
				timer := orDash(j.TimerState)
				if j.Paused != nil {
					timer = "paused"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d attempt(s), %s backoff\n",
					j.Name, j.DeploymentPhase, j.OperatorRun, j.Cron, j.Timezone, timer, orDash(j.NextRun), orDash(j.LastTrigger), j.DeployLock, j.Timeout, j.MaxAttempts, j.RetryBudget)
			}
			return w.Flush()
		},
	}
	scheduleCmd.AddCommand(listCmd)

	var pauseReason string
	var pauseBreakLock bool
	pauseCmd := &cobra.Command{
		Use:   "pause <job>",
		Short: "stop a scheduled job's timer until it is resumed",
		Long: "Stop one job's timer. The units stay installed and a deploy keeps updating them, so a fix still lands; only the firing stops, and it stays stopped until `ob schedule resume`.\n\n" +
			"A run already under way is left alone. `--reason` is required and is kept on the host with the operator and the time, because a job that is deliberately not running looks exactly like one that is broken. `ob status` reports a paused job on its own line for the same reason.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindSchedulePause, Job: args[0], Reason: pauseReason, BreakLock: pauseBreakLock,
			}, "schedule pause")
		},
	}
	pauseCmd.Flags().StringVar(&pauseReason, "reason", "", "why this job is being stopped; kept on the host and shown by ob status")
	// A running job holds the application lock for its whole run, and a
	// crashed deploy leaves that lock behind. Without this, pause is refused
	// in exactly the situations someone reaches for it.
	pauseCmd.Flags().BoolVar(&pauseBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	scheduleCmd.AddCommand(pauseCmd)

	var resumeBreakLock bool
	resumeCmd := &cobra.Command{
		Use:   "resume <job>",
		Short: "start a paused scheduled job's timer again",
		Long:  "Start a paused job's timer and clear the record of the pause.\n\nThe next run is the next scheduled elapse: resuming does not run the job now, and does not make up the firings missed while it was paused. Use `ob job run` for an immediate run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindScheduleResume, Job: args[0], BreakLock: resumeBreakLock,
			}, "schedule resume")
		},
	}
	resumeCmd.Flags().BoolVar(&resumeBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	scheduleCmd.AddCommand(resumeCmd)

	root.AddCommand(scheduleCmd)
}

// parseScheduleInputs turns repeated --input NAME=VALUE flags into overrides.
// Only the shape is checked here; names and values are validated against the
// job's declaration by the engine before anything reaches the host.
func parseScheduleInputs(raw []string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range raw {
		name, value, ok := strings.Cut(item, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--input %q must be NAME=VALUE", item)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--input %s given twice", name)
		}
		out[name] = value
	}
	return out, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
