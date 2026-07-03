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
	"github.com/pmezard/go-difflib/difflib"
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

	var planOut string
	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "refresh → rendered diff + pinned images + command list → plan artifact",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPlan(cmd, g, planOut)
		},
	}
	planCmd.Flags().StringVarP(&planOut, "out", "o", "yeet-plan.json", "plan artifact path")
	root.AddCommand(planCmd)

	var planFile string
	deployCmd := &cobra.Command{
		Use:   "deploy",
		Short: "release to the environment host with health-gated zero downtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, planFile, false)
		},
	}
	deployCmd.Flags().StringVar(&planFile, "plan", "", "apply a plan artifact (binds config + host state)")
	deployCmd.Flags().BoolVar(&g.NoRollback, "no-rollback", false, "verify failures halt; never auto-rollback")
	deployCmd.Flags().BoolVar(&g.Force, "force", false, "break a held deploy lock (prints the holder first)")
	root.AddCommand(deployCmd)

	root.AddCommand(&cobra.Command{
		Use:   "bootstrap",
		Short: "first contact: dirs + bootstrap hook + registry login + accessories",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, p, err := loadAll(ctx, g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p)
			if err != nil {
				return err
			}
			defer cleanup()
			id := release.NewID(time.Now(), gitShortSHA(filepath.Dir(g.ConfigPath))) + "-bootstrap"
			staging, sc, err := stageRelease(g, cfg, p, id)
			if err != nil {
				return err
			}
			defer sc()
			return e.Bootstrap(ctx, id, staging)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "rollback",
		Short: "re-release the previous release dir (pinned local image)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, "", true)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "resume",
		Short: "continue an interrupted deploy from the journal (fences the old runner)",
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
			return e.Resume(cmd.Context())
		},
	})

	abortCmd := &cobra.Command{
		Use:   "abort",
		Short: "revert an interrupted deploy to the previous release (migration-gated)",
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
			return e.Abort(cmd.Context(), g.Force)
		},
	}
	abortCmd.Flags().BoolVar(&g.Force, "force", false, "abort past a closed migration gate (you assert schema compatibility)")
	root.AddCommand(abortCmd)

	var auditN int
	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "who deployed what, when, from which SHA — incl. failed runs",
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
			return e.Audit(cmd.Context(), auditN)
		},
	}
	auditCmd.Flags().IntVarP(&auditN, "count", "n", 10, "journals to show")
	root.AddCommand(auditCmd)
}

