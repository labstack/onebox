package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/release"
	"github.com/labstack/yeet/internal/secrets"
)

func addOpsCommands(root *cobra.Command, g *globalFlags) {
	// accessory apply
	accessory := &cobra.Command{Use: "accessory", Short: "manage stateful services"}
	accessory.AddCommand(&cobra.Command{
		Use:   "apply",
		Short: "planned convergence of accessories — diff shown, destructive mounts refused without --force",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p)
			if err != nil {
				return err
			}
			defer cleanup()
			id := release.NewID(time.Now(), gitShortSHA(filepath.Dir(g.ConfigPath))) + "-acc"
			staging, sc, err := stageRelease(g, cfg, p, id)
			if err != nil {
				return err
			}
			defer sc()
			return e.AccessoryApply(cmd.Context(), id, staging, g.Force)
		},
	})
	accessory.PersistentFlags().BoolVar(&g.Force, "force", false, "proceed past destructive mount changes")
	root.AddCommand(accessory)

	// proxy apply — converge the HOST-scoped managed proxy (shared by every
	// yeet app on the box; see proxy.managed)
	proxyCmd := &cobra.Command{Use: "proxy", Short: "manage the host-scoped proxy (proxy.managed: true)"}
	proxyCmd.AddCommand(&cobra.Command{
		Use:   "apply",
		Short: "converge the shared proxy — diff shown; unchanged config never touches the container (ACME-safe)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p)
			if err != nil {
				return err
			}
			defer cleanup()
			id := release.NewID(time.Now(), gitShortSHA(filepath.Dir(g.ConfigPath))) + "-proxy"
			return e.ProxyApply(cmd.Context(), id, g.Force)
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
			c := exec.CommandContext(cmd.Context(), "sops", filepath.Join(filepath.Dir(g.ConfigPath), cfg.Secrets.Sops))
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	})
	secretsCmd.AddCommand(&cobra.Command{
		Use:   "push",
		Short: "re-render secrets into the live release and bounce roles if changed",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			if cfg.Secrets == nil {
				return fmt.Errorf("no secrets: {sops: ...} declared in %s", g.ConfigPath)
			}
			envBytes, err := secrets.Render(filepath.Dir(g.ConfigPath), cfg.Secrets.Sops)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p)
			if err != nil {
				return err
			}
			defer cleanup()
			return e.SecretsPush(cmd.Context(), envBytes)
		},
	})
	root.AddCommand(secretsCmd)

	// destroy
	var destroyVolumes, destroyProxy bool
	destroyCmd := &cobra.Command{
		Use:   "destroy",
		Short: "tear the app down (typed confirmation; volumes kept unless --volumes)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "This tears down every %s container and yeet's state dir on %s.\nVolumes are %s.\nType the app name (%s) to confirm: ",
				cfg.App, g.Env, map[bool]string{true: "REMOVED — data loss", false: "kept"}[destroyVolumes], cfg.App)
			line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if strings.TrimSpace(line) != cfg.App {
				return fmt.Errorf("confirmation mismatch — aborted, nothing touched")
			}
			e, cleanup, err := connect(cmd, g, cfg, p)
			if err != nil {
				return err
			}
			defer cleanup()
			return e.Destroy(cmd.Context(), destroyVolumes, destroyProxy)
		},
	}
	destroyCmd.Flags().BoolVar(&destroyVolumes, "volumes", false, "also remove named volumes (DATA LOSS)")
	destroyCmd.Flags().BoolVar(&destroyProxy, "proxy", false, "also remove the shared managed proxy — only when no other app is registered")
	root.AddCommand(destroyCmd)

	// logs
	var follow bool
	var tail int
	logsCmd := &cobra.Command{
		Use:   "logs [role|service]",
		Short: "compose logs from the current release",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p)
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
		Use:   "exec <role|service> -- <command...>",
		Short: "run a command inside a role's container",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p)
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
// the role/order checks in Validate can't run without the compose project.
func loadConfigOnly(g *globalFlags) (*config.Config, error) {
	return config.Load(g.ConfigPath)
}
