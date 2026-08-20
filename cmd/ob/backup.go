package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/onebox"
)

// `ob backup` is the operator's whole view of protection.
//
// Three verbs, and the split between them is the point. `enable` is the one
// that changes what the server is — it restarts the database under the
// protected image and does not return until a recoverable backup exists.
// `create` takes another one. `status` asks the repository, not the project,
// what can actually be recovered; every figure comes from the repository itself.
//
// There is deliberately no verb that reports protection as established from the
// project alone. A policy in `ob.yml` is a request, and until `enable` has
// succeeded the service renders as an ordinary unprotected server.
func addBackupCommands(root *cobra.Command, g *globalFlags) {
	backupCmd := &cobra.Command{
		Use:   "backup",
		Short: "protect a data service and inspect what can be recovered",
		Long: "Backup and recovery for the data services this project declares.\n\n" +
			"Protection is physical: a base backup plus continuous WAL archiving to the\n" +
			"off-host repository the project's backup_targets name, which is what makes\n" +
			"recovery to a point in time possible rather than recovery to last night.\n\n" +
			"Declaring a policy does not establish it. `ob backup enable` restarts the\n" +
			"service with archiving on, stages the verified backup tooling, and takes\n" +
			"the first base backup; only then does the service render as protected.",
		Args: cobra.NoArgs, RunE: showCommandHelp,
	}

	var enableBreakLock bool
	enableCmd := &cobra.Command{
		Use:   "enable <service>",
		Short: "establish protection — restarts the service archiving and takes the first backup",
		Long: "Make a declared protection policy real.\n\n" +
			"The order is forced: the credentials are checked, the image is pinned by\n" +
			"registry digest, the verified wal-g binary is staged on the host, and only\n" +
			"then does the server restart with archiving on.\n\n" +
			"The restart is a real restart of the database. It is not complete until the\n" +
			"first base backup exists, because WAL archiving with nothing to replay onto\n" +
			"can recover nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindProtectionEnable, Service: args[0], BreakLock: enableBreakLock,
			}, "backup enable")
		},
	}
	enableCmd.Flags().BoolVar(&enableBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	backupCmd.AddCommand(enableCmd)

	var createBreakLock bool
	createCmd := &cobra.Command{
		Use:   "create <service>",
		Short: "take a base backup now",
		Long: "Take a base backup of a protected service.\n\n" +
			"Every base backup is complete: the space between them is covered by the WAL\n" +
			"stream rather than by differential backups, so there is no type to choose.\n\n" +
			"WAL archiving runs continuously and is not this command. Between backups the\n" +
			"recoverable point keeps advancing on its own; a base backup bounds how much\n" +
			"WAL a recovery has to replay, and how far back the window reaches.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindBackupCreate, Service: args[0], BreakLock: createBreakLock,
			}, "backup create")
		},
	}
	createCmd.Flags().BoolVar(&createBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	backupCmd.AddCommand(createCmd)

	var disableConfirm string
	disableCmd := &cobra.Command{
		Use:   "disable <service>",
		Short: "stop archiving; keep every backup already taken",
		Long: "Take a service out of protection.\n\n" +
			"Archiving stops, the schedules are removed, the service restarts as an\n" +
			"ordinary unprotected one, and its destination credentials are removed from\n" +
			"the host.\n\n" +
			"The repository is not touched: every backup already taken stays where it\n" +
			"is. Reading or recovering from them needs protection enabled again,\n" +
			"because the binary and credentials that reach the repository live in the\n" +
			"protected service.\n\n" +
			"What does stop is the recovery window advancing: from here on there is no\n" +
			"new WAL, so the newest recoverable point is the moment this ran.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if disableConfirm != args[0] {
				return fmt.Errorf(
					"disabling protection for %s stops archiving, so the recovery window stops advancing from now.\n"+
						"Re-run with --confirm %s once you mean it",
					args[0], args[0])
			}
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindProtectionDisable, Service: args[0],
			}, "backup disable")
		},
	}
	disableCmd.Flags().StringVar(&disableConfirm, "confirm", "", "name of the service whose archiving may stop")
	backupCmd.AddCommand(disableCmd)

	var pruneBreakLock bool
	pruneCmd := &cobra.Command{
		Use:   "prune <service>",
		Short: "expire backups outside the declared retention",
		Long: "Expire everything the policy no longer promises to keep.\n\n" +
			"Retention comes from services.<name>.backup.retention.keep,\n" +
			"so this never removes more than the project says it may keep fewer of.\n\n" +
			"WAL older than the oldest retained backup goes with it: WAL that cannot be\n" +
			"replayed onto any surviving base backup recovers nothing and only costs storage.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindBackupPrune, Service: args[0], BreakLock: pruneBreakLock,
			}, "backup prune")
		},
	}
	pruneCmd.Flags().BoolVar(&pruneBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	backupCmd.AddCommand(pruneCmd)

	verifyCmd := &cobra.Command{
		Use:   "verify <service>",
		Short: "prove the archived WAL forms an unbroken chain",
		Long: "Check that the WAL in the repository is continuous.\n\n" +
			"This is the check worth running on a schedule, and it is not implied by a\n" +
			"backup that exited zero. A base backup with a gapped WAL stream recovers to\n" +
			"the backup and no further — a nightly snapshot wearing the label of\n" +
			"point-in-time recovery — and nothing else notices until someone needs it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindAssuranceCheck, Service: args[0],
			}, "backup verify")
		},
	}
	backupCmd.AddCommand(verifyCmd)

	var restoreTo, restoreConfirm string
	var restoreBreakLock bool
	restoreCmd := &cobra.Command{
		Use:   "restore <service>",
		Short: "recover to a point in time and put it in service",
		Long: "Recover a protected service from its repository.\n\n" +
			"The recovered cluster is always built beside the live one, never over it:\n" +
			"the base backup is fetched into a fresh volume, WAL is replayed to the\n" +
			"requested point, and the result has to start and answer a query before\n" +
			"anything touches the running database. A repository that cannot recover\n" +
			"fails while the database it would have replaced is still serving.\n\n" +
			"The data being replaced is copied aside first, under a dated volume name,\n" +
			"and never deleted. A restore is run on a day that is already going badly;\n" +
			"it must not be the step that makes it unrecoverable.\n\n" +
			"Without --to, recovery goes to the newest recoverable point.\n\n" +
			"The service name has to be typed back with --confirm. Onebox's approval flow\n" +
			"binds a recorded confirmation to an exact plan, and a recovery has no plan to\n" +
			"bind to — so the guard is the name of the thing being replaced, which cannot\n" +
			"be given by accident or by a shell history entry meant for another service.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if restoreConfirm != args[0] {
				return fmt.Errorf(
					"restoring replaces the live data of service %s.\n"+
						"Re-run with --confirm %s once you mean it, or use `ob backup drill %s` to prove the repository recovers without touching anything",
					args[0], args[0], args[0])
			}
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindRestoreCutover, Service: args[0],
				RecoveryTarget: restoreTo, BreakLock: restoreBreakLock,
			}, "backup restore")
		},
	}
	restoreCmd.Flags().StringVar(&restoreTo, "to", "", "RFC 3339 point in time to recover to (default: the newest recoverable point)")
	restoreCmd.Flags().StringVar(&restoreConfirm, "confirm", "", "name of the service whose live data may be replaced")
	restoreCmd.Flags().BoolVar(&restoreBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	backupCmd.AddCommand(restoreCmd)

	var drillTo string
	drillCmd := &cobra.Command{
		Use:   "drill <service>",
		Short: "prove the repository recovers, without touching anything",
		Long: "Recover into a throwaway volume, prove the cluster opens and answers, then\n" +
			"discard it. The live service is never touched.\n\n" +
			"This runs the same code as `ob backup restore` and stops before the last\n" +
			"step, which is the point: a drill that exercised a different path would\n" +
			"prove the drill works rather than that the backups do.\n\n" +
			"A backup nobody has restored is a hypothesis.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindRestoreTest, Service: args[0], RecoveryTarget: drillTo,
			}, "backup drill")
		},
	}
	drillCmd.Flags().StringVar(&drillTo, "to", "", "RFC 3339 point in time to prove recoverable (default: the newest recoverable point)")
	backupCmd.AddCommand(drillCmd)

	statusCmd := &cobra.Command{
		Use:   "status <service>",
		Short: "what the repository can recover, read from the repository",
		Long: "Report what is actually recoverable.\n\n" +
			"Every figure comes from the repository rather than from the project: the\n" +
			"policy states what should be true, and this states what is. A service whose\n" +
			"policy is declared but never enabled has no repository to ask, and says so.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := operationsService(cmd, g).ProtectionStatus(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if isStructuredOutput(g) {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(status)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "service     %s\nrepository  %s\n", status.Service, status.Repository)
			for _, issue := range status.RuntimeIssues {
				fmt.Fprintf(out, "drift       %s\n", issue)
			}
			if len(status.Generations) == 0 {
				fmt.Fprintln(out, "\nno recoverable base backup yet")
				return nil
			}
			fmt.Fprintf(out, "recoverable to  %s or later, as far as the archived WAL reaches\n\n", status.RecoverableTo)
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "BACKUP\tCOMPLETED\tFROM WAL")
			for _, generation := range status.Generations {
				fmt.Fprintf(w, "%s\t%s\t%s\n", generation.Label,
					time.Unix(generation.StoppedAt, 0).UTC().Format(time.RFC3339), generation.WALStart)
			}
			return w.Flush()
		},
	}
	backupCmd.AddCommand(statusCmd)

	root.AddCommand(backupCmd)
}
