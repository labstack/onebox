package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/labstack/onebox/internal/onebox"
	"github.com/spf13/cobra"
)

// addScheduleCommands wires `ob schedule`: apply reconciles the host units,
// list, history and logs read what the host recorded. The read commands
// connect directly, like `ob status`; they hold no lock and write nothing.
func addScheduleCommands(root *cobra.Command, g *globalFlags) {
	scheduleCmd := &cobra.Command{Use: "schedule", Short: "manage host timers for scheduled jobs",
		Long: "Manage the systemd timers generated for scheduled jobs.\n\n" +
			"Timers outlive the Onebox process and the package installed on the operator\n" +
			"workstation. `apply` explicitly reconciles their units after a runner or\n" +
			"configuration change without deploying a release. `list`, `history` and `logs`\n" +
			"read the timer state and the run records the host keeps in its journal.",
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
		Long:  "List every job that declares a schedule beside what the host's timer says: whether it is active, when it fires next, and when it last fired. Reads only.",
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
			fmt.Fprintln(w, "JOB\tCRON\tTZ\tTIMER\tNEXT\tLAST TRIGGER\tPOLICY\tTIMEOUT")
			for _, j := range jobs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					j.Name, j.Cron, j.Timezone, orDash(j.TimerState), orDash(j.NextRun), orDash(j.LastTrigger), j.DeployLock, j.Timeout)
			}
			return w.Flush()
		},
	}
	scheduleCmd.AddCommand(listCmd)

	var historyCount int
	historyCmd := &cobra.Command{
		Use:   "history <job>",
		Short: "run records of one scheduled job, newest first",
		Long:  "Read the run records the host wrote for one scheduled job. Each record is one activation: run id, trigger, release, start and end, attempts, exit status, outcome and, for a manual run, its inputs.\n\nRecords live in the host journal with syslog identifier ob-run and the job's unit in their ONEBOX_UNIT field; retention is the journal's. Reads only.",
		Args:  cobra.ExactArgs(1),
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
			records, err := e.ScheduleHistory(cmd.Context(), args[0], historyCount)
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "schedule_history_failed", "run history could not be read", err)
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"job": args[0], "runs": records})
			}
			if len(records) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no recorded runs for %s\n", args[0])
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "STARTED\tOUTCOME\tDURATION\tATTEMPTS\tEXIT\tTRIGGER\tRELEASE\tRUN")
			for _, r := range records {
				exit := "-"
				if r.ExitStatus != nil {
					exit = strconv.Itoa(*r.ExitStatus)
				}
				fmt.Fprintf(w, "%s\t%s\t%ds\t%d\t%s\t%s\t%s\t%s\n",
					r.StartedAt, r.Outcome, r.DurationSeconds, r.Attempts, exit, r.Trigger, orDash(r.Release), r.Run)
			}
			return w.Flush()
		},
	}
	historyCmd.Flags().IntVarP(&historyCount, "count", "n", 20, "number of newest runs to show")
	scheduleCmd.AddCommand(historyCmd)

	var logsRun string
	logsCmd := &cobra.Command{
		Use:   "logs <job>",
		Short: "journal of one scheduled run",
		Long:  "Stream the host journal for one run of a scheduled job: by default the newest recorded run, or the run named with --run. The run id is systemd's invocation id, so the output is exactly that activation. Reads only.\n\nLog bytes are operator-controlled and may contain secrets; Onebox does not claim\nto redact passthrough output.",
		Args:  cobra.ExactArgs(1),
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
			if g.Output == "json" {
				var stdout, stderr bytes.Buffer
				err = e.ScheduleLogs(cmd.Context(), args[0], logsRun, &stdout, &stderr)
				data := map[string]any{
					"job": args[0], "run": logsRun, "stdout": stdout.String(), "stderr": stderr.String(),
					"passthrough_unredacted": true,
				}
				if err != nil {
					publicErr := publicError(err, "schedule_logs_failed", "run logs could not be read")
					publicErr.Details = data
					if writeErr := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); writeErr != nil {
						return writeErr
					}
					return withExitCode(err, 1)
				}
				return writeFiniteSuccess(cmd, g, data)
			}
			if g.Output == "ndjson" {
				stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
				err = e.ScheduleLogs(cmd.Context(), args[0], logsRun, stream.channelWriter("stdout"), stream.channelWriter("stderr"))
				data := map[string]any{"job": args[0], "run": logsRun, "passthrough_unredacted": true}
				if err != nil {
					if writeErr := stream.terminal(cliOutcomeError, nil, publicError(err, "schedule_logs_failed", "run logs could not be read")); writeErr != nil {
						return writeErr
					}
					return withExitCode(err, 1)
				}
				return stream.terminal(cliOutcomeSuccess, data, nil)
			}
			return e.ScheduleLogs(cmd.Context(), args[0], logsRun, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	logsCmd.Flags().StringVar(&logsRun, "run", "", "run id from ob schedule history; default the newest run")
	scheduleCmd.AddCommand(logsCmd)

	var runInputs []string
	var runWait, runBreakLock bool
	runCmd := &cobra.Command{
		Use:   "run <job>",
		Short: "start a scheduled job now with declared inputs",
		Long: "Start one scheduled job's unit now, with values for its declared inputs. Values are validated on the workstation against the declaration; an undeclared name or a value outside its enum or pattern is refused before anything reaches the host.\n\n" +
			"Only a job with data_effect none may run this way; a migration or destructive job keeps the sealed plan of ob job run. The request is journaled as schedule_run with the operator and inputs, and the host record carries the operation id, so ob audit and ob schedule history join on it.\n\n" +
			"The outcome is the run record: ob schedule history <job>, or --wait to block until the unit exits and print it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := parseScheduleInputs(runInputs)
			if err != nil {
				return writeEarlyOperationFailure(cmd, g, codedError("schedule_input_invalid", "%v", err))
			}
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindScheduleRun, Job: args[0], Inputs: inputs, Wait: runWait, BreakLock: runBreakLock,
			}, "schedule run")
		},
	}
	runCmd.Flags().StringArrayVar(&runInputs, "input", nil, "input override as NAME=VALUE; repeatable")
	runCmd.Flags().BoolVar(&runWait, "wait", false, "block until the unit exits and report the run record")
	runCmd.Flags().BoolVar(&runBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	scheduleCmd.AddCommand(runCmd)

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
