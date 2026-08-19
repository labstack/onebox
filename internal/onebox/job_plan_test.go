package onebox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/transport"
)

func writeManualJobProject(t *testing.T, effect string, requireBackup bool) string {
	t.Helper()
	path := writeServiceProject(t)
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	project := strings.Replace(string(encoded),
		"  database:\n",
		"  maintenance:\n    role: job\n    image: ghcr.io/example/maintenance:v1\n    when: manual\n    data_effect: "+effect+"\n  database:\n", 1)
	if requireBackup {
		project = strings.Replace(project,
			"      allow_agent_proposals: true\n",
			"      allow_agent_proposals: true\n      migrations: {require_backup: true, backup_maximum_age: 24h}\n", 1)
	}
	if err := os.WriteFile(path, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func manualJobRuntime(image string) string {
	return "services:\n  maintenance:\n    image: " + image + "\n"
}

func jobPlanFake(current *string, runtime string) *transport.Fake {
	fake := serviceFake()
	base := fake.Dynamic
	fake.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/" + *current + "\n"}, true
		case strings.Contains(cmd, "cat ") && strings.Contains(cmd, "compose.yaml"):
			return transport.Result{Stdout: runtime}, true
		}
		return base(cmd)
	}
	return fake
}

func newManualJobService(t *testing.T, effect string, requireBackup bool, fake *transport.Fake, now *time.Time, connects *int) *Service {
	t.Helper()
	return New(Options{
		ConfigPath: writeManualJobProject(t, effect, requireBackup),
		Now:        func() time.Time { return *now },
		Connect: func(_ context.Context, target string) (transport.Transport, error) {
			*connects++
			if target != "deploy@example.invalid" {
				t.Fatalf("connector target = %q", target)
			}
			return fake, nil
		},
	})
}

