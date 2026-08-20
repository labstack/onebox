package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/onebox"
)

func addJobCommand(root *cobra.Command, g *globalFlags) {
	group := &cobra.Command{
		Use:   "job",
		Short: "plan and run a sealed one-shot manual job",
		Long:  "Plan and run one declared `when: manual` job against the current serving release.\n\nThe job remains in the release runtime but never runs during deploy. Saved plans\nbind its release, runtime digest, immutable image and data effect so agents can\nobtain a separate approval before execution.",
		Args:  cobra.NoArgs,
		RunE:  showCommandHelp,
	}

	var planOut, backupReportOut string
	plan := &cobra.Command{
		Use:   "plan <id>",
		Short: "seal a current-release-bound one-shot job plan",
		Long:  "Observe the current serving release and write a short-lived executable job plan.\n\nThe plan binds the exact runtime digest, digest-pinned image, job data effect,\ntarget and expiry. It reads the target and writes only the local plan artifact.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJobPlan(cmd, g, args[0], planOut, backupReportOut)
		},
	}
	plan.Flags().StringVarP(&planOut, "out", "o", "ob-job-plan.json", "job plan artifact path")
	plan.Flags().StringVar(&backupReportOut, "backup-report-out", "", "write a plan-bound backup report template when migration backup is required")

	var planPath, approvalPath, backupReportPath, overrideReason string
	var breakLock bool
	run := &cobra.Command{
		Use:   "run [id]",
		Short: "run one manual job from an inline or saved sealed plan",
		Long:  "Run one manual job through the canonical lock, fence, local-confirmation and journal boundary.\n\nHumans may pass an id and confirm interactively. Automation should supply a\nsaved --plan and its separately created local-confirmation artifact through\n--approval; migration plans may also require the exact plan-bound --backup-report.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := ""
			if len(args) == 1 {
				jobID = args[0]
			}
			return runJob(cmd, g, jobID, planPath, approvalPath, backupReportPath, overrideReason, breakLock)
		},
	}
	run.Flags().StringVar(&planPath, "plan", "", "apply a saved job plan artifact")
	run.Flags().StringVar(&approvalPath, "approval", "", "apply a plan-bound local confirmation artifact")
	run.Flags().StringVar(&backupReportPath, "backup-report", "", "apply the backup report bound into the local confirmation")
	run.Flags().StringVar(&overrideReason, "override-migration-backup", "", "audited break-glass reason (requires --approval)")
	run.Flags().BoolVar(&breakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")

	group.AddCommand(plan, run)
	root.AddCommand(group)
}

