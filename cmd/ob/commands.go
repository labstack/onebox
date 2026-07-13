package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	ctypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/notify"
	"github.com/labstack/onebox/internal/onebox"
	"github.com/labstack/onebox/internal/proxy"
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
		Short: "validate schema, components, and rollability — no side effects",
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
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %s (%s, %s, %s)\n",
				countLabel(len(cfg.Components), "component"), countLabel(len(cfg.Roles), "workload"),
				countLabel(len(cfg.Jobs), "job"), countLabel(len(cfg.Accessories), "supporting/data service"))
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
				network := cfg.Proxy.Network
				if network == "" {
					network = proxy.DefaultNetwork
				}
				compose.InjectProxyNetwork(p, cfg, network)
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
			return runDeploy(cmd, g, planFile, deployYes, deployRedeploy)
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
		Short: "first contact: host setup, registry login, and supporting/data services",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := operationsService(cmd, g).Execute(cmd.Context(), onebox.ExecuteRequest{
				Kind: onebox.KindBootstrap, Force: g.Force,
			})
			return err
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
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindRollback}, "rollback")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "resume",
		Short: "continue an interrupted deploy from the journal (fences the old runner)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindResume}, "resume")
		},
	})

	abortCmd := &cobra.Command{
		Use:   "abort",
		Short: "revert an interrupted deploy to the previous release (migration-gated)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindAbort, Force: g.Force}, "abort")
		},
	}
	abortCmd.Flags().BoolVar(&g.Force, "force", false, "abort past a closed migration gate (you assert schema compatibility)")
	root.AddCommand(abortCmd)

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "recorded versus actual state per workload and service",
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

// preparedPlan is the CLI projection of the canonical onebox plan. The service
// owns host observation, image pinning, staging, binding, and execution; the
// adapter only renders the result and asks for confirmation.
type preparedPlan struct {
	cfg  *config.Config
	svc  *onebox.Service
	plan onebox.DeployPlan
	ui   *ui.UI
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

// notifyOperationOutcome maps the canonical result to legacy webhook fields.
// A no-op is deliberately silent: reporting the planned release ID as deployed
// would claim an activation that never happened.
func notifyOperationOutcome(cfg *config.Config, g *globalFlags, verb string, result onebox.OperationResult, err error) {
	if err == nil && result.NoOp {
		return
	}
	evidenceID := result.EvidenceID
	if evidenceID == "" {
		evidenceID = result.ReleaseID
	}
	notifyOutcome(cfg, g, verb, evidenceID, err)
}

// buildPlan asks the canonical service for an executable graph, then renders
// its redaction-safe CLI projection. It mutates nothing on the host.
func buildPlan(cmd *cobra.Command, g *globalFlags) (*preparedPlan, error) {
	u := newUI(cmd, g)
	busy, stopBusy := u.Busy("building executable plan")
	busy("observing host, pinning images, and staging release")
	svc := operationsServiceWithUI(cmd, g, u)
	plan, err := svc.PlanDeploy(cmd.Context(), onebox.PlanDeployRequest{})
	stopBusy()
	if err != nil {
		return nil, err
	}
	cfg, err := notificationConfig(g)
	if err != nil {
		return nil, err
	}
	renderDeployPlan(cmd, u, plan)
	return &preparedPlan{cfg: cfg, svc: svc, plan: plan, ui: u}, nil
}

func renderDeployPlan(cmd *cobra.Command, u *ui.UI, plan onebox.DeployPlan) {
	out := cmd.OutOrStdout()
	u.Header("plan " + plan.Operation.ReleaseID)
	for _, line := range strings.Split(engine.FidelityContract, "\n") {
		u.Println(u.Dim(line))
	}
	fmt.Fprintln(out)
	if plan.Diff != "" {
		u.Diff(plan.Diff)
		fmt.Fprintln(out)
	} else if plan.NoOp {
		u.Infof("no changes — compose (release labels aside) and payload are byte-identical to live")
	} else {
		u.Infof("compose unchanged (release labels aside) — but the payload differs (env files / secrets); deploying ships it")
	}

	u.Println(u.Bold("operation:"))
	u.Println(fmt.Sprintf("  risk=%s  reversibility=%s  approval=%s",
		plan.Operation.Risk, plan.Operation.Reversibility, plan.Operation.Approval))
	for _, step := range plan.Operation.Steps {
		detail := step.Component
		if step.Service != "" && step.Service != step.Component {
			detail += " → " + step.Service
		}
		if step.DataEffect != onebox.DataEffectNone {
			detail += "  data_effect=" + string(step.DataEffect)
		}
		if step.Strategy != "" {
			detail += "  strategy=" + step.Strategy
		}
		if detail != "" {
			detail = "  " + detail
		}
		u.Println("  " + step.ID + detail)
	}
	fmt.Fprintln(out)

	u.Println(u.Bold("images:"))
	services := make([]string, 0, len(plan.Artifact.PinnedImages))
	for service := range plan.Artifact.PinnedImages {
		services = append(services, service)
	}
	sort.Strings(services)
	for _, service := range services {
		ref := plan.Artifact.PinnedImages[service]
		mark := u.OK("[pinned]")
		if !strings.Contains(ref, "@sha256:") {
			mark = u.Warn("[TAG-BOUND (unpinned)]")
		}
		name, digest := ref, ""
		if i := strings.Index(ref, "@"); i >= 0 {
			name, digest = ref[:i], ref[i:]
		}
		u.Println(fmt.Sprintf("  %-12s %s%s  %s", service, name, u.Dim(digest), mark))
	}
	fmt.Fprintln(out)

	u.Println(u.Bold("commands:"))
	for _, command := range plan.Artifact.Commands {
		switch {
		case strings.HasPrefix(command, " "):
			u.Println("  " + u.Dim(command))
		case strings.HasSuffix(command, ":"):
			u.Println("  " + u.Bold(command))
		default:
			if i := strings.Index(command, "): "); i >= 0 {
				u.Println("  " + u.Bold(command[:i+2]) + " " + u.Dim(command[i+3:]))
			} else {
				u.Println("  " + command)
			}
		}
	}
}

// runPlan writes the plan artifact for the CI/review flow.
func runPlan(cmd *cobra.Command, g *globalFlags, outPath string) error {
	pl, err := buildPlan(cmd, g)
	if err != nil {
		return err
	}
	if err := pl.plan.Save(outPath); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nplan written to %s — apply with: ob deploy --plan %s\n", outPath, outPath)
	return nil
}

// connect builds the SSH-backed engine for the selected environment.
// newUI builds the one UI instance a command shares across the connect
// spinner, narrative, and command log — one stream, one line discipline.
func newUI(cmd *cobra.Command, g *globalFlags) *ui.UI {
	return ui.New(cmd.OutOrStdout(), g.Verbose)
}

// cliConnector is replaceable by in-package tests and honors OB_LOCAL for the
// existing local-docker workflow. Production uses cancellable SSH dialing.
var cliConnector onebox.Connector = func(ctx context.Context, target string) (transport.Transport, error) {
	if value := strings.TrimSpace(strings.ToLower(os.Getenv("OB_LOCAL"))); value == "1" || value == "true" {
		return transport.NewLocal(), nil
	}
	return transport.NewSSHContext(ctx, target)
}

func attachTransportLogger(t transport.Transport, logger func(string, string)) {
	switch typed := t.(type) {
	case *transport.SSH:
		typed.Logger = logger
	case *transport.Local:
		typed.Logger = logger
	}
}

func operationsService(cmd *cobra.Command, g *globalFlags) *onebox.Service {
	return operationsServiceWithUI(cmd, g, newUI(cmd, g))
}

func operationsServiceWithUI(cmd *cobra.Command, g *globalFlags, u *ui.UI) *onebox.Service {
	connector := func(ctx context.Context, target string) (transport.Transport, error) {
		t, err := cliConnector(ctx, target)
		if err == nil {
			attachTransportLogger(t, u.Cmd)
		}
		return t, err
	}
	return onebox.New(onebox.Options{
		ConfigPath: g.ConfigPath, Environment: g.Env, Connect: connector,
		EngineOptions: engine.Options{Verbose: g.Verbose, UI: u, Out: cmd.OutOrStdout()},
	})
}

func notificationConfig(g *globalFlags) (*config.Config, error) {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return nil, err
	}
	if cfg.App == "" {
		cfg.App = config.DefaultApp(g.ConfigPath)
	}
	return cfg, nil
}

