package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/notify"
	"github.com/labstack/onebox/internal/onebox"
	"github.com/labstack/onebox/internal/transport"
	"github.com/labstack/onebox/internal/ui"
)

// loadAll reads the project, resolves it for the selected environment, and
// parses the runtime it generates. There is no user-authored Compose file to
// find: the declaration is the source, and the author states what would
// otherwise have to be inferred.
func loadAll(ctx context.Context, g *globalFlags) (*app.Resolved, *ctypes.Project, error) {
	return loadAllWith(ctx, g, false)
}

// loadAllLenient is for read-only verbs — status, logs, exec, audit, proxy
// apply. None of them run anything, so a workload whose image nobody has built
// yet must not block the query; it renders with a placeholder and the caller
// can say so.
func loadAllLenient(ctx context.Context, g *globalFlags) (*app.Resolved, *ctypes.Project, error) {
	return loadAllWith(ctx, g, true)
}

func loadAllWith(ctx context.Context, g *globalFlags, lenient bool) (*app.Resolved, *ctypes.Project, error) {
	spec, err := app.Load(g.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := spec.Resolve(g.Env)
	if err != nil {
		return nil, nil, err
	}
	var rendered *app.Rendered
	if lenient {
		rendered, err = resolved.RenderForInspection(g.Env, nil)
	} else {
		rendered, err = resolved.Render(g.Env, "", nil)
	}
	if err != nil {
		return nil, nil, err
	}
	interpolation, err := resolved.Spec.InterpolationEnv()
	if err != nil {
		return nil, nil, err
	}
	p, err := compose.LoadBytes(ctx, rendered.Bytes, resolved.NamesFor(g.Env).ComposeProject(), spec.Dir, interpolation)
	if err != nil {
		var interpolation *compose.InterpolationError
		if errors.As(err, &interpolation) {
			if hidden := resolved.Spec.EncryptedDocumentEntries(); len(hidden) > 0 {
				return nil, nil, fmt.Errorf("%w\n  the encrypted %s may supply it, and this command decrypts nothing — plan the deploy, which decrypts as it stages",
					err, strings.Join(hidden, ", "))
			}
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("the generated runtime did not parse as Compose — this is an Onebox bug: %w", err)
	}
	return resolved, p, nil
}

func addCommands(root *cobra.Command, g *globalFlags) {
	root.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "validate schema, workloads, and rollability — no side effects",
		Long:  "Load the project, expand shorthand, apply defaults and the environment's\noverrides, and check every rule the contract states.\n\nContacts nothing and writes nothing. A failure names the field, the line and\nthe constraint; `ob canonical` shows what was understood.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadAll(cmd.Context(), g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{
					"app": cfg.Name, "environment": g.Env,
					"workloads": cfg.ReleaseOrder(), "jobs": cfg.JobOrder(), "services": cfg.ServiceNames(),
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %s (%s, %s)\n",
				countLabel(len(cfg.ReleaseOrder()), "workload"),
				countLabel(len(cfg.JobOrder()), "job"),
				countLabel(len(cfg.ServiceNames()), "supporting service"))
			return nil
		},
	})

	var planOut, planBackupReportOut string
	// Shared by plan and deploy: both need to know what a build produced.
	var imageArgs []string
	resolveImages := func() error {
		images, err := parseImages(imageArgs)
		if err != nil {
			return err
		}
		g.Images = images
		return nil
	}

	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "refresh → rendered diff + pinned images + command list → plan artifact",
		Long:  "Read the target's current state, render the runtime, pin every image to a\ndigest, and write a plan artifact.\n\nReads the target but changes nothing there. The plan binds the configuration,\nthe rendered runtime, the host state and the pinned images, and expires after\n15 minutes — so a tag that moves afterwards cannot change what is deployed.\nApprove it with `ob approve --plan`, apply it with `ob deploy --plan`.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := resolveImages(); err != nil {
				return err
			}
			return runPlan(cmd, g, planOut, planBackupReportOut)
		},
	}
	planCmd.Flags().StringVarP(&planOut, "out", "o", "ob-plan.json", "plan artifact path")
	planCmd.Flags().StringVar(&planBackupReportOut, "backup-report-out", "", "write a plan-bound backup report template when migration protection is required")
	planCmd.Flags().StringArrayVar(&imageArgs, "image", nil, "resolved image as workload=reference, for build-sourced workloads (repeatable)")
	root.AddCommand(planCmd)

	var approvePlanFile, approveBackupReportFile, approveOut string
	approveCmd := &cobra.Command{
		Use:   "approve",
		Short: "record a local human confirmation for one exact executable plan",
		Long:  "Record a short-lived local confirmation bound to one exact plan and, when supplied, one exact backup report.\n\nPrompts for confirmation, because approving is a human act: a routine plan\nasks yes or no, and one that touches data asks for the release identifier to\nbe typed back. There is no flag to skip it. The artifact is tamper-evident but\nis not an authenticated identity-provider signature. Contacts nothing.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runApprove(cmd, g, approvePlanFile, approveBackupReportFile, approveOut)
		},
	}
	approveCmd.Flags().StringVar(&approvePlanFile, "plan", "", "executable plan artifact to approve")
	approveCmd.Flags().StringVar(&approveBackupReportFile, "backup-report", "", "backup report to bind into this local confirmation")
	approveCmd.Flags().StringVarP(&approveOut, "out", "o", "ob-approval.json", "local confirmation artifact path")
	_ = approveCmd.MarkFlagRequired("plan")
	root.AddCommand(approveCmd)

	var planFile, approvalFile, backupReportFile, migrationBackupOverrideReason string
	var deployYes, deployRedeploy, deployBreakLock bool
	deployCmd := &cobra.Command{
		Use:   "deploy",
		Short: "show the plan, confirm, and release with health-gated zero downtime",
		Long:  "Release: run pre-release jobs, replace workloads behind their health checks,\nverify, and move the current symlink.\n\nThis is the only way an application reaches a host. It takes the deploy lock,\nfences any older runner, journals every phase, drains connections before\nstopping a container, and can roll back. Without --plan it plans inline and\nasks for confirmation; with --plan it applies exactly what was reviewed.\n\nIf it is interrupted, `ob resume` finishes it and `ob abort` reverts it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := resolveImages(); err != nil {
				return err
			}
			return runDeploy(cmd, g, planFile, approvalFile, backupReportFile, migrationBackupOverrideReason, deployYes, deployRedeploy, deployBreakLock)
		},
	}
	deployCmd.Flags().StringVar(&planFile, "plan", "", "apply a saved plan artifact (binds config + host state; local confirmation is separate)")
	deployCmd.Flags().StringVar(&approvalFile, "approval", "", "apply a plan-bound local confirmation artifact")
	deployCmd.Flags().StringVar(&backupReportFile, "backup-report", "", "apply the backup report bound into the local confirmation")
	deployCmd.Flags().StringVar(&migrationBackupOverrideReason, "override-migration-backup", "", "audited break-glass reason for proceeding without a required backup report (requires --approval)")
	deployCmd.Flags().StringArrayVar(&imageArgs, "image", nil, "resolved image as workload=reference, for build-sourced workloads (repeatable)")
	deployCmd.Flags().BoolVarP(&deployYes, "yes", "y", false, "skip the confirmation prompt")
	deployCmd.Flags().BoolVar(&deployRedeploy, "redeploy", false, "deploy even when nothing changed (fresh roll of identical content)")
	deployCmd.Flags().BoolVar(&g.NoRollback, "no-rollback", false, "verify failures halt; never auto-rollback")
	deployCmd.Flags().BoolVar(&deployBreakLock, "break-lock", false, "break a stale deploy lock after inspecting its holder")
	root.AddCommand(deployCmd)

	var bootstrapBreakLock bool
	bootstrapCmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "first contact: host setup, registry login, and supporting/data services",
		Long:  "Prepare a host: install what the deploy needs, create the layout, log in to\nregistries, start the proxy, and start supporting services.\n\nRun once per host before the first deploy. It is safe to run again — each\nstep converges rather than repeats. It does not release the application;\n`ob deploy` does that.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindBootstrap, BreakLock: bootstrapBreakLock,
			}, "bootstrap")
		},
	}
	// without this, a bootstrap killed while holding the app/host lock can
	// only be retried after the full lock TTL: each retry mints a fresh
	// deploy id, so the same-deploy reclaim path never fires
	bootstrapCmd.Flags().BoolVar(&bootstrapBreakLock, "break-lock", false, "break a stale bootstrap lock after inspecting its holder")
	root.AddCommand(bootstrapCmd)

	root.AddCommand(&cobra.Command{
		Use:   "rollback",
		Short: "re-release the previous release dir (pinned local image)",
		Long:  "Re-activate the previous release from the directory still on the host.\n\nNothing is pulled and nothing is rebuilt: the previous release's images are\nalready there, which is what makes this fast and available when a registry is\nnot. Refused when there is no previous release, or when its snapshot is\nunavailable or unusable. Supporting services are not rolled back, so a\nmigration a job already applied stays applied — moving the symlink does\nnot undo it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindRollback}, "rollback")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "resume",
		Short: "continue an interrupted deploy from the journal (fences the old runner)",
		Long:  "Continue a deploy that was interrupted, from the journal.\n\nFences the runner that stopped so it cannot wake up and act on stale state,\nthen carries on from the last completed phase. Use when the interruption was\nthe runner's — a lost connection, a killed process — rather than the\nrelease's.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindResume}, "resume")
		},
	})

	var abortBreakLock, abortBreakMigrationGate bool
	abortCmd := &cobra.Command{
		Use:   "abort",
		Short: "revert an interrupted deploy to the previous release (migration-gated)",
		Long:  "Revert an interrupted deploy to the release that was serving before it.\n\nGated on what the interrupted deploy already did: a migration whose effect\ncannot be reversed by re-activating a directory refuses, because reverting\nthe containers would leave them running against data they do not match.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindAbort, BreakLock: abortBreakLock, BreakMigrationGate: abortBreakMigrationGate}, "abort")
		},
	}
	abortCmd.Flags().BoolVar(&abortBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	abortCmd.Flags().BoolVar(&abortBreakMigrationGate, "break-migration-gate", false, "abort past a closed migration gate (you assert schema compatibility)")
	root.AddCommand(abortCmd)

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "recorded versus actual state per workload and service",
		Long:  "Compare what the host records against what it is actually running, per\nworkload and service.\n\nReads only. Reports the recorded release, each container's release label and\nhealth, replica shortfalls, the proxy, and any incomplete deploy. Exits\nnon-zero on divergence so a script can branch on it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, g)
		},
	})

	var auditN int
	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "who deployed what, when, from which SHA — incl. failed runs",
		Long:  "Who did what, when, and from which revision — including runs whose terminal\nis long gone.\n\nReads the append-only journals on the host. One row per invocation, so a\nrollback appears as its own event rather than hiding inside the release it\nrestored.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			defer cleanup()
			if isStructuredOutput(g) {
				records, err := e.AuditSnapshot(cmd.Context(), auditN)
				if err != nil {
					return writeStructuredReadFailure(cmd, g, err)
				}
				return writeFiniteSuccess(cmd, g, map[string]any{"records": records})
			}
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
	cfg  *app.Resolved
	svc  *onebox.Service
	plan onebox.DeployPlan
	ui   *ui.UI
}

