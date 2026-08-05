package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/onebox"
)

func addOpsCommands(root *cobra.Command, g *globalFlags) {
	// supporting/data service apply
	serviceCmd := &cobra.Command{Use: "service", Aliases: []string{"accessory"}, Short: "manage supporting and data services"}
	serviceCmd.AddCommand(&cobra.Command{
		Use:   "apply",
		Short: "planned service convergence — diff shown, destructive mounts refused without --force",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindServiceApply, Force: g.Force,
			}, "service apply")
		},
	})
	serviceCmd.PersistentFlags().BoolVar(&g.Force, "force", false, "proceed past destructive mount changes")
	root.AddCommand(serviceCmd)

	// proxy apply — converge the HOST-scoped managed proxy (shared by every
	// ob app on the box; see proxy.managed)
	proxyCmd := &cobra.Command{Use: "proxy", Short: "manage the host-scoped proxy (proxy.managed: true)"}
	proxyCmd.AddCommand(&cobra.Command{
		Use:   "apply",
		Short: "converge the shared proxy — diff shown; unchanged config never touches the container (ACME-safe)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindProxyApply, Force: g.Force,
			}, "proxy apply")
		},
	})
	proxyCmd.PersistentFlags().BoolVar(&g.Force, "force", false, "break the host lock / override a cross-app config conflict")
	root.AddCommand(proxyCmd)

	// secrets edit | push
	secretsCmd := &cobra.Command{Use: "secrets", Short: "SOPS-encrypted secrets"}
	secretsCmd.AddCommand(&cobra.Command{
		Use:   "edit",
		Short: "open the encrypted file in $EDITOR via sops",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfigOnly(g)
			if err != nil {
				return err
			}
			if cfg.Secrets == nil {
				return fmt.Errorf("no secrets: {sops: ...} declared in %s", g.ConfigPath)
			}
			c := exec.CommandContext(cmd.Context(), "sops", filepath.Join(filepath.Dir(g.ConfigPath), specSopsSource(cfg)))
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	})
	secretsCmd.AddCommand(&cobra.Command{
		Use:   "push",
		Short: "re-render secrets into the live release and restart workloads if changed",
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
		Use:   "logs [component|service]",
		Short: "compose logs from the current release",
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
		Use:   "exec <component|service> -- <command...>",
		Short: "run a command inside a workload or service container",
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

// specSopsSource is the declared SOPS-encrypted file, if one exists. The
// contract allows several secret providers and only SOPS has an
// implementation; a project declaring another gets nothing here rather than a
// silent fallback to a file it never named.
func specSopsSource(spec *app.Spec) string {
	names := make([]string, 0, len(spec.Secrets))
	for name := range spec.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if s := spec.Secrets[name]; s.Provider == "sops" {
			return s.File
		}
	}
	return ""
}