func runMutation(cmd *cobra.Command, g *globalFlags, request onebox.ExecuteRequest, verb string) error {
	cfg, err := notificationConfig(g)
	if err != nil {
		return err
	}
	result, err := operationsService(cmd, g).Execute(cmd.Context(), request)
	notifyOperationOutcome(cfg, g, verb, result, err)
	return err
}

func connect(cmd *cobra.Command, g *globalFlags, cfg *config.Config, p *ctypes.Project, u *ui.UI) (*engine.Engine, func(), error) {
	env, err := cfg.Environment(g.Env)
	if err != nil {
		return nil, nil, err
	}
	t, err := cliConnector(cmd.Context(), env.Hosts[0])
	if err != nil {
		return nil, nil, err
	}
	attachTransportLogger(t, u.Cmd)
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
	return e, func() { _ = t.Close() }, nil
}

func runDeploy(cmd *cobra.Command, g *globalFlags, planFile string, yes, redeploy bool) error {
	if planFile != "" {
		plan, err := onebox.LoadDeployPlan(planFile)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "→ applying plan %s (bound; drift will be rechecked)\n", plan.Operation.ReleaseID)
		return runMutation(cmd, g, onebox.ExecuteRequest{
			Kind: onebox.KindDeploy, Plan: plan, Force: g.Force,
			NoRollback: g.NoRollback, Redeploy: redeploy,
		}, "deploy")
	}

	// Interactive deploy: show the canonical plan, confirm, then execute it
	// through the same binding checks as the saved-plan flow.
	pl, err := buildPlan(cmd, g)
	if err != nil {
		return err
	}
	plannedNoOp := pl.plan.NoOp && !redeploy
	if !plannedNoOp && !yes && !confirm(cmd, "\nApply this plan?") {
		fmt.Fprintln(cmd.OutOrStdout(), "not applied")
		return nil
	}
	result, err := pl.svc.Execute(cmd.Context(), onebox.ExecuteRequest{
		Kind: onebox.KindDeploy, Plan: &pl.plan, Force: g.Force,
		NoRollback: g.NoRollback, Redeploy: redeploy,
	})
	if err == nil && result.NoOp {
		pl.ui.Successf("nothing to deploy — %s is current (`--redeploy` forces a fresh roll)", pl.plan.Artifact.HostState.CurrentRelease)
		return nil
	}
	notifyOperationOutcome(pl.cfg, g, "deploy", result, err)
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

func gitShortSHA(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short=7", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
