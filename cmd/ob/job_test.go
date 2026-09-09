package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/onebox"
)

func cliJobPlan(t *testing.T) onebox.JobPlan {
	t.Helper()
	return cliJobPlanWith(t, onebox.DataEffectNone, onebox.RiskModerate, onebox.ApprovalOneTime)
}

func cliJobPlanWith(t *testing.T, effect onebox.DataEffectClass, risk onebox.RiskClass, approval onebox.ApprovalClass) onebox.JobPlan {
	t.Helper()
	// A destructive plan only seals as critical, irreversible and strongly
	// approved, so the caller picks the effect and the rest follows it.
	reversibility := onebox.ReversibilityReversible
	if effect == onebox.DataEffectDestructive {
		reversibility = onebox.ReversibilityIrreversible
	}
	now := time.Now().UTC()
	artifact := onebox.JobArtifact{
		Application: "demo", Environment: "production", Server: "deploy@example.invalid",
		CurrentRelease: "R0", RuntimeDigest: "sha256:" + strings.Repeat("ab", 32),
		Job: "maintenance", Image: "ghcr.io/example/maintenance@sha256:" + strings.Repeat("cd", 32),
		DataEffect: effect,
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	operation := onebox.OperationPlan{
		SchemaVersion: onebox.OperationPlanSchemaVersion,
		ID:            "job-operation", Kind: onebox.KindJobRun, ReleaseID: artifact.CurrentRelease,
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano),
		Risk: risk, Reversibility: reversibility, Approval: approval,
		Binding: onebox.OperationBinding{
			Application: artifact.Application, Environment: artifact.Environment, Server: artifact.Server,
			ConfigDigest: "sha256:config", ComposeDigest: artifact.RuntimeDigest, StateDigest: engine.HashBytes(encoded),
		},
		Steps: []onebox.OperationStep{{
			ID: "job:maintenance", Kind: onebox.StepJob, Component: "maintenance", Service: "maintenance",
			DataEffect: effect, Mutation: true,
		}},
	}
	if err := operation.Seal(); err != nil {
		t.Fatal(err)
	}
	plan := onebox.JobPlan{
		SchemaVersion: onebox.ExecutableJobPlanSchemaVersion,
		Runner:        onebox.CurrentRunnerProvenance(), Operation: operation, Artifact: artifact,
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestStructuredJobRunRequiresSavedPlan(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"job", "run", "maintenance", "--output", "json"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires --plan") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(out.String(), "Approve exact plan") {
		t.Fatalf("structured output prompted interactively: %s", out.String())
	}
	var envelope cliEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil || envelope.Error == nil {
		t.Fatalf("structured job failure = %+v, decode=%v\n%s", envelope, decodeErr, out.String())
	}
}

func TestStructuredJobRunRequiresOutOfBandApproval(t *testing.T) {
	plan := cliJobPlan(t)
	path := filepath.Join(t.TempDir(), "job-plan.json")
	if err := plan.Save(path); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"job", "run", "--plan", path, "--output", "json"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires --approval") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(out.String(), "Approve exact plan") || strings.Contains(out.String(), "not approved") {
		t.Fatalf("structured output prompted interactively: %s", out.String())
	}
}

