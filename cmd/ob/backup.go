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
// what can actually be recovered; everything it prints comes from pgBackRest.
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
			"service under the protected image, creates the repository stanza, and takes\n" +
			"the first base backup; only then does the service render as protected.",
		Args: cobra.NoArgs, RunE: showCommandHelp,
	}

	var enableBreakLock bool
	enableCmd := &cobra.Command{
		Use:   "enable <service>",
		Short: "establish protection — restarts the service, creates the stanza, takes the first backup",
		Long: "Make a declared protection policy real.\n\n" +
			"The order is forced by PostgreSQL: a server cannot archive to a stanza that\n" +
			"does not exist, and a stanza cannot be created against a server that is not\n" +
			"already archiving. So this pins the protected image, writes the repository\n" +
			"configuration, restarts the server with archiving on, creates the stanza,\n" +
			"and takes a full backup.\n\n" +
			"The restart is a real restart of the database. It is not complete until the\n" +
			"first backup exists, because a stanza with no backup can recover nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindProtectionEnable, Service: args[0], BreakLock: enableBreakLock,
			}, "backup enable")
		},
	}
	enableCmd.Flags().BoolVar(&enableBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	backupCmd.AddCommand(enableCmd)

	var backupType string
	var createBreakLock bool
	createCmd := &cobra.Command{
		Use:   "create <service>",
		Short: "take a base backup now",
		Long: "Take a base backup of a protected service.\n\n" +
			"Retention counts full generations, so only --type full starts a new one; a\n" +
			"diff or incr backup extends the newest full and is expired with it.\n\n" +
			"WAL archiving runs continuously and is not this command: between backups the\n" +
			"recoverable point keeps advancing on its own.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindBackupCreate, Service: args[0],
				BackupType: backupType, BreakLock: createBreakLock,
			}, "backup create")
		},
	}
	createCmd.Flags().StringVar(&backupType, "type", "full", "full, diff, or incr")
	createCmd.Flags().BoolVar(&createBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	backupCmd.AddCommand(createCmd)

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
			fmt.Fprintf(out, "service   %s\nstanza    %s\nstate     %s\n", status.Service, status.Stanza, status.State)
			if status.ArchiveMin != "" {
				fmt.Fprintf(out, "wal       %s .. %s\n", status.ArchiveMin, status.ArchiveMax)
			}
			if len(status.Generations) == 0 {
				fmt.Fprintln(out, "\nno recoverable generation yet")
				return nil
			}
			fmt.Fprintln(out)
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "LABEL\tTYPE\tCOMPLETED\tWAL")
			for _, generation := range status.Generations {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s..%s\n", generation.Label, generation.Type,
					time.Unix(generation.StoppedAt, 0).UTC().Format(time.RFC3339),
					generation.WALStart, generation.WALStop)
			}
			return w.Flush()
		},
	}
	backupCmd.AddCommand(statusCmd)

	root.AddCommand(backupCmd)
}