// notifyOutcome pushes a mutating verb's outcome to the configured webhook.
// Fail-open: a webhook problem is a stderr warning, never the verb's result.
func notifyOutcome(cfg *app.Resolved, g *globalFlags, verb, deployID string, err error) {
	if cfg == nil || len(cfg.Notifications) == 0 {
		return
	}
	host := ""
	if env, eerr := cfg.Environment(g.Env); eerr == nil {
		host = env.Destination()
	}
	p := notify.Payload{
		App: cfg.Name, Env: g.Env, Host: host, Verb: verb, DeployID: deployID,
		Status: "ok", Operator: journal.DefaultOperator(),
	}
	if err != nil {
		// Remote stderr and provider responses can contain credentials. Detailed
		// failures stay on the trusted local stderr path; webhooks receive only a
		// stable, redaction-safe outcome.
		p.Status, p.Error = "fail", "operation failed; inspect trusted local diagnostics and journal evidence"
	}
	for _, name := range sortedNames(cfg.Notifications) {
		if nerr := notify.Send(cfg.Notifications[name], p); nerr != nil {
			fmt.Fprintf(os.Stderr, "warn: notify webhook %s: %v\n", name, nerr)
		}
	}
}

// notifyOperationOutcome maps the canonical result to the stable webhook fields.
// A no-op is deliberately silent: reporting the planned release ID as deployed
// would claim an activation that never happened.
func notifyOperationOutcome(cfg *app.Resolved, g *globalFlags, verb string, result onebox.OperationResult, err error) {
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
	if !isStructuredOutput(g) {
		renderDeployPlan(cmd, u, plan)
	}
	return &preparedPlan{cfg: cfg, svc: svc, plan: plan, ui: u}, nil
}

