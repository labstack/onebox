package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/notify"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/secrets"
	"github.com/labstack/onebox/internal/transport"
	"github.com/labstack/onebox/internal/ui"
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
	// Defaults ob can derive without the project: app from the directory,
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
		// staging predicate, so `ob validate` catches it.
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
	planCmd.Flags().StringVarP(&planOut, "out", "o", "ob-plan.json", "plan artifact path")
	root.AddCommand(planCmd)

	var planFile string
	var deployYes, deployRedeploy bool
	deployCmd := &cobra.Command{
		Use:   "deploy",
		Short: "show the plan, confirm, and release with health-gated zero downtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, planFile, false, deployYes, deployRedeploy)
		},
	}
	deployCmd.Flags().StringVar(&planFile, "plan", "", "apply a saved plan artifact (binds config + host state; no prompt)")
	deployCmd.Flags().BoolVarP(&deployYes, "yes", "y", false, "skip the confirmation prompt")
	deployCmd.Flags().BoolVar(&deployRedeploy, "redeploy", false, "deploy even when nothing changed (fresh roll of identical content)")
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
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
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
			return runDeploy(cmd, g, "", true, true, false)
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
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return err
			}
			defer cleanup()
			err = e.Resume(cmd.Context())
			notifyOutcome(cfg, g, "resume", "", err)
			return err
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
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return err
			}
			defer cleanup()
			err = e.Abort(cmd.Context(), g.Force)
			notifyOutcome(cfg, g, "abort", "", err)
			return err
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
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return err
			}
			defer cleanup()
			return e.Status(cmd.Context())
		},
	})

	var lsHost string
	var lsJSON, lsFailOnDrift, lsIncomplete bool
	lsCmd := &cobra.Command{
		Use:   "ls",
		Short: "every app on the host — release, health, drift (host-wide, config-free)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, g, lsHost, lsJSON, lsFailOnDrift, lsIncomplete)
		},
	}
	lsCmd.Flags().StringVar(&lsHost, "host", "", "connect directly to [user@]host[:port] instead of using an ob.yml")
	lsCmd.Flags().BoolVar(&lsJSON, "json", false, "emit JSON (apps sorted alphabetically)")
	lsCmd.Flags().BoolVar(&lsFailOnDrift, "fail-on-drift", false, "exit non-zero if the managed proxy is down or any app is not running, running unrecorded, or diverged")
	lsCmd.Flags().BoolVar(&lsIncomplete, "incomplete", false, "also flag apps with an unfinished deploy (one extra host read)")
	root.AddCommand(lsCmd)

	var auditN int
	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "who deployed what, when, from which SHA — incl. failed runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return err
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
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
	cfg     *config.Config
	e       *engine.Engine
	art     *engine.Artifact
	staging string
	noop    bool   // content-identical to live: compose (label-invariant) AND payload digest
	cleanup func() // closes the connection and removes the staging dir
}