func runJobPlan(cmd *cobra.Command, g *globalFlags, jobID, outPath, backupReportOut string) error {
	plan, err := operationsService(cmd, g).PlanJob(cmd.Context(), onebox.PlanJobRequest{Job: jobID})
	if err != nil {
		return writeStructuredCommandFailure(cmd, g, "job_plan_failed", "job planning failed; inspect stderr for local diagnostics", err)
	}
	// Validated before anything is staged. Save() validates on its way to
	// writing, so without this a plan that is simply invalid would be
	// reported as artifact_write_failed — a write that never happened, and a
	// code whose guidance is blank.
	if err := plan.Validate(); err != nil {
		return writeStructuredCommandFailure(cmd, g, "job_plan_failed", "job planning failed; inspect stderr for local diagnostics", err)
	}
	// A job that declares no migration effect must not lose its plan because
	// the caller asked for a report template it turns out not to need — hence
	// the not-required case below is not a failure.
	var reportTemplate *onebox.BackupReport
	backupReportRequired := false
	if backupReportOut != "" {
		template, templateErr := onebox.NewBackupReportTemplate(&plan)
		switch {
		case templateErr == nil:
			reportTemplate = &template
			backupReportRequired = true
		case errors.Is(templateErr, onebox.ErrBackupReportNotRequired):
		default:
			// Built, not written: see the note on the same arm in plan. The
			// code stays job-scoped — plan_failed publishes `ob validate` as
			// its guidance, which is not the command that retries this.
			return writeStructuredCommandFailure(cmd, g, "job_plan_failed", "job planning failed; inspect stderr for local diagnostics", templateErr)
		}
	}
	// One destination cannot hold both. Staging gives them distinct temp
	// names, so the set would commit and the plan — renamed second — would
	// silently win, leaving the run reporting a backup_report_path that holds
	// a deploy plan.
	if backupReportOut != "" && sameArtifactPath(outPath, backupReportOut) {
		return writeStructuredCommandFailure(cmd, g, "artifact_write_failed",
			"job plan artifacts could not be written", fmt.Errorf("--out and --backup-report-out name the same path: %s", outPath))
	}
	// Staged and committed together, for the reason given in runPlan.
	var stagedReport stagedArtifact
	if reportTemplate != nil {
		var stageErr error
		stagedReport, stageErr = stageArtifact(backupReportOut, ".report", reportTemplate.SaveTemplate)
		if stageErr != nil {
			return writeStructuredCommandFailure(cmd, g, "artifact_write_failed", "job plan artifacts could not be written", stageErr)
		}
	}
	// artifact_write_failed, not job_plan_failed: staging is where the writes actually
	// happen — MkdirAll, CreateTemp, Write, Sync, Rename, directory fsync — so
	// ENOSPC and EACCES land here, and job_plan_failed publishes `ob validate`, which
	// would pass and leave the caller looping.
	stagedPlan, stageErr := stageArtifact(outPath, ".plan", plan.Save)
	if stageErr != nil {
		stagedReport.discard()
		return writeStructuredCommandFailure(cmd, g, "artifact_write_failed", "job plan artifacts could not be written", stageErr)
	}
	// Committed as a set: a failure part-way puts the tree back — what landed
	// is removed and whatever it replaced is restored from its backup — and
	// every staged and backup file is discarded on the way out. A caller told
	// the run failed finds neither a fresh artifact nor a missing one.
	// A commit failure is a rename or a directory sync — a write, not a
	// planning problem, so it must not report job_plan_failed.
	if err := commitArtifactSet(stagedReport, stagedPlan); err != nil {
		return writeStructuredCommandFailure(cmd, g, "artifact_write_failed", "job plan artifacts could not be written", err)
	}
	if isStructuredOutput(g) {
		data := map[string]any{"plan": plan, "artifact_path": outPath}
		if backupReportOut != "" {
			data["backup_report_required"] = backupReportRequired
		}
		if reportTemplate != nil {
			data["backup_report"] = reportTemplate
			data["backup_report_path"] = backupReportOut
		}
		return writeFiniteSuccess(cmd, g, data)
	}
	renderJobPlan(cmd, &plan)
	fmt.Fprintf(cmd.OutOrStdout(), "\nplan written to %s", outPath)
	switch {
	case reportTemplate != nil:
		fmt.Fprintf(cmd.OutOrStdout(), "; backup report template written to %s", backupReportOut)
	case backupReportOut != "":
		fmt.Fprint(cmd.OutOrStdout(), "; no migration backup report is required for this job")
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

func renderJobPlan(cmd *cobra.Command, plan *onebox.JobPlan) {
	operation := plan.Operation
	fmt.Fprintf(cmd.OutOrStdout(), "\nJob plan %s\n", operation.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "  job:      %s\n", plan.Artifact.Job)
	fmt.Fprintf(cmd.OutOrStdout(), "  release:  %s\n", plan.Artifact.CurrentRelease)
	fmt.Fprintf(cmd.OutOrStdout(), "  image:    %s\n", plan.Artifact.Image)
	fmt.Fprintf(cmd.OutOrStdout(), "  effect:   %s\n", plan.Artifact.DataEffect)
	fmt.Fprintf(cmd.OutOrStdout(), "  risk:     %s (%s)\n", operation.Risk, operation.Approval)
	fmt.Fprintf(cmd.OutOrStdout(), "  target:   %s\n", operation.Binding.Server)
	fmt.Fprintf(cmd.OutOrStdout(), "  expires:  %s\n", operation.ExpiresAt)
	if plan.MigrationBackup != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "  backup:   required (%d resources)\n", len(plan.MigrationBackup.Resources))
	}
}