func renderDeployPlan(cmd *cobra.Command, u *ui.UI, plan onebox.DeployPlan) {
	out := cmd.OutOrStdout()
	u.Header("plan " + plan.Operation.ReleaseID)
	u.Println(u.Dim("planner: " + formatRunnerProvenance(plan.Runner)))
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
		if step.ResultPolicy != "" {
			detail += "  result_policy=" + string(step.ResultPolicy)
		}
		if step.Strategy != "" {
			detail += "  strategy=" + step.Strategy
		}
		if detail != "" {
			detail = "  " + detail
		}
		u.Println("  " + step.ID + detail)
	}
	if plan.MigrationBackup != nil {
		u.Println(fmt.Sprintf("  migration_backup=maximum_age:%s restore_test:%t resources:%d keys:%d",
			plan.MigrationBackup.MaximumAge, plan.MigrationBackup.RequireRestoreTest,
			len(plan.MigrationBackup.Resources), len(plan.MigrationBackup.RequiredKeyMaterial)))
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
func runPlan(cmd *cobra.Command, g *globalFlags, outPath, backupReportOut string) error {
	preexisting := artifactExists(outPath)
	pl, err := buildPlan(cmd, g)
	if err != nil {
		return writeStructuredCommandFailure(cmd, g, "plan_failed", "plan failed; inspect stderr for local diagnostics", err)
	}
	// The plan is written before the template is attempted. A caller cannot know
	// ahead of time whether a plan will require a backup report, so passing
	// --backup-report-out defensively must not cost the plan artifact on every
	// deploy that touches no data.
	if err := pl.plan.Save(outPath); err != nil {
		return writeStructuredCommandFailure(cmd, g, "plan_failed", "plan failed; inspect stderr for local diagnostics", err)
	}
	var reportTemplate *onebox.BackupReport
	backupReportRequired := false
	if backupReportOut != "" {
		template, templateErr := onebox.NewBackupReportTemplate(&pl.plan)
		switch {
		case templateErr == nil:
			reportTemplate = &template
			backupReportRequired = true
		case errors.Is(templateErr, onebox.ErrBackupReportNotRequired):
			// Not a failure: this plan declares no migration backup requirement,
			// so there is no template to write. The plan stands, and the caller
			// is told plainly rather than by the absence of a file.
		default:
			if cleanupErr := removeArtifactWeCreated(outPath, preexisting); cleanupErr != nil {
				templateErr = errors.Join(templateErr, fmt.Errorf("remove incomplete plan artifact set: %w", cleanupErr))
			}
			return writeStructuredCommandFailure(cmd, g, "artifact_write_failed", "plan artifacts could not be written", templateErr)
		}
	}
	if reportTemplate != nil {
		if err := reportTemplate.SaveTemplate(backupReportOut); err != nil {
			if cleanupErr := removeArtifactWeCreated(outPath, preexisting); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove incomplete plan artifact set: %w", cleanupErr))
			}
			return writeStructuredCommandFailure(cmd, g, "artifact_write_failed", "plan artifacts could not be written", err)
		}
	}
	if isStructuredOutput(g) {
		data := map[string]any{"plan": pl.plan, "artifact_path": outPath}
		if backupReportOut != "" {
			// Always present when the flag was passed, so a caller branches on a
			// field rather than on whether a key exists.
			data["backup_report_required"] = backupReportRequired
		}
		if reportTemplate != nil {
			data["backup_report"] = reportTemplate
			data["backup_report_path"] = backupReportOut
		}
		return writeFiniteSuccess(cmd, g, data)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nplan written to %s\n", outPath)
	switch {
	case reportTemplate != nil:
		fmt.Fprintf(cmd.OutOrStdout(), "backup report template written to %s\n", backupReportOut)
	case backupReportOut != "":
		fmt.Fprintln(cmd.OutOrStdout(), "no migration backup report is required for this plan")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "approve with: ob approve --plan %s", outPath)
	if reportTemplate != nil {
		fmt.Fprintf(cmd.OutOrStdout(), " --backup-report %s", backupReportOut)
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

// connect builds the SSH-backed engine for the selected environment.
// newUI builds the one UI instance a command shares across the connect
// spinner, narrative, and command log — one stream, one line discipline.
func newUI(cmd *cobra.Command, g *globalFlags) *ui.UI {
	return ui.New(commandOutput(cmd, g), g.Verbose && !isStructuredOutput(g))
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
		Images:        g.Images,
		EngineOptions: engine.Options{Verbose: g.Verbose, UI: u, Out: commandOutput(cmd, g)},
	})
}

// notificationConfig loads only enough to know where to report an outcome. It
// deliberately does not render: a verb whose failure was the rendering must
// still be able to say so.
func notificationConfig(g *globalFlags) (*app.Resolved, error) {
	spec, err := app.Load(g.ConfigPath)
	if err != nil {
		return nil, err
	}
	return spec.Resolve(g.Env)
}

// sortedNames orders map keys so repeated runs report in the same sequence.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func runMutation(cmd *cobra.Command, g *globalFlags, request onebox.ExecuteRequest, verb string) error {
	cfg, err := notificationConfig(g)
	if err != nil {
		return writeEarlyOperationFailure(cmd, g, err)
	}
	var structured *cliOperationOutput
	if isStructuredOutput(g) {
		structured = newCLIOperationOutput(cmd, g)
		previousSink := request.Events
		request.Events = func(event onebox.OperationEvent) {
			if previousSink != nil {
				previousSink(event)
			}
			structured.event(event)
		}
	}
	result, err := operationsService(cmd, g).Execute(cmd.Context(), request)
	notifyOperationOutcome(cfg, g, verb, result, err)
	if structured != nil {
		if outputErr := structured.finish(&result, err); outputErr != nil {
			if err == nil {
				return outputErr
			}
			exitCode := 1
			if errors.Is(err, context.Canceled) {
				exitCode = 2
			}
			return errors.Join(withExitCode(err, exitCode), fmt.Errorf("write structured operation result: %w", outputErr))
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return withExitCode(err, 2)
			}
			return withExitCode(err, 1)
		}
	}
	return err
}