// notifyOutcome pushes a mutating verb's outcome to the configured webhook.
// Fail-open: a webhook problem is a stderr warning, never the verb's result.
func notifyOutcome(cfg *config.Config, g *globalFlags, verb, deployID string, err error) {
	if cfg == nil || cfg.Notify == nil {
		return
	}
	host := ""
	if env, eerr := cfg.Environment(g.Env); eerr == nil && len(env.Hosts) == 1 {
		host = env.Hosts[0]
	}
	p := notify.Payload{
		App: cfg.App, Env: g.Env, Host: host, Verb: verb, DeployID: deployID,
		Status: "ok", Operator: journal.DefaultOperator(),
	}
	if err != nil {
		p.Status, p.Error = "fail", err.Error()
	}
	if nerr := notify.Send(cfg.Notify, p); nerr != nil {
		fmt.Fprintf(os.Stderr, "warn: notify webhook: %v\n", nerr)
	}
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
	pu := newUI(cmd, g)
	// the silent stretch between `deploy` and the plan: ssh dial, host
	// refresh, one registry round-trip per image — show where the time goes
	busy, stopBusy := pu.Busy("connecting to " + cfg.App)
	defer stopBusy()
	e, closeConn, err := connect(cmd, g, cfg, p, pu)
	if err != nil {
		return nil, err
	}
	done := func() { closeConn() }
	fail := func(err error) (*preparedPlan, error) { stopBusy(); done(); return nil, err }
	out := cmd.OutOrStdout()

	busy("refreshing host state")
	hs, err := e.Refresh(ctx)
	if err != nil {
		return fail(fmt.Errorf("refresh: %w", err))
	}
	busy("pinning images (registry digests)")
	pins, err := e.PinImages(ctx)
	if err != nil {
		return fail(fmt.Errorf("pin images: %w", err))
	}
	busy("rendering + staging release")
	id := release.NewID(time.Now(), gitShortSHA(filepath.Dir(g.ConfigPath)))
	// Render through the exact staging path apply uses (p is already pinned by
	// PinImages above), so the plan's stored compose is byte-identical to what
	// apply re-renders. The canonical bytes are what release.Stage wrote.
	staging, sc, err := stageRelease(g, cfg, p, id)
	if err != nil {
		return fail(err)
	}
	stopBusy()
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

	u := e.Opts.UI
	u.Header("plan " + id)
	for _, l := range strings.Split(engine.FidelityContract, "\n") {
		u.Println(u.Dim(l)) // line-by-line: multi-line Render pads a block
	}
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
	noop := false
	if diff == "" || engine.OnlyReleaseLabelsChanged(liveRedacted, string(renderedRedacted)) {
		// the compose is content-identical — but env files and secrets ship
		// BESIDE it, so only a payload digest match proves a true no-op
		payloadSame := false
		if hs.CurrentRelease != "" {
			localD, lerr := engine.LocalPayloadDigest(staging)
			remoteD, rerr := e.RemotePayloadDigest(ctx, hs.CurrentRelease)
			payloadSame = lerr == nil && rerr == nil && localD != "" && localD == remoteD
		}
		if payloadSame {
			noop = true
			u.Infof("no changes — compose (release labels aside) and payload are byte-identical to live")
		} else {
			u.Infof("compose unchanged (release labels aside) — but the payload differs (env files / secrets); deploying ships it")
		}
	} else {
		u.Diff(diff)
		fmt.Fprintln(out)
	}

	u.Println(u.Bold("images:"))
	for svc, ref := range pins {
		mark := u.OK("[pinned]")
		if !strings.Contains(ref, "@sha256:") {
			mark = u.Warn("[TAG-BOUND (unpinned)]")
		}
		name, digest := ref, ""
		if i := strings.Index(ref, "@"); i >= 0 {
			name, digest = ref[:i], ref[i:]
		}
		u.Println(fmt.Sprintf("  %-12s %s%s  %s", svc, name, u.Dim(digest), mark))
	}
	fmt.Fprintln(out)

	remoteCompose := release.PathsFor(cfg.App).Releases + "/" + id + "/compose.yaml"
	commands := e.Describe(remoteCompose)
	u.Println(u.Bold("commands:"))
	for _, c := range commands {
		switch {
		case strings.HasPrefix(c, " "): // sub-command / branch line
			u.Println("  " + u.Dim(c))
		case strings.HasSuffix(c, ":"): // release <role> (...) header
			u.Println("  " + u.Bold(c))
		default: // "job X (...): <cmd>" / "hook X (...): <cmd>" — bold label, dim command
			if i := strings.Index(c, "): "); i >= 0 {
				u.Println("  " + u.Bold(c[:i+2]) + " " + u.Dim(c[i+3:]))
			} else {
				u.Println("  " + c)
			}
		}
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
	return &preparedPlan{cfg: cfg, e: e, art: a, staging: staging, noop: noop, cleanup: done}, nil
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
	fmt.Fprintf(cmd.OutOrStdout(), "\nplan written to %s — apply with: ob deploy --plan %s\n", outPath, outPath)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// connect builds the SSH-backed engine for the selected environment.
// newUI builds the one UI instance a command shares across the connect
// spinner, narrative, and command log — one stream, one line discipline.
func newUI(cmd *cobra.Command, g *globalFlags) *ui.UI {
	return ui.New(cmd.OutOrStdout(), g.Verbose)
}

func connect(cmd *cobra.Command, g *globalFlags, cfg *config.Config, p *ctypes.Project, u *ui.UI) (*engine.Engine, func(), error) {
	env, err := cfg.Environment(g.Env)
	if err != nil {
		return nil, nil, err
	}
	t, err := transport.NewSSH(env.Hosts[0])
	if err != nil {
		return nil, nil, err
	}
	t.Logger = u.Cmd
	cfgBytes, _ := os.ReadFile(g.ConfigPath)
	e := engine.New(cfg, p, t, engine.Options{
		Verbose:    g.Verbose,
		UI:         u,
		Out:        cmd.OutOrStdout(),
		LocalDir:   filepath.Dir(g.ConfigPath),
		NoRollback: g.NoRollback,
		ForceLock:  g.Force,
		GitSHA:     gitShortSHA(filepath.Dir(g.ConfigPath)),
		ConfigHash: engine.HashBytes(cfgBytes),
	})
	return e, func() { t.Close() }, nil
}

// runList drives `ob ls` — the host-wide overview. It resolves a transport from
// either --host or the ambient ob.yml, then renders engine.HostList.
func runList(cmd *cobra.Command, g *globalFlags, host string, jsonOut, failOnDrift, incomplete bool) error {
	ctx := cmd.Context()
	// In --json mode the spinner and any connect narrative must not touch
	// stdout — that stream is the machine payload (`ob ls --json | jq`). Route
	// the human UI to stderr so progress still shows on a slow link without
	// corrupting the JSON.
	u := newUI(cmd, g)
	if jsonOut {
		u = ui.New(cmd.ErrOrStderr(), g.Verbose)
	}
	t, closeT, err := dialHost(ctx, cmd, g, host, u)
	if err != nil {
		return err
	}
	defer closeT()

	ov, err := engine.HostList(ctx, t, u, engine.ListOptions{Incomplete: incomplete})
	if err != nil {
		return err
	}
	// The drift gate applies to BOTH output modes — `ob ls --json --fail-on-drift`
	// is the canonical CI shape (payload to jq, exit code gates the build), so it
	// must not silently exit 0. Emit the chosen output first, then return the
	// gate error so the exit code reflects drift either way.
	var driftErr error
	if failOnDrift && ov.HasProblems() {
		driftErr = fmt.Errorf("ob ls: drift detected (--fail-on-drift)")
	}
	if jsonOut {
		if err := writeHostJSON(cmd.OutOrStdout(), ov); err != nil {
			return err
		}
		return driftErr
	}
	renderHostList(u, cmd.OutOrStdout(), t.Host(), ov)
	return driftErr
}

// dialHost opens the transport for a host-level command: --host connects
// directly (no ob.yml), otherwise the ambient config's env host is used.
func dialHost(ctx context.Context, cmd *cobra.Command, g *globalFlags, host string, u *ui.UI) (transport.Transport, func(), error) {
	if host != "" {
		s, err := transport.NewSSH(host)
		if err != nil {
			return nil, nil, err
		}
		s.Logger = u.Cmd
		return s, func() { s.Close() }, nil
	}
	cfg, p, err := loadAllLenient(ctx, g)
	if err != nil {
		return nil, nil, fmt.Errorf("ob ls needs an ob.yml for the host, or --host [user@]host: %w", err)
	}
	e, cleanup, err := connect(cmd, g, cfg, p, u)
	if err != nil {
		return nil, nil, err
	}
	return e.T, cleanup, nil
}

func renderHostList(u *ui.UI, w io.Writer, host string, ov engine.HostOverview) {
	if ov.Proxy.Managed {
		switch {
		case !ov.Proxy.Running:
			u.Println(fmt.Sprintf("proxy %-10s %s", proxy.ContainerName, u.Warn("NOT RUNNING ⚠ — `ob proxy apply`")))
		case ov.Proxy.Health != "healthy":
			u.Println(fmt.Sprintf("proxy %-10s %s", proxy.ContainerName, u.Warn(ov.Proxy.Health)))
		default:
			u.Println(fmt.Sprintf("proxy %-10s %s", proxy.ContainerName, u.OK(ov.Proxy.Health)))
		}
		fmt.Fprintln(w)
	}
	if len(ov.Apps) == 0 {
		fmt.Fprintf(w, "no apps deployed on %s\n", host)
		return
	}
	u.Println(u.Bold(fmt.Sprintf("%-14s %-28s %-8s %-12s %-6s %s", "APP", "RECORDED", "RUNNING", "HEALTH", "PROXY", "STATE")))
	rows := append([]engine.AppRow(nil), ov.Apps...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Problem() && !rows[j].Problem() })
	for _, r := range rows {
		proxied := "-"
		if r.Proxied {
			proxied = "✓"
		}
		rec := r.Recorded
		if rec == "" {
			rec = "-"
		}
		state := u.OK(r.StateLabel())
		if r.Problem() {
			state = u.Warn(r.StateLabel() + " ⚠")
		}
		if r.Incomplete {
			state += u.Warn("  INCOMPLETE")
		}
		u.Println(fmt.Sprintf("%-14s %-28s %-8d %-12s %-6s %s", r.App, rec, r.Running, r.Health, proxied, state))
	}
	if ov.Foreign > 0 {
		fmt.Fprintln(w)
		u.Println(u.Dim(fmt.Sprintf("%d foreign compose project(s) not managed by ob", ov.Foreign)))
	}
}

func writeHostJSON(w io.Writer, ov engine.HostOverview) error {
	type appJSON struct {
		App        string `json:"app"`
		Recorded   string `json:"recorded"`
		Running    int    `json:"running"`
		Health     string `json:"health"`
		Proxied    bool   `json:"proxied"`
		State      string `json:"state"`
		Incomplete bool   `json:"incomplete"`
	}
	out := struct {
		Proxy struct {
			Managed bool   `json:"managed"`
			Running bool   `json:"running"`
			Health  string `json:"health"`
		} `json:"proxy"`
		Apps    []appJSON `json:"apps"`
		Foreign int       `json:"foreign"`
	}{}
	out.Apps = []appJSON{} // encode an empty host as [] rather than null
	out.Proxy.Managed, out.Proxy.Running, out.Proxy.Health = ov.Proxy.Managed, ov.Proxy.Running, ov.Proxy.Health
	for _, r := range ov.Apps { // already alphabetical
		out.Apps = append(out.Apps, appJSON{r.App, r.Recorded, r.Running, r.Health, r.Proxied, r.StateKey(), r.Incomplete})
	}
	out.Foreign = ov.Foreign
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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
	staging, err := os.MkdirTemp("", "ob-"+cfg.App)
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

func runDeploy(cmd *cobra.Command, g *globalFlags, planFile string, rollback, yes, redeploy bool) error {
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
		e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
		if err != nil {
			return err
		}
		defer cleanup()
		if rollback {
			err := e.Rollback(ctx)
			notifyOutcome(cfg, g, "rollback", "", err)
			return err
		}
		if err := cfg.RunPreflight(filepath.Dir(g.ConfigPath)); err != nil {
			return err
		}
		err = applyPlan(cmd, g, cfg, p, e, planFile)
		notifyOutcome(cfg, g, "deploy", "", err)
		return err
	}

	// Interactive deploy: show the plan, confirm, then apply the exact bytes we
	// just staged — one command in place of plan → eyeball → deploy --plan.
	pl, err := buildPlan(cmd, g)
	if err != nil {
		return err
	}
	defer pl.cleanup()
	if pl.noop && !redeploy {
		pl.e.Opts.UI.Successf("nothing to deploy — %s is current (`--redeploy` forces a fresh roll)", pl.art.HostState.CurrentRelease)
		return nil
	}
	if !yes && !confirm(cmd, "\nApply this plan?") {
		fmt.Fprintln(cmd.OutOrStdout(), "not applied")
		return nil
	}
	err = pl.e.Deploy(ctx, pl.art.ID, pl.staging)
	notifyOutcome(pl.cfg, g, "deploy", pl.art.ID, err)
	return err
}

// confirm reads a yes/no answer; anything but y/yes (incl. EOF on a
// non-interactive stdin) is No — deploys never proceed on ambiguity.
func confirm(cmd *cobra.Command, prompt string) bool {
	u := ui.New(cmd.OutOrStdout(), false)
	fmt.Fprintf(cmd.OutOrStdout(), "%s ", u.Bold(prompt+" [y/N]"))
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