func TestApproveAndHumanCancellationAcceptJobPlan(t *testing.T) {
	plan := cliJobPlan(t)
	dir := t.TempDir()
	planPath := filepath.Join(dir, "job-plan.json")
	approvalPath := filepath.Join(dir, "approval.json")
	if err := plan.Save(planPath); err != nil {
		t.Fatal(err)
	}

	var approveOut bytes.Buffer
	approve := newRootCmd()
	approve.SetOut(&approveOut)
	approve.SetIn(strings.NewReader("yes\n"))
	approve.SetArgs([]string{"approve", "--plan", planPath, "--out", approvalPath})
	if err := approve.Execute(); err != nil {
		t.Fatalf("approve job plan: %v\n%s", err, approveOut.String())
	}
	grant, err := onebox.LoadApprovalGrant(approvalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := grant.ValidateForPlan(&plan, time.Now()); err != nil {
		t.Fatalf("job approval does not bind plan: %v", err)
	}
	if !strings.Contains(approveOut.String(), "ob job run --plan") {
		t.Fatalf("approval printed wrong apply command: %s", approveOut.String())
	}

	var cancelOut bytes.Buffer
	cancel := newRootCmd()
	cancel.SetOut(&cancelOut)
	cancel.SetIn(strings.NewReader("no\n"))
	cancel.SetArgs([]string{"job", "run", "--plan", planPath})
	if err := cancel.Execute(); err == nil {
		t.Fatal("cancel job run returned success")
	} else {
		var coded interface{ ExitCode() int }
		if !errors.As(err, &coded) || coded.ExitCode() != 2 {
			t.Fatalf("cancel job run = %v, want exit 2", err)
		}
	}
	if !strings.Contains(cancelOut.String(), "not approved") {
		t.Fatalf("human cancellation was not reported: %s", cancelOut.String())
	}
}

func TestStrongJobApprovalAsksForTheJobName(t *testing.T) {
	plan := cliJobPlanWith(t, onebox.DataEffectDestructive, onebox.RiskCritical, onebox.ApprovalStrong)
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.json")
	if err := plan.Save(planPath); err != nil {
		t.Fatal(err)
	}

	// The release ID is what the prompt used to ask for. It identifies the
	// deploy this job runs inside, which every job planned against that
	// release shares, so it must no longer be accepted.
	var refusedOut bytes.Buffer
	refused := newRootCmd()
	refused.SetOut(&refusedOut)
	refused.SetErr(&refusedOut)
	refused.SetIn(strings.NewReader(plan.Operation.ReleaseID + "\n"))
	refused.SetArgs([]string{"approve", "--plan", planPath, "--out", filepath.Join(dir, "refused.json")})
	if err := refused.Execute(); err == nil {
		t.Fatalf("release ID approved a job run: %s", refusedOut.String())
	}
	if !strings.Contains(refusedOut.String(), "not approved") {
		t.Fatalf("expected refusal: %s", refusedOut.String())
	}
	if !strings.Contains(refusedOut.String(), "Type job name maintenance to approve:") {
		t.Fatalf("prompt did not ask for the job name: %s", refusedOut.String())
	}
	// The summary has to name the job: this path prints no plan block, so
	// without it nothing on screen says what is about to run.
	if !strings.Contains(refusedOut.String(), "job:       maintenance (destructive)") {
		t.Fatalf("summary did not name the job: %s", refusedOut.String())
	}

	approvalPath := filepath.Join(dir, "approval.json")
	var out bytes.Buffer
	approve := newRootCmd()
	approve.SetOut(&out)
	approve.SetIn(strings.NewReader("maintenance\n"))
	approve.SetArgs([]string{"approve", "--plan", planPath, "--out", approvalPath})
	if err := approve.Execute(); err != nil {
		t.Fatalf("job name did not approve: %v\n%s", err, out.String())
	}
	if _, err := onebox.LoadApprovalGrant(approvalPath); err != nil {
		t.Fatalf("load grant: %v", err)
	}
}

func TestApprovalTokenNamesWhatThePlanActsOn(t *testing.T) {
	job := cliJobPlanWith(t, onebox.DataEffectDestructive, onebox.RiskCritical, onebox.ApprovalStrong)
	if label, want := approvalToken(job.Operation); label != "job name" || want != "maintenance" {
		t.Fatalf("job token = %q %q, want \"job name\" \"maintenance\"", label, want)
	}
	// A deploy mints the release it names, so its token is unchanged.
	deploy := onebox.OperationPlan{Kind: onebox.KindDeploy, ReleaseID: "R7"}
	if label, want := approvalToken(deploy); label != "release ID" || want != "R7" {
		t.Fatalf("deploy token = %q %q, want \"release ID\" \"R7\"", label, want)
	}
}