func runStatus(cmd *cobra.Command, g *globalFlags) error {
	cfg, p, err := loadAllLenient(cmd.Context(), g)
	if err != nil {
		return writeStatusFailure(cmd, g, err)
	}
	e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
	if err != nil {
		return writeStatusFailure(cmd, g, err)
	}
	defer cleanup()
	if !isStructuredOutput(g) {
		return e.Status(cmd.Context())
	}
	snapshot, err := e.StatusSnapshot(cmd.Context())
	if err != nil {
		return writeStatusFailure(cmd, g, err)
	}
	snapshot = safeStatusSnapshot(snapshot)
	if snapshot.Diverged {
		statusErr := errors.New("status: divergence detected")
		publicErr := &cliPublicError{
			Code: "divergence_detected", SafeMessage: "recorded and observed state diverge",
			DiagnosticCommand: "ob audit --output json", Details: map[string]any{"status": snapshot},
		}
		if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); err != nil {
			return err
		}
		return withExitCode(statusErr, 1)
	}
	return writeFiniteSuccess(cmd, g, map[string]any{"status": snapshot})
}

func writeStatusFailure(cmd *cobra.Command, g *globalFlags, statusErr error) error {
	if !isStructuredOutput(g) {
		return statusErr
	}
	if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil,
		publicError(statusErr, "status_failed", "status failed; inspect stderr for local diagnostics")); err != nil {
		return err
	}
	return withExitCode(statusErr, 1)
}

