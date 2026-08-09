package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/onebox"
)

func addOpsCommands(root *cobra.Command, g *globalFlags) {
	// supporting/data service apply
	serviceCmd := &cobra.Command{Use: "service", Short: "manage supporting and data services",
		Long: "Manage the supporting services this project declares.\n\n" +
			"They run in their own Compose projects, so no deploy and no rollback can\n" +
			"stop them or remove their volumes. `apply` converges them to what the\n" +
			"project declares; a major version change a driver cannot perform in place\n" +
			"is refused rather than attempted.",
		Args: cobra.NoArgs, RunE: showCommandHelp}
	serviceCmd.AddCommand(&cobra.Command{
		Use:   "apply",
		Short: "planned service convergence — diff shown, destructive mounts refused without --allow-destructive-mounts",
		Long:  "Converge the supporting services to what the project declares.\n\nEach runs in its own Compose project, so this never touches a release and a\nrelease never touches it. A change a driver can apply in place is applied; a\nmajor version change it cannot — a data directory the new version could not\nopen — is refused with what to do instead, rather than replacing the\ncontainer and leaving the data intact and unreachable.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindServiceApply, Force: g.Force,
			}, "service apply")
		},
	})
	serviceCmd.PersistentFlags().BoolVar(&g.Force, "allow-destructive-mounts", false, "proceed past destructive mount changes")
	root.AddCommand(serviceCmd)

	// proxy apply — converge the HOST-scoped managed proxy (shared by every
	// ob app on the box; see proxy.managed)
	proxyCmd := &cobra.Command{Use: "proxy", Short: "manage the host-scoped proxy (proxy.managed: true)",
		Long: "Manage the proxy shared by every application on this box.\n\n" +
			"Host-scoped rather than application-scoped: one proxy holds the ports and\n" +
			"the certificates, and applications register with it. It is taken under a\n" +
			"separate host lock so two applications cannot reconfigure it at once.",
		Args: cobra.NoArgs, RunE: showCommandHelp}
	proxyCmd.AddCommand(&cobra.Command{
		Use:   "apply",
		Short: "converge the shared proxy — diff shown; unchanged config never touches the container (ACME-safe)",
		Long:  "Reconfigure the shared proxy from the applications registered with it.\n\nTaken under the host lock rather than the application's, because the proxy\nbelongs to the box. An application that declares no route does not appear in\nit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindProxyApply, Force: g.Force,
			}, "proxy apply")
		},
	})
	proxyCmd.PersistentFlags().BoolVar(&g.Force, "force", false, "break the held host lock (prints the holder first); also overrides a cross-app config conflict")
	root.AddCommand(proxyCmd)

	// secrets edit | push
	secretsCmd := &cobra.Command{Use: "secrets", Short: "SOPS-encrypted secrets",
		Long: "SOPS-encrypted secrets for this project.\n\n" +
			"`edit` decrypts to a temporary file, opens an editor, and re-encrypts.\n" +
			"`push` renders the decrypted values into the current release on the host\n" +
			"and restarts what reads them. Plaintext never enters the project file, the\n" +
			"generated runtime, or any plan.",
		Args: cobra.NoArgs, RunE: showCommandHelp}
	secretsCmd.AddCommand(&cobra.Command{
		Use:   "edit",
		Short: "open the encrypted file in $EDITOR via sops",
		Long:  "Decrypt the secrets file, open it in $EDITOR, and re-encrypt on save.\n\nPlaintext exists only in a temporary file for the life of the editor. It\nnever enters the project file, the generated runtime, or any plan.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfigOnly(g)
			if err != nil {
				return err
			}
			if specSopsSource(cfg) == "" {
				return fmt.Errorf("no encrypted env_files entry declared in %s — add one as {file: <path>, provider: sops}", g.ConfigPath)
			}
			c := exec.CommandContext(cmd.Context(), "sops", filepath.Join(filepath.Dir(g.ConfigPath), specSopsSource(cfg)))
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	})
	secretsCmd.AddCommand(&cobra.Command{
		Use:   "push",
		Short: "re-render secrets into the live release and restart workloads if changed",
		Long:  "Render the decrypted secrets into the current release on the host and\nrestart the workloads that read them.\n\nA no-op when the values have not changed, so running it twice costs one\nround trip and no restart.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := operationsService(cmd, g).Execute(cmd.Context(), onebox.ExecuteRequest{
				Kind: onebox.KindSecretsPush,
			})
			return err
		},
	})
	root.AddCommand(secretsCmd)

	// destroy
	var destroyVolumes, destroyProxy bool
	destroyCmd := &cobra.Command{
		Use:   "destroy",
		Short: "tear the app down (typed confirmation; volumes kept unless --volumes)",
		Long:  "Tear the application down: every container it owns, its scheduled timers, and\nits state directory.\n\nRequires the application name typed back. Volumes are kept unless --volumes,\nand when they are kept the service credentials are kept with them — a volume\nwhose credential is gone cannot be opened by a new one. The shared proxy\nsurvives unless --proxy and no other application is registered.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc := operationsService(cmd, g)
			binding, err := svc.ResolveExecutionBinding(cmd.Context(), onebox.KindDestroy)
			if err != nil {
				return err
			}
			cfg, err := notificationConfig(g)
			if err != nil {
				return err
			}
			if cfg.Name != binding.Application {
				return fmt.Errorf("configuration changed while preparing destroy — retry")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "This tears down every %s container \u2014 workloads, supporting services and scheduled timers \u2014 and ob's state dir on %s.\nVolumes are %s.\nType the app name (%s) to confirm: ",
				binding.Application, binding.Environment, map[bool]string{true: "REMOVED — data loss", false: "kept"}[destroyVolumes], binding.Application)
			line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if strings.TrimSpace(line) != binding.Application {
				return fmt.Errorf("confirmation mismatch — aborted, nothing touched")
			}
			result, err := svc.Execute(cmd.Context(), onebox.ExecuteRequest{
				Kind: onebox.KindDestroy, RemoveVolumes: destroyVolumes, RemoveProxy: destroyProxy,
				ExpectedBinding: &binding,
			})
			notifyOutcome(cfg, g, "destroy", result.ID, err)
			return err
		},
	}
	destroyCmd.Flags().BoolVar(&destroyVolumes, "volumes", false, "also remove named volumes (DATA LOSS)")
	destroyCmd.Flags().BoolVar(&destroyProxy, "proxy", false, "also remove the shared managed proxy — only when no other app is registered")
	root.AddCommand(destroyCmd)

	// logs
	var follow bool
	var tail int
	logsCmd := &cobra.Command{
		Use:   "logs [workload|service]",
		Short: "compose logs from the current release",
		Long:  "Stream logs from the current release.\n\nNames a workload or service, or omits it for all of them. Reads only.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return err
			}
			defer cleanup()
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return e.Logs(cmd.Context(), name, follow, tail, cmd.OutOrStdout())
		},
	}
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream")
	logsCmd.Flags().IntVar(&tail, "tail", 100, "lines per service")
	root.AddCommand(logsCmd)

	// exec
	root.AddCommand(&cobra.Command{
		Use:   "exec <workload|service> -- <command...>",
		Short: "run a command inside a workload or service container",
		Long:  "Run a command inside a running container.\n\nNames a workload or a supporting service. What the command does is not\nOnebox's business: this is an escape hatch, outside the journal and the\nsafety regime, and nothing it changes is part of any release.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return err
			}
			defer cleanup()
			return e.ExecIn(cmd.Context(), args[0], strings.Join(args[1:], " "), cmd.OutOrStdout())
		},
	})
}

// loadConfigOnly skips compose loading and inference (secrets edit needs no
// host or compose — only cfg.Secrets). CUE validation already ran in Load;
// component/order checks in Validate can't run without the compose project.
func loadConfigOnly(g *globalFlags) (*app.Spec, error) {
	return app.Load(g.ConfigPath)
}

// specSopsSource is the first encrypted entry declared at project scope, which
// is the file `ob secrets edit` opens. An entry declared on an environment or a
// workload is not reachable from here.
//
// "First declared" rather than "first alphabetically": entries are an ordered
// list, and the order is the author's. A project with several encrypted entries
// wanting to edit a later one is a gap this leaves open deliberately rather
// than resolving by a rule nobody stated.
func specSopsSource(spec *app.Spec) string {
	if spec.Runtime == nil {
		return ""
	}
	for _, entry := range spec.Runtime.EnvFiles {
		if entry.Provider == "sops" {
			return entry.File
		}
	}
	return ""
}
