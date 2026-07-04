package main

import (
	"bufio"
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
	"github.com/labstack/yeet/internal/proxy"
	"github.com/labstack/yeet/internal/release"
	"github.com/labstack/yeet/internal/secrets"
	"github.com/labstack/yeet/internal/transport"
)

func loadAll(ctx context.Context, g *globalFlags) (*config.Config, *ctypes.Project, error) {
	return loadAllWith(ctx, g, compose.Load)
}

// loadAllLenient is for READ-ONLY verbs (status, logs, exec, audit) and proxy
// apply — none consume interpolated compose values, so a missing ${VAR:?}
// (e.g. an image version normally resolved by the deploy wrapper) must not
// block a query.
func loadAllLenient(ctx context.Context, g *globalFlags) (*config.Config, *ctypes.Project, error) {
	return loadAllWith(ctx, g, compose.LoadLenient)
}

func loadAllWith(ctx context.Context, g *globalFlags, load func(context.Context, string, string, ...string) (*ctypes.Project, error)) (*config.Config, *ctypes.Project, error) {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	// Defaults yeet can derive without the project: app from the directory,
	// compose from the conventional file. Inference (which needs the project)
	// runs after the load; Validate then checks the fully-resolved config.
	dir := filepath.Dir(g.ConfigPath)
	if cfg.App == "" {
		cfg.App = config.DefaultApp(g.ConfigPath)
	}
	if cfg.Compose == "" {
		cfg.Compose = config.FindCompose(dir)
	}
	composePath := cfg.Compose
	if !filepath.IsAbs(composePath) {
		composePath = filepath.Join(dir, composePath)
	}
	// env_files feed ${VAR} interpolation; resolve them against the compose
	// working dir — the same base InjectEnvFiles/StagePayload use, so the file
	// that feeds interpolation is exactly the one that ships as container env.
	composeDir := filepath.Dir(composePath)
	var envFiles []string
	for _, ef := range cfg.EnvFiles {
		abs := ef
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(composeDir, ef)
		}
		// An env file must live under the project dir so it ships with the
		// release (StagePayload only stages sources inside it). One outside
		// would leave a local-machine path in the compose shipped to the host —
		// a silent runtime failure. Reject it here, mirroring PayloadRewrites'
		// staging predicate, so `yeet validate` catches it.
		if rel, err := filepath.Rel(composeDir, abs); err != nil || strings.HasPrefix(rel, "..") {
			return nil, nil, fmt.Errorf("env_files: %q resolves outside the project (%s) — it must live with the compose file so it ships with the release", ef, composeDir)
		}
		envFiles = append(envFiles, abs)
	}
	p, err := load(ctx, composePath, cfg.App, envFiles...)
	if err != nil {
		return nil, nil, err
	}
	compose.Infer(cfg, p)
	if err := cfg.Validate(); err != nil {
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
		Use:   "config",
		Short: "print the fully-resolved config (defaults + inference applied)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// lenient: inference reads structure (ports/healthchecks/names),
			// never interpolated values — a pure diagnostic like status
			cfg, _, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return err
			}
			b, err := cfg.YAML()
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "render",
		Short: "print the rendered per-release compose (shows the injected delta; env values redacted)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAll(cmd.Context(), g)
			if err != nil {
				return err
			}
			compose.InjectEnvFiles(p, cfg)
			if cfg.Proxy.Managed {
				compose.InjectProxyNetwork(p, cfg, proxyNetwork(cfg))
			}
			out, err := compose.Render(p, cfg, "render-preview")
			if err != nil {
				return err
			}
			// Redact environment values — even a preview must never print
			// secrets to the terminal (design §07).
			out, err = compose.RedactEnvYAML(out)
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
	var deployYes bool
	deployCmd := &cobra.Command{
		Use:   "deploy",
		Short: "show the plan, confirm, and release with health-gated zero downtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, planFile, false, deployYes)
		},
	}
	deployCmd.Flags().StringVar(&planFile, "plan", "", "apply a saved plan artifact (binds config + host state; no prompt)")
	deployCmd.Flags().BoolVarP(&deployYes, "yes", "y", false, "skip the confirmation prompt")
	deployCmd.Flags().BoolVar(&g.NoRollback, "no-rollback", false, "verify failures halt; never auto-rollback")
	deployCmd.Flags().BoolVar(&g.Force, "force", false, "break a held deploy lock (prints the holder first)")
	root.AddCommand(deployCmd)

	bootstrapCmd := &cobra.Command{
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
	}
	// without this, a bootstrap killed while holding the app/host lock can
	// only be retried after the full lock TTL: each retry mints a fresh
	// deploy id, so the same-deploy reclaim path never fires
	bootstrapCmd.Flags().BoolVar(&g.Force, "force", false, "break a held lock left by a crashed bootstrap (prints the holder first)")
	root.AddCommand(bootstrapCmd)

	root.AddCommand(&cobra.Command{
		Use:   "rollback",
		Short: "re-release the previous release dir (pinned local image)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, "", true, true)
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

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "recorded vs actual per role — divergence is the point",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p)
			if err != nil {
				return err
			}
			defer cleanup()
			return e.Status(cmd.Context())
		},
	})

	var auditN int
	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "who deployed what, when, from which SHA — incl. failed runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
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