func connect(cmd *cobra.Command, g *globalFlags, cfg *app.Resolved, p *ctypes.Project, u *ui.UI) (*engine.Engine, func(), error) {
	env, err := cfg.Environment(g.Env)
	if err != nil {
		return nil, nil, err
	}
	t, err := cliConnector(cmd.Context(), env.Destination())
	if err != nil {
		return nil, nil, err
	}
	attachTransportLogger(t, u.Cmd)
	cfgBytes, _ := os.ReadFile(g.ConfigPath)
	e := engine.New(cfg, p, t, engine.Options{
		Verbose:    g.Verbose,
		UI:         u,
		Out:        commandOutput(cmd, g),
		LocalDir:   filepath.Dir(g.ConfigPath),
		NoRollback: g.NoRollback,
		ForceLock:  false,
		GitSHA:     gitShortSHA(filepath.Dir(g.ConfigPath)),
		ConfigHash: engine.HashBytes(cfgBytes),
		Runner:     onebox.CurrentRunnerProvenance(),
	})
	return e, func() { _ = t.Close() }, nil
}

func runDeploy(cmd *cobra.Command, g *globalFlags, planFile, approvalFile, backupReportFile, migrationBackupOverrideReason string, yes, redeploy, breakLock bool) error {
	if backupReportFile != "" && migrationBackupOverrideReason != "" {
		return writeEarlyOperationFailure(cmd, g, fmt.Errorf("--backup-report and --override-migration-backup are mutually exclusive"))
	}
	if planFile == "" && (backupReportFile != "" || migrationBackupOverrideReason != "") {
		return writeEarlyOperationFailure(cmd, g, fmt.Errorf("backup reports and migration overrides require --plan"))
	}
	if planFile == "" && approvalFile != "" {
		return writeEarlyOperationFailure(cmd, g, codedError("plan_required", "--approval requires --plan so the exact approved artifact is applied"))
	}
	if migrationBackupOverrideReason != "" && approvalFile == "" {
		return writeEarlyOperationFailure(cmd, g, fmt.Errorf("--override-migration-backup requires a plan-bound local-confirmation --approval file created with the strong ceremony"))
	}
	if isStructuredOutput(g) && planFile == "" {
		return writeEarlyOperationFailure(cmd, g, codedError("plan_required", "structured deploy requires --plan; run `ob plan --output %s` first", g.Output))
	}
	if planFile != "" {
		plan, err := onebox.LoadDeployPlan(planFile)
		if err != nil {
			return writeEarlyOperationFailure(cmd, g, err)
		}
		backupReport, err := loadBackupReport(backupReportFile)
		if err != nil {
			return writeEarlyOperationFailure(cmd, g, err)
		}
		approval, err := loadApproval(approvalFile)
		if err != nil {
			return writeEarlyOperationFailure(cmd, g, err)
		}
		needsMutation := !plan.NoOp || redeploy
		if isStructuredOutput(g) && approval == nil && needsMutation && plan.Operation.Approval != onebox.ApprovalNone {
			return writeEarlyOperationFailure(cmd, g, fmt.Errorf(
				"structured deploy requires an explicit plan-bound local-confirmation artifact through --approval; run `ob approve --plan %s` first",
				planFile,
			))
		}
		if approval == nil && needsMutation && plan.Operation.Approval != onebox.ApprovalNone && !yes {
			renderApprovalSummary(cmd, plan)
			if !confirmPlanApproval(cmd, plan) {
				fmt.Fprintln(cmd.OutOrStdout(), "not approved")
				return writeCancelled(cmd, g, "deploy approval cancelled")
			}
			grant, err := onebox.NewApprovalGrant(plan, backupReport, journal.DefaultOperator(), time.Now())
			if err != nil {
				return err
			}
			approval = &grant
		}
		backupOverride, err := loadMigrationBackupOverride(plan, approval, migrationBackupOverrideReason, time.Now())
		if err != nil {
			return writeEarlyOperationFailure(cmd, g, err)
		}
		if !isStructuredOutput(g) {
			fmt.Fprintf(cmd.OutOrStdout(), "→ runner %s; applying plan %s (bound; drift will be rechecked)\n",
				formatRunnerProvenance(onebox.CurrentRunnerProvenance()), plan.Operation.ReleaseID)
		}
		return runMutation(cmd, g, onebox.ExecuteRequest{
			Kind: onebox.KindDeploy, Plan: plan, Approval: approval, BreakLock: breakLock,
			NoRollback: g.NoRollback, Redeploy: redeploy,
			BackupReport: backupReport, MigrationBackupOverride: backupOverride,
		}, "deploy")
	}

	// Interactive deploy: show the canonical plan, confirm, then execute it
	// through the same binding checks as the saved-plan flow.
	pl, err := buildPlan(cmd, g)
	if err != nil {
		return err
	}
	plannedNoOp := pl.plan.NoOp && !redeploy
	if !plannedNoOp && pl.plan.MigrationBackup != nil {
		return fmt.Errorf("migration backup policy requires a saved plan: run `ob plan --backup-report-out ob-backup-report.json`, fill the report, then confirm and deploy it with --backup-report (or use the audited override flags)")
	}
	approval, err := loadApproval(approvalFile)
	if err != nil {
		return err
	}
	if !plannedNoOp && approval == nil && !yes {
		if !confirmInteractiveDeploy(cmd, &pl.plan) {
			fmt.Fprintln(cmd.OutOrStdout(), "not applied")
			return writeCancelled(cmd, g, "deploy cancelled")
		}
		if pl.plan.Operation.Approval != onebox.ApprovalNone {
			grant, err := onebox.NewApprovalGrant(&pl.plan, nil, journal.DefaultOperator(), time.Now())
			if err != nil {
				return err
			}
			approval = &grant
		}
	}
	result, err := pl.svc.Execute(cmd.Context(), onebox.ExecuteRequest{
		Kind: onebox.KindDeploy, Plan: &pl.plan, Approval: approval, BreakLock: breakLock,
		NoRollback: g.NoRollback, Redeploy: redeploy,
	})
	if err == nil && result.NoOp {
		pl.ui.Successf("nothing to deploy — %s is current (`--redeploy` forces a fresh roll)", pl.plan.Artifact.HostState.CurrentRelease)
		return nil
	}
	notifyOperationOutcome(pl.cfg, g, "deploy", result, err)
	return err
}