func runPlan(cmd *cobra.Command, g *globalFlags, outPath string) error {
	ctx := cmd.Context()
	cfg, p, err := loadAll(ctx, g)
	if err != nil {
		return err
	}
	if errs := compose.CheckRollable(p, cfg); len(errs) > 0 {
		return fmt.Errorf("not rollable: %v", errs)
	}
	e, cleanup, err := connect(cmd, g, cfg, p)
	if err != nil {
		return err
	}
	defer cleanup()
	out := cmd.OutOrStdout()

	hs, err := e.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	pins, err := e.PinImages(ctx)
	if err != nil {
		return fmt.Errorf("pin images: %w", err)
	}
	id := release.NewID(time.Now(), gitShortSHA(filepath.Dir(g.ConfigPath)))
	rendered, err := compose.Render(p, cfg, id)
	if err != nil {
		return err
	}
	rendered = compose.RewriteSources(rendered, compose.PayloadRewrites(p))

	fmt.Fprintln(out, engine.FidelityContract)
	fmt.Fprintln(out)

	// rendered diff against the live release
	live := ""
	if hs.CurrentRelease != "" {
		res, err := e.T.Run(ctx, "cat '"+release.PathsFor(cfg.App).Releases+"/"+hs.CurrentRelease+"/compose.yaml' 2>/dev/null || true")
		if err != nil {
			return err
		}
		live = res.Stdout
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(live), B: difflib.SplitLines(string(rendered)),
		FromFile: "live (" + orNone(hs.CurrentRelease) + ")", ToFile: "planned (" + id + ")", Context: 3,
	})
	if err != nil {
		return err
	}
	if diff == "" {
		fmt.Fprintln(out, "rendered compose: no change against live release")
	} else {
		fmt.Fprintln(out, diff)
	}

	fmt.Fprintln(out, "images:")
	for svc, ref := range pins {
		mark := "pinned"
		if !strings.Contains(ref, "@sha256:") {
			mark = "TAG-BOUND (unpinned)"
		}
		fmt.Fprintf(out, "  %-12s %s  [%s]\n", svc, ref, mark)
	}
	fmt.Fprintln(out)

	remoteCompose := release.PathsFor(cfg.App).Releases + "/" + id + "/compose.yaml"
	commands := e.Describe(remoteCompose)
	fmt.Fprintln(out, "commands:")
	for _, c := range commands {
		fmt.Fprintln(out, "  "+c)
	}

	cfgBytes, err := os.ReadFile(g.ConfigPath)
	if err != nil {
		return err
	}
	a := &engine.Artifact{
		ID: id, App: cfg.App, Env: g.Env, CreatedAt: time.Now(),
		GitSHA:     gitShortSHA(filepath.Dir(g.ConfigPath)),
		ConfigHash: engine.HashBytes(cfgBytes),
		HostState:  hs, PinnedImages: pins,
		RenderedCompose: string(rendered), Commands: commands,
	}
	if err := a.Save(outPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nplan written to %s — apply with: yeet deploy --plan %s\n", outPath, outPath)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// connect builds the SSH-backed engine for the selected environment.
func connect(cmd *cobra.Command, g *globalFlags, cfg *config.Config, p *ctypes.Project) (*engine.Engine, func(), error) {
	env, err := cfg.Environment(g.Env)
	if err != nil {
		return nil, nil, err
	}
	t, err := transport.NewSSH(env.Hosts[0])
	if err != nil {
		return nil, nil, err
	}
	if g.Verbose {
		t.Logger = func(host, c string) { fmt.Fprintf(cmd.ErrOrStderr(), "[%s] $ %s\n", host, c) }
	}
	cfgBytes, _ := os.ReadFile(g.ConfigPath)
	e := engine.New(cfg, p, t, engine.Options{
		Verbose:    g.Verbose,
		Out:        cmd.OutOrStdout(),
		LocalDir:   filepath.Dir(g.ConfigPath),
		NoRollback: g.NoRollback,
		ForceLock:  g.Force,
		GitSHA:     gitShortSHA(filepath.Dir(g.ConfigPath)),
		ConfigHash: engine.HashBytes(cfgBytes),
	})
	return e, func() { t.Close() }, nil
}

// stageRelease renders + stages the full release payload locally.
func stageRelease(g *globalFlags, cfg *config.Config, p *ctypes.Project, id string) (string, func(), error) {
	rendered, err := compose.Render(p, cfg, id)
	if err != nil {
		return "", nil, err
	}
	snapshot, err := os.ReadFile(g.ConfigPath)
	if err != nil {
		return "", nil, err
	}
	staging, err := os.MkdirTemp("", "yeet-"+cfg.App)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(staging) }
	rewrites, err := compose.StagePayload(p, staging)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	rendered = compose.RewriteSources(rendered, rewrites)
	if err := release.Stage(staging, rendered, snapshot); err != nil {
		cleanup()
		return "", nil, err
	}
	return staging, cleanup, nil
}

func runDeploy(cmd *cobra.Command, g *globalFlags, planFile string, rollback bool) error {
	ctx := cmd.Context()
	cfg, p, err := loadAll(ctx, g)
	if err != nil {
		return err
	}
	if errs := compose.CheckRollable(p, cfg); len(errs) > 0 {
		return fmt.Errorf("not rollable: %v", errs)
	}
	e, cleanup, err := connect(cmd, g, cfg, p)
	if err != nil {
		return err
	}
	defer cleanup()
	if rollback {
		return e.Rollback(ctx)
	}

	if planFile != "" {
		return applyPlan(cmd, g, cfg, p, e, planFile)
	}

	id := release.NewID(time.Now(), gitShortSHA(filepath.Dir(g.ConfigPath)))
	staging, sc, err := stageRelease(g, cfg, p, id)
	if err != nil {
		return err
	}
	defer sc()
	return e.Deploy(ctx, id, staging)
}

// applyPlan deploys the artifact's rendered bytes verbatim after verifying
// the binding: same env, same config, no host drift. Payload files are
// re-staged fresh at apply — a stated fidelity limit.
func applyPlan(cmd *cobra.Command, g *globalFlags, cfg *config.Config, p *ctypes.Project, e *engine.Engine, planFile string) error {
	ctx := cmd.Context()
	a, err := engine.LoadArtifact(planFile)
	if err != nil {
		return err
	}
	cfgBytes, err := os.ReadFile(g.ConfigPath)
	if err != nil {
		return err
	}
	fresh, err := e.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	if err := a.VerifyBinding(g.Env, cfgBytes, fresh); err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "yeet-"+cfg.App)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if _, err := compose.StagePayload(p, staging); err != nil {
		return err
	}
	if err := release.Stage(staging, []byte(a.RenderedCompose), cfgBytes); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "→ applying plan %s (bound, no drift)\n", a.ID)
	return e.Deploy(ctx, a.ID, staging)
}

func gitShortSHA(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short=7", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
