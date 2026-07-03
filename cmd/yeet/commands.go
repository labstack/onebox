package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/labstack/yeet/internal/compose"
	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/engine"
	"github.com/labstack/yeet/internal/release"
	"github.com/labstack/yeet/internal/transport"
)

func loadAll(ctx context.Context, g *globalFlags) (*config.Config, *ctypes.Project, error) {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	composePath := cfg.Compose
	if !filepath.IsAbs(composePath) {
		composePath = filepath.Join(filepath.Dir(g.ConfigPath), composePath)
	}
	p, err := compose.Load(ctx, composePath, cfg.App)
	if err != nil {
		return nil, nil, err
	}
	if err := compose.Classify(p, cfg); err != nil {
		return nil, nil, err
	}
	return cfg, p, nil
}

func addCommands(root *cobra.Command, g *globalFlags) {
	root.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "schema + rollability + class assignment — no side effects",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			if errs := compose.CheckRollable(p, cfg); len(errs) > 0 {
				for _, e := range errs {
					fmt.Fprintln(cmd.OutOrStdout(), "error:", e)
				}
				return fmt.Errorf("%d rollability error(s)", len(errs))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %d services, %d roles\n", len(p.Services), len(cfg.Roles))
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "render",
		Short: "print the rendered per-release compose (shows the injected delta)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			out, err := compose.Render(p, cfg, "render-preview")
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "deploy",
		Short: "release to the environment host with health-gated zero downtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, false)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "rollback",
		Short: "re-release the previous release dir (pinned local image)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, true)
		},
	})
}

func runDeploy(cmd *cobra.Command, g *globalFlags, rollback bool) error {
	ctx := cmd.Context()
	cfg, p, err := loadAll(ctx, g)
	if err != nil {
		return err
	}
	if errs := compose.CheckRollable(p, cfg); len(errs) > 0 {
		return fmt.Errorf("not rollable: %v", errs)
	}
	env, err := cfg.Environment(g.Env)
	if err != nil {
		return err
	}
	t, err := transport.NewSSH(env.Hosts[0])
	if err != nil {
		return err
	}
	defer t.Close()
	if g.Verbose {
		t.Logger = func(host, c string) { fmt.Fprintf(cmd.ErrOrStderr(), "[%s] $ %s\n", host, c) }
	}
	e := engine.New(cfg, p, t, engine.Options{Verbose: g.Verbose, Out: cmd.OutOrStdout()})
	if rollback {
		return e.Rollback(ctx)
	}

	id := release.NewID(time.Now(), gitShortSHA(filepath.Dir(g.ConfigPath)))
	rendered, err := compose.Render(p, cfg, id)
	if err != nil {
		return err
	}
	snapshot, err := os.ReadFile(g.ConfigPath)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "yeet-"+cfg.App)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := release.Stage(staging, rendered, snapshot); err != nil {
		return err
	}
	return e.Deploy(ctx, id, staging)
}

func gitShortSHA(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short=7", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