// preparedPlan bundles a computed-and-displayed plan with the connected engine
// and staged release, so `plan` (save the artifact) and interactive `deploy`
// (prompt, then apply the already-staged bytes) share one code path.
type preparedPlan struct {
	e       *engine.Engine
	art     *engine.Artifact
	staging string
	cleanup func() // closes the connection and removes the staging dir
}

// buildPlan refreshes host state, pins images, stages the release, prints the
// fidelity contract + redacted diff + images + command list, and returns the
// artifact and staging. It mutates nothing on the host.
func buildPlan(cmd *cobra.Command, g *globalFlags) (*preparedPlan, error) {
	ctx := cmd.Context()
	cfg, p, err := loadAll(ctx, g)
	if err != nil {
		return nil, err
	}
	if err := cfg.RunPreflight(filepath.Dir(g.ConfigPath)); err != nil {
		return nil, err
	}
	if errs := compose.CheckRollable(p, cfg); len(errs) > 0 {
		return nil, fmt.Errorf("not rollable: %v", errs)
	}
	e, closeConn, err := connect(cmd, g, cfg, p)
	if err != nil {
		return nil, err
	}
	done := func() { closeConn() }
	fail := func(err error) (*preparedPlan, error) { done(); return nil, err }
	out := cmd.OutOrStdout()

	hs, err := e.Refresh(ctx)
	if err != nil {
		return fail(fmt.Errorf("refresh: %w", err))
	}
	pins, err := e.PinImages(ctx)
	if err != nil {
		return fail(fmt.Errorf("pin images: %w", err))
	}
	id := release.NewID(time.Now(), gitShortSHA(filepath.Dir(g.ConfigPath)))
	// Render through the exact staging path apply uses (p is already pinned by
	// PinImages above), so the plan's stored compose is byte-identical to what
	// apply re-renders. The canonical bytes are what release.Stage wrote.
	staging, sc, err := stageRelease(g, cfg, p, id)
	if err != nil {
		return fail(err)
	}
	done = func() { sc(); closeConn() } // staging now needs cleanup too
	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return fail(err)
	}
	// Everything displayed or persisted is redacted: environment VALUES become
	// content hashes so secrets never reach the terminal or the plan file
	// (design §07). Only the hash travels.
	renderedRedacted, err := compose.RedactEnvYAML(rendered)
	if err != nil {
		return fail(err)
	}

	fmt.Fprintln(out, engine.FidelityContract)
	fmt.Fprintln(out)

	// rendered diff against the live release — both sides redacted.
	live := ""
	if hs.CurrentRelease != "" {
		res, err := e.T.Run(ctx, "cat '"+release.PathsFor(cfg.App).Releases+"/"+hs.CurrentRelease+"/compose.yaml' 2>/dev/null || true")
		if err != nil {
			return fail(err)
		}
		live = res.Stdout
	}
	liveRedacted := ""
	if strings.TrimSpace(live) != "" {
		// Never fall back to raw live on a parse failure — that could expose it.
		if lr, rerr := compose.RedactEnvYAML([]byte(live)); rerr == nil {
			liveRedacted = string(lr)
		}
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(liveRedacted), B: difflib.SplitLines(string(renderedRedacted)),
		FromFile: "live (" + orNone(hs.CurrentRelease) + ")", ToFile: "planned (" + id + ")", Context: 3,
	})
	if err != nil {
		return fail(err)
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
		return fail(err)
	}
	a := &engine.Artifact{
		ID: id, App: cfg.App, Env: g.Env, CreatedAt: time.Now(),
		GitSHA:     gitShortSHA(filepath.Dir(g.ConfigPath)),
		ConfigHash: engine.HashBytes(cfgBytes),
		HostState:  hs, PinnedImages: pins,
		RenderedCompose: string(renderedRedacted), Commands: commands,
	}
	return &preparedPlan{e: e, art: a, staging: staging, cleanup: done}, nil
}