func loadMigrationBackupOverride(
	plan *onebox.DeployPlan,
	approval *onebox.ApprovalGrant,
	overrideReason string,
	now time.Time,
) (*onebox.MigrationBackupOverride, error) {
	if overrideReason == "" {
		return nil, nil
	}
	if approval == nil {
		return nil, errors.New("migration backup override requires a strong plan-bound local confirmation")
	}
	override, err := onebox.NewMigrationBackupOverride(plan, journal.DefaultOperator(), overrideReason, now)
	if err != nil {
		return nil, err
	}
	return &override, nil
}

func loadBackupReport(path string) (*onebox.BackupReport, error) {
	if path == "" {
		return nil, nil
	}
	return onebox.LoadBackupReport(path)
}

func loadApproval(path string) (*onebox.ApprovalGrant, error) {
	if path == "" {
		return nil, nil
	}
	return onebox.LoadApprovalGrant(path)
}

func runApprove(cmd *cobra.Command, g *globalFlags, planPath, backupReportPath, outPath string) error {
	plan, err := onebox.LoadExecutablePlan(planPath)
	if err != nil {
		return writeStructuredCommandFailure(cmd, g, "approval_failed", "approval could not be created", err)
	}
	backupReport, err := loadBackupReport(backupReportPath)
	if err != nil {
		return writeStructuredCommandFailure(cmd, g, "confirmation_failed", "local confirmation could not be created", err)
	}
	if backupReport != nil {
		if err := backupReport.ValidateForPlan(plan, time.Now()); err != nil {
			return writeStructuredCommandFailure(cmd, g, "confirmation_failed", "local confirmation could not be created", err)
		}
	}
	promptOut := cmd.OutOrStdout()
	if isStructuredOutput(g) {
		promptOut = cmd.ErrOrStderr()
	}
	renderApprovalSummaryTo(promptOut, plan)
	if !confirmPlanApprovalAt(cmd, plan, promptOut) {
		if !isStructuredOutput(g) {
			fmt.Fprintln(cmd.OutOrStdout(), "not approved")
		}
		return writeCancelled(cmd, g, "approval cancelled")
	}
	approval, err := onebox.NewApprovalGrant(plan, backupReport, journal.DefaultOperator(), time.Now())
	if err != nil {
		return writeStructuredCommandFailure(cmd, g, "approval_failed", "approval could not be created", err)
	}
	if err := approval.Save(outPath); err != nil {
		return writeStructuredCommandFailure(cmd, g, "approval_failed", "approval could not be created", err)
	}
	apply := "ob deploy"
	if plan.ExecutableOperation().Kind == onebox.KindJobRun {
		apply = "ob job run"
	}
	if isStructuredOutput(g) {
		applyCommand := fmt.Sprintf("%s --plan %s --approval %s", apply, planPath, outPath)
		if backupReportPath != "" {
			applyCommand += " --backup-report " + backupReportPath
		}
		return writeFiniteSuccess(cmd, g, map[string]any{
			"approval": approval, "artifact_path": outPath,
			"apply_command": applyCommand,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "local confirmation written to %s — apply with: %s --plan %s --approval %s", outPath, apply, planPath, outPath)
	if backupReportPath != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " --backup-report %s", backupReportPath)
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

func renderApprovalSummary(cmd *cobra.Command, plan onebox.ExecutablePlan) {
	renderApprovalSummaryTo(cmd.OutOrStdout(), plan)
}

func renderApprovalSummaryTo(out io.Writer, plan onebox.ExecutablePlan) {
	operation := plan.ExecutableOperation()
	binding := operation.Binding
	fmt.Fprintf(out, "\nApprove exact plan:\n")
	fmt.Fprintf(out, "  operation: %s\n", operation.Kind)
	fmt.Fprintf(out, "  release: %s\n", operation.ReleaseID)
	fmt.Fprintf(out, "  digest:  %s\n", plan.ExecutablePlanDigest())
	fmt.Fprintf(out, "  target:  %s (%s/%s)\n", binding.Server, binding.Application, binding.Environment)
	fmt.Fprintf(out, "  risk:    %s (%s)\n", operation.Risk, operation.Approval)
	fmt.Fprintf(out, "  expires: %s\n", operation.ExpiresAt)
}

func confirmPlanApproval(cmd *cobra.Command, plan onebox.ExecutablePlan) bool {
	return confirmPlanApprovalAt(cmd, plan, cmd.OutOrStdout())
}

func confirmPlanApprovalAt(cmd *cobra.Command, plan onebox.ExecutablePlan, out io.Writer) bool {
	operation := plan.ExecutableOperation()
	if operation.Approval == onebox.ApprovalNone {
		return true
	}
	if operation.Approval != onebox.ApprovalStrong && operation.Approval != onebox.ApprovalBreakGlass {
		return confirmAt(cmd, out, "Approve this exact plan?")
	}
	want := operation.ReleaseID
	fmt.Fprintf(out, "Type release ID %s to approve: ", want)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	return strings.TrimSpace(line) == want
}

func confirmInteractiveDeploy(cmd *cobra.Command, plan *onebox.DeployPlan) bool {
	if plan.Operation.Approval == onebox.ApprovalNone {
		return confirm(cmd, "\nApply this plan?")
	}
	return confirmPlanApproval(cmd, plan)
}

// confirm reads a yes/no answer; anything but y/yes (incl. EOF on a
// non-interactive stdin) is No — deploys never proceed on ambiguity.
func confirm(cmd *cobra.Command, prompt string) bool {
	return confirmAt(cmd, cmd.OutOrStdout(), prompt)
}

func confirmAt(cmd *cobra.Command, out io.Writer, prompt string) bool {
	u := ui.New(out, false)
	fmt.Fprintf(out, "%s ", u.Bold(prompt+" [y/N]"))
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