func TestPlanJobBindsCurrentReleaseRuntimeImageAndEffect(t *testing.T) {
	current := "R0"
	digest := strings.Repeat("ab", 32)
	runtime := manualJobRuntime("ghcr.io/example/maintenance@sha256:" + digest)
	fake := jobPlanFake(&current, runtime)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	connects := 0
	service := newManualJobService(t, "none", false, fake, &now, &connects)

	plan, err := service.PlanJob(context.Background(), PlanJobRequest{Job: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("validate plan: %v", err)
	}
	if plan.Operation.Kind != KindJobRun || plan.Operation.ReleaseID != current || plan.Artifact.CurrentRelease != current {
		t.Fatalf("job release binding = %+v", plan)
	}
	if plan.Artifact.RuntimeDigest != engine.HashBytes([]byte(runtime)) || plan.Artifact.RuntimeDigest != plan.Operation.Binding.ComposeDigest {
		t.Fatalf("runtime digest was not bound: %+v", plan.Artifact)
	}
	if plan.Artifact.Image != "ghcr.io/example/maintenance@sha256:"+digest || plan.Artifact.DataEffect != DataEffectNone {
		t.Fatalf("job execution inputs were not bound: %+v", plan.Artifact)
	}
	if plan.Operation.Approval != ApprovalOneTime || len(plan.Operation.Steps) != 1 {
		t.Fatalf("job authority = %+v", plan.Operation)
	}
	approval, err := NewApprovalGrant(&plan, nil, "operator@example.test", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("approve job plan: %v", err)
	}
	if err := approval.ValidateForPlan(&plan, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("validate job approval: %v", err)
	}
}

func TestPlanJobRejectsMutableCurrentImage(t *testing.T) {
	current := "R0"
	fake := jobPlanFake(&current, manualJobRuntime("ghcr.io/example/maintenance:latest"))
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	connects := 0
	service := newManualJobService(t, "none", false, fake, &now, &connects)
	_, err := service.PlanJob(context.Background(), PlanJobRequest{Job: "maintenance"})
	if err == nil || !strings.Contains(err.Error(), "not digest-pinned") || !strings.Contains(err.Error(), "ob deploy") {
		t.Fatalf("mutable current image was accepted: %v", err)
	}
}

func TestExecuteJobRejectsStaleReleaseBeforeContainerCreation(t *testing.T) {
	current := "R0"
	runtime := manualJobRuntime("ghcr.io/example/maintenance@sha256:" + strings.Repeat("ab", 32))
	fake := jobPlanFake(&current, runtime)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	connects := 0
	service := newManualJobService(t, "none", false, fake, &now, &connects)
	plan, err := service.PlanJob(context.Background(), PlanJobRequest{Job: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := NewApprovalGrant(&plan, nil, "operator@example.test", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	current = "R1"
	now = now.Add(2 * time.Minute)
	_, err = service.Execute(context.Background(), ExecuteRequest{Kind: KindJobRun, JobPlan: &plan, Approval: &approval})
	if err == nil || !strings.Contains(err.Error(), "job plan is stale") {
		t.Fatalf("stale job plan was accepted: %v", err)
	}
	if commands := strings.Join(fake.Commands, "\n"); strings.Contains(commands, "OB_RESULT_FILE") {
		t.Fatalf("stale job plan created a job container:\n%s", commands)
	}
}

func TestExecuteMigrationJobRequiresBackupReportBeforeReconnect(t *testing.T) {
	current := "R0"
	runtime := manualJobRuntime("ghcr.io/example/maintenance@sha256:" + strings.Repeat("ab", 32))
	fake := jobPlanFake(&current, runtime)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	connects := 0
	service := newManualJobService(t, "migration", true, fake, &now, &connects)
	plan, err := service.PlanJob(context.Background(), PlanJobRequest{Job: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Operation.Approval != ApprovalStrong || plan.MigrationBackup == nil {
		t.Fatalf("migration authority was weakened: %+v", plan)
	}
	approval, err := NewApprovalGrant(&plan, nil, "operator@example.test", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	connectsAfterPlan := connects
	now = now.Add(2 * time.Minute)
	_, err = service.Execute(context.Background(), ExecuteRequest{Kind: KindJobRun, JobPlan: &plan, Approval: &approval})
	if err == nil || !strings.Contains(err.Error(), "fresh backup report is required") {
		t.Fatalf("migration without a backup report was accepted: %v", err)
	}
	if connects != connectsAfterPlan {
		t.Fatalf("missing backup report reconnected: before=%d after=%d", connectsAfterPlan, connects)
	}
}

func TestExecuteJobRequiresPlanBoundApprovalBeforeReconnect(t *testing.T) {
	current := "R0"
	runtime := manualJobRuntime("ghcr.io/example/maintenance@sha256:" + strings.Repeat("ab", 32))
	fake := jobPlanFake(&current, runtime)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	connects := 0
	service := newManualJobService(t, "none", false, fake, &now, &connects)
	plan, err := service.PlanJob(context.Background(), PlanJobRequest{Job: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	connectsAfterPlan := connects
	now = now.Add(time.Minute)
	_, err = service.Execute(context.Background(), ExecuteRequest{Kind: KindJobRun, JobPlan: &plan})
	if err == nil || !strings.Contains(err.Error(), "approval is required") {
		t.Fatalf("job without approval was accepted: %v", err)
	}
	if connects != connectsAfterPlan {
		t.Fatalf("missing approval reconnected: before=%d after=%d", connectsAfterPlan, connects)
	}
}

func TestExecuteJobRejectsTamperedPlanBeforeReconnect(t *testing.T) {
	current := "R0"
	runtime := manualJobRuntime("ghcr.io/example/maintenance@sha256:" + strings.Repeat("ab", 32))
	fake := jobPlanFake(&current, runtime)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	connects := 0
	service := newManualJobService(t, "none", false, fake, &now, &connects)
	plan, err := service.PlanJob(context.Background(), PlanJobRequest{Job: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := NewApprovalGrant(&plan, nil, "operator@example.test", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	requirement := *backupEvidenceTestPlan(t, now).MigrationBackup
	resealOperation := func(t *testing.T, candidate *JobPlan) {
		t.Helper()
		if err := candidate.Operation.Seal(); err != nil {
			t.Fatalf("reseal operation fixture: %v", err)
		}
	}
	tests := []struct {
		name   string
		want   string
		mutate func(*testing.T, *JobPlan)
	}{
		{name: "job schema", want: "schema_version", mutate: func(_ *testing.T, p *JobPlan) { p.SchemaVersion = "future" }},
		{name: "runner version", want: "runner: version is required", mutate: func(_ *testing.T, p *JobPlan) { p.Runner.Version = "" }},
		{name: "runner schema", want: "does not declare support", mutate: func(_ *testing.T, p *JobPlan) { p.Runner.SupportedExecutablePlanSchemas = nil }},
		{name: "operation content", want: "operation:", mutate: func(_ *testing.T, p *JobPlan) { p.Operation.SchemaVersion = "future" }},
		{name: "operation digest", want: "operation: plan digest mismatch", mutate: func(_ *testing.T, p *JobPlan) { p.Operation.PlanDigest = "sha256:tampered" }},
		{name: "operation kind", want: "operation kind", mutate: func(t *testing.T, p *JobPlan) { p.Operation.Kind = KindDeploy; resealOperation(t, p) }},
		{name: "artifact authority", want: "artifact authority", mutate: func(_ *testing.T, p *JobPlan) { p.Artifact.Application = "other" }},
		{name: "current release", want: "current release", mutate: func(_ *testing.T, p *JobPlan) { p.Artifact.CurrentRelease = "R1" }},
		{name: "runtime digest", want: "runtime digest", mutate: func(_ *testing.T, p *JobPlan) { p.Artifact.RuntimeDigest = "sha256:" + strings.Repeat("cd", 32) }},
		{name: "mutable image", want: "digest-pinned image", mutate: func(_ *testing.T, p *JobPlan) { p.Artifact.Image = "ghcr.io/example/maintenance:latest" }},
		{name: "step count", want: "exactly one step", mutate: func(t *testing.T, p *JobPlan) {
			p.Operation.Steps = append(p.Operation.Steps, OperationStep{ID: "verify", Kind: StepVerify, DependsOn: []string{p.Operation.Steps[0].ID}, DataEffect: DataEffectNone})
			resealOperation(t, p)
		}},
		{name: "step binding", want: "step does not match", mutate: func(_ *testing.T, p *JobPlan) { p.Artifact.Job = "other" }},
		{name: "artifact state digest", want: "artifact digest", mutate: func(t *testing.T, p *JobPlan) {
			p.Operation.Binding.StateDigest = "sha256:" + strings.Repeat("ef", 32)
			resealOperation(t, p)
		}},
		{name: "backup on non-migration", want: "non-migration job", mutate: func(_ *testing.T, p *JobPlan) { p.MigrationBackup = &requirement }},
		{name: "outer plan digest", want: "executable job plan digest mismatch", mutate: func(_ *testing.T, p *JobPlan) { p.Runner.VCSRevision = "tampered" }},
	}
	connectsAfterPlan := connects
	now = now.Add(2 * time.Minute)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := plan
			candidate.Runner.SupportedExecutablePlanSchemas = append([]string(nil), plan.Runner.SupportedExecutablePlanSchemas...)
			candidate.Operation.Steps = append([]OperationStep(nil), plan.Operation.Steps...)
			test.mutate(t, &candidate)
			_, executeErr := service.Execute(context.Background(), ExecuteRequest{Kind: KindJobRun, JobPlan: &candidate, Approval: &approval})
			if executeErr == nil || !strings.Contains(executeErr.Error(), test.want) {
				t.Fatalf("tampered job plan error = %v, want %q", executeErr, test.want)
			}
			if connects != connectsAfterPlan {
				t.Fatalf("tampered plan reconnected: before=%d after=%d", connectsAfterPlan, connects)
			}
		})
	}
}

func TestExecuteJobRunsOnceAndJournalsTerminalResult(t *testing.T) {
	current := "R0"
	runtime := manualJobRuntime("ghcr.io/example/maintenance@sha256:" + strings.Repeat("ab", 32))
	fake := jobPlanFake(&current, runtime)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	connects := 0
	service := newManualJobService(t, "none", false, fake, &now, &connects)
	plan, err := service.PlanJob(context.Background(), PlanJobRequest{Job: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := NewApprovalGrant(&plan, nil, "operator@example.test", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	result, err := service.Execute(context.Background(), ExecuteRequest{Kind: KindJobRun, JobPlan: &plan, Approval: &approval})
	if err != nil {
		t.Fatalf("execute job: %v", err)
	}
	if result.Status != OperationStatusSuccess || result.ID != plan.Operation.ID || result.EvidenceID != plan.Operation.ID {
		t.Fatalf("job result = %+v", result)
	}
	commands := strings.Join(fake.Commands, "\n")
	if strings.Count(commands, "OB_RESULT_FILE=/run/onebox/job-result") != 1 {
		t.Fatalf("job did not run exactly once:\n%s", commands)
	}
	for _, want := range []string{`"operation_kind":"job_run"`, `"sub_step":"job:maintenance"`, `"event":"finish","status":"ok"`, approval.ApprovalDigest} {
		if !strings.Contains(commands, want) {
			t.Fatalf("job journal omitted %q:\n%s", want, commands)
		}
	}
}