func runJob(cmd *cobra.Command, g *globalFlags, jobID, planPath, approvalPath, backupReportPath, overrideReason string, breakLock bool) error {
	if planPath != "" && jobID != "" {
		return writeEarlyOperationFailure(cmd, g, errors.New("supply either a job id or --plan, not both"))
	}
	if planPath == "" && jobID == "" {
		return writeEarlyOperationFailure(cmd, g, errors.New("job run requires an id or --plan"))
	}
	if backupReportPath != "" && overrideReason != "" {
		return writeEarlyOperationFailure(cmd, g, errors.New("--backup-report and --override-migration-backup are mutually exclusive"))
	}
	if planPath == "" && (approvalPath != "" || backupReportPath != "" || overrideReason != "") {
		return writeEarlyOperationFailure(cmd, g, errors.New("approval, backup reports, and migration overrides require --plan"))
	}
	if overrideReason != "" && approvalPath == "" {
		return writeEarlyOperationFailure(cmd, g, errors.New("--override-migration-backup requires --approval"))
	}
	if isStructuredOutput(g) && planPath == "" {
		return writeEarlyOperationFailure(cmd, g, codedError("plan_required", "structured job run requires --plan; run `ob job plan %s --output %s` first", jobID, g.Output))
	}

	if planPath != "" {
		plan, err := onebox.LoadJobPlan(planPath)
		if err != nil {
			return writeEarlyOperationFailure(cmd, g, err)
		}
		backupReport, err := loadBackupReport(backupReportPath)
		if err != nil {
			return writeEarlyOperationFailure(cmd, g, err)
		}
		approval, err := loadApproval(approvalPath)
		if err != nil {
			return writeEarlyOperationFailure(cmd, g, err)
		}
		if approval == nil && plan.Operation.Approval != onebox.ApprovalNone {
			if isStructuredOutput(g) {
				return writeEarlyOperationFailure(cmd, g, fmt.Errorf("structured job run requires --approval; run `ob approve --plan %s` first", planPath))
			}
			renderApprovalSummary(cmd, plan)
			if !confirmPlanApproval(cmd, plan) {
				fmt.Fprintln(cmd.OutOrStdout(), "not approved")
				return writeCancelled(cmd, g, "job approval cancelled")
			}
			grant, err := onebox.NewApprovalGrant(plan, backupReport, journal.DefaultOperator(), time.Now())
			if err != nil {
				return err
			}
			approval = &grant
		}
		override, err := loadJobMigrationOverride(plan, approval, overrideReason)
		if err != nil {
			return writeEarlyOperationFailure(cmd, g, err)
		}
		return runMutation(cmd, g, onebox.ExecuteRequest{
			Kind: onebox.KindJobRun, JobPlan: plan, Approval: approval, BreakLock: breakLock,
			BackupReport: backupReport, MigrationBackupOverride: override,
		}, "job run")
	}

	plan, err := operationsService(cmd, g).PlanJob(cmd.Context(), onebox.PlanJobRequest{Job: jobID})
	if err != nil {
		return err
	}
	renderJobPlan(cmd, &plan)
	if plan.MigrationBackup != nil {
		return fmt.Errorf("job %s requires a plan-bound backup report; run `ob job plan %s --backup-report-out ob-backup-report.json`, fill and confirm it, then use `ob job run --plan`", jobID, jobID)
	}
	var approval *onebox.ApprovalGrant
	if plan.Operation.Approval != onebox.ApprovalNone {
		renderApprovalSummary(cmd, &plan)
		if !confirmPlanApproval(cmd, &plan) {
			fmt.Fprintln(cmd.OutOrStdout(), "not approved")
			return writeCancelled(cmd, g, "job approval cancelled")
		}
		grant, err := onebox.NewApprovalGrant(&plan, nil, journal.DefaultOperator(), time.Now())
		if err != nil {
			return err
		}
		approval = &grant
	}
	return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindJobRun, JobPlan: &plan, Approval: approval, BreakLock: breakLock}, "job run")
}

func loadJobMigrationOverride(
	plan *onebox.JobPlan,
	approval *onebox.ApprovalGrant,
	overrideReason string,
) (*onebox.MigrationBackupOverride, error) {
	if overrideReason == "" {
		return nil, nil
	}
	if approval == nil {
		return nil, errors.New("migration backup override requires a strong plan-bound local confirmation")
	}
	override, err := onebox.NewMigrationBackupOverride(plan, journal.DefaultOperator(), overrideReason, time.Now())
	if err != nil {
		return nil, err
	}
	return &override, nil
}