// runPlan writes the plan artifact for the CI/review flow.
func runPlan(cmd *cobra.Command, g *globalFlags, outPath string) error {
	pl, err := buildPlan(cmd, g)
	if err != nil {
		return err
	}
	defer pl.cleanup()
	if err := pl.art.Save(outPath); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nplan written to %s — apply with: yeet deploy --plan %s\n", outPath, outPath)
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

// applyPins rewrites service image refs to the plan's pinned digests so an
// apply-time re-render reproduces the planned images without re-hitting the
// registry (which could resolve a moved tag differently).
func applyPins(p *ctypes.Project, pins map[string]string) {
	for svc, ref := range pins {
		if ref == "" {
			continue
		}
		if s, ok := p.Services[svc]; ok {
			s.Image = ref
			p.Services[svc] = s
		}
	}
}

// stageRelease renders + stages the full release payload locally, including
// decrypted secrets (mode 600) when declared.
// proxyNetwork: the shared ingress network name, defaulted at the point of use.
func proxyNetwork(cfg *config.Config) string {
	if cfg.Proxy.Network != "" {
		return cfg.Proxy.Network
	}
	return proxy.DefaultNetwork
}

func stageRelease(g *globalFlags, cfg *config.Config, p *ctypes.Project, id string) (string, func(), error) {
	staging, err := os.MkdirTemp("", "yeet-"+cfg.App)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(staging) }
	fail := func(err error) (string, func(), error) { cleanup(); return "", nil, err }

	// Order matters: compose applies env_file entries left to right, later
	// overriding earlier. env_files go first, then the SOPS secrets file last,
	// so a decrypted secret always wins over a same-named plaintext key.
	compose.InjectEnvFiles(p, cfg)
	if cfg.Proxy.Managed {
		compose.InjectProxyNetwork(p, cfg, proxyNetwork(cfg))
	}
	if cfg.Secrets != nil {
		envBytes, err := secrets.Render(filepath.Dir(g.ConfigPath), cfg.Secrets.Sops)
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(filepath.Join(staging, secrets.EnvFileName), envBytes, 0o600); err != nil {
			return fail(err)
		}
		compose.InjectSecretsEnv(p, cfg, "./"+secrets.EnvFileName)
	}
	rendered, err := compose.Render(p, cfg, id)
	if err != nil {
		return fail(err)
	}
	// The snapshot is the RESOLVED config (inference applied), so rollback
	// replays this deploy's exact choreography.
	snapshot, err := cfg.YAML()
	if err != nil {
		return fail(err)
	}
	rewrites, err := compose.StagePayload(p, staging)
	if err != nil {
		return fail(err)
	}
	rendered = compose.RewriteSources(rendered, rewrites)
	if err := release.Stage(staging, rendered, snapshot); err != nil {
		return fail(err)
	}
	return staging, cleanup, nil
}

func runDeploy(cmd *cobra.Command, g *globalFlags, planFile string, rollback, yes bool) error {
	ctx := cmd.Context()

	// rollback and the CI-bound `--plan` flow need a connection but not a
	// freshly-shown plan.
	if rollback || planFile != "" {
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
		if err := cfg.RunPreflight(filepath.Dir(g.ConfigPath)); err != nil {
			return err
		}
		return applyPlan(cmd, g, cfg, p, e, planFile)
	}

	// Interactive deploy: show the plan, confirm, then apply the exact bytes we
	// just staged — one command in place of plan → eyeball → deploy --plan.
	pl, err := buildPlan(cmd, g)
	if err != nil {
		return err
	}
	defer pl.cleanup()
	if !yes && !confirm(cmd, "\nApply this plan?") {
		fmt.Fprintln(cmd.OutOrStdout(), "not applied")
		return nil
	}
	return pl.e.Deploy(ctx, pl.art.ID, pl.staging)
}

// confirm reads a yes/no answer; anything but y/yes (incl. EOF on a
// non-interactive stdin) is No — deploys never proceed on ambiguity.
func confirm(cmd *cobra.Command, prompt string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// applyPlan re-renders the release from source and deploys it after verifying
// the binding: same env, same config, no host drift, and a rendered compose
// that still matches the plan. The plan stores only a REDACTED compose (secret
// values hashed), so the real bytes are produced fresh here — secrets never
// persist in the plan file (design §07). Re-rendering with the plan's pins is
// deterministic, so the redacted re-render must equal the plan's; any
// difference means the compose file or a secret value changed — drift.
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
	applyPins(p, a.PinnedImages)
	staging, sc, err := stageRelease(g, cfg, p, a.ID)
	if err != nil {
		return err
	}
	defer sc()
	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return err
	}
	redacted, err := compose.RedactEnvYAML(rendered)
	if err != nil {
		return err
	}
	if string(redacted) != a.RenderedCompose {
		return fmt.Errorf("compose drift: the rendered compose no longer matches the plan (a compose file or secret value changed) — re-plan")
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
