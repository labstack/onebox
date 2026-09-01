package onebox

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/transport"

	"github.com/labstack/onebox/internal/app"
)

func sealedTestDeployPlan(t *testing.T, createdAt, expiresAt time.Time) DeployPlan {
	t.Helper()
	artifact := engine.Artifact{
		ID:         "R-test",
		App:        "demo",
		Env:        "production",
		CreatedAt:  createdAt,
		ConfigHash: "sha256:config",
		HostState: engine.HostState{
			Host:           "example.invalid",
			CurrentRelease: "R0",
			ImageIDs:       map[string]string{"server": "sha256:image"},
		},
		PinnedImages:    map[string]string{"server": "ghcr.io/example/app@sha256:digest"},
		RenderedCompose: "services: {}\n",
		Commands:        []string{"deploy release R-test"},
	}
	stateDigest, err := artifactDigest(artifact)
	if err != nil {
		t.Fatal(err)
	}
	operation := OperationPlan{
		SchemaVersion: OperationPlanSchemaVersion,
		ID:            "operation-test",
		Kind:          KindDeploy,
		ReleaseID:     artifact.ID,
		CreatedAt:     createdAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt:     expiresAt.UTC().Format(time.RFC3339Nano),
		Risk:          RiskModerate,
		Reversibility: ReversibilityReversible,
		Approval:      ApprovalOneTime,
		Binding: OperationBinding{
			Application:       artifact.App,
			Environment:       artifact.Env,
			Server:            "deploy@example.invalid",
			ConfigDigest:      artifact.ConfigHash,
			ComposeDigest:     "sha256:compose",
			StateDigest:       stateDigest,
			PayloadDigest:     "payload-digest",
			LiveComposeDigest: "sha256:live-compose",
			LivePayloadDigest: "live-payload",
		},
		Steps: []OperationStep{{
			ID:         "preflight",
			Kind:       StepPreflight,
			DataEffect: DataEffectNone,
		}},
	}
	if err := operation.Seal(); err != nil {
		t.Fatal(err)
	}
	plan := DeployPlan{
		SchemaVersion: ExecutableDeployPlanSchemaVersion,
		Runner:        CurrentRunnerProvenance(),
		Operation:     operation,
		Artifact:      artifact,
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestDeployPlanSaveLoadIsStrictAndProtected(t *testing.T) {
	base := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	plan := sealedTestDeployPlan(t, base, base.Add(15*time.Minute))
	path := filepath.Join(t.TempDir(), "nested", "deploy.json")
	if err := plan.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("plan mode = %04o, want 0600", got)
	}
	loaded, err := LoadDeployPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*loaded, plan) {
		t.Fatalf("loaded plan differs:\n got: %#v\nwant: %#v", *loaded, plan)
	}

	t.Run("unknown field", func(t *testing.T) {
		encoded, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		document["unrecognized_authority"] = true
		encoded, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		unknownPath := filepath.Join(t.TempDir(), "unknown.json")
		if err := os.WriteFile(unknownPath, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDeployPlan(unknownPath); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("unknown field was not rejected: %v", err)
		}
	})

	t.Run("tampered artifact", func(t *testing.T) {
		tampered := plan
		tampered.Artifact.Commands = append(tampered.Artifact.Commands, "unexpected mutation")
		encoded, err := json.Marshal(tampered)
		if err != nil {
			t.Fatal(err)
		}
		tamperedPath := filepath.Join(t.TempDir(), "tampered.json")
		if err := os.WriteFile(tamperedPath, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDeployPlan(tamperedPath); err == nil || !strings.Contains(err.Error(), "artifact digest") {
			t.Fatalf("tampered artifact was not rejected: %v", err)
		}
	})

	t.Run("tampered no-op decision", func(t *testing.T) {
		tampered := plan
		tampered.NoOp = !tampered.NoOp
		encoded, err := json.Marshal(tampered)
		if err != nil {
			t.Fatal(err)
		}
		tamperedPath := filepath.Join(t.TempDir(), "tampered-no-op.json")
		if err := os.WriteFile(tamperedPath, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDeployPlan(tamperedPath); err == nil || !strings.Contains(err.Error(), "executable deploy plan digest mismatch") {
			t.Fatalf("tampered no-op decision was not rejected: %v", err)
		}
	})
}

func TestLoadDeployPlanRejectsOversizedArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized-plan.json")
	if err := os.WriteFile(path, make([]byte, maxExecutableDeployPlanBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeployPlan(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized plan error = %v", err)
	}
}

func TestSchemaLessDeployPlansAreRejectedBeforeConnecting(t *testing.T) {
	base := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	valid := sealedTestDeployPlan(t, base, base.Add(15*time.Minute))

	tests := []struct {
		name          string
		schemaVersion string
		omitSchema    bool
		wantReason    string
	}{
		{
			name:       "missing schema",
			omitSchema: true,
			wantReason: "executable deploy plan has no schema_version",
		},
		{
			name:          "older schema",
			schemaVersion: "onebox.run/executable-deploy-plan/v1alpha1",
			wantReason:    `unsupported executable deploy plan schema "onebox.run/executable-deploy-plan/v1alpha1"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document, err := json.Marshal(valid)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(document, &fields); err != nil {
				t.Fatal(err)
			}
			if tt.omitSchema {
				delete(fields, "schema_version")
			} else {
				fields["schema_version"] = tt.schemaVersion
			}
			document, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "schema-less-plan.json")
			if err := os.WriteFile(path, document, 0o600); err != nil {
				t.Fatal(err)
			}

			_, err = LoadDeployPlan(path)
			assertSchemaLessPlanGuidance(t, err, tt.wantReason)

			plan := valid
			plan.SchemaVersion = tt.schemaVersion
			connected := false
			fake := serviceFake()
			svc := New(Options{
				ConfigPath: filepath.Join(t.TempDir(), "must-not-be-read.yml"),
				Now:        func() time.Time { return base },
				Connect: func(context.Context, transport.Route) (transport.Transport, error) {
					connected = true
					return fake, nil
				},
			})
			result, err := svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
			assertSchemaLessPlanGuidance(t, err, tt.wantReason)
			if connected {
				t.Fatal("schema-less plan connected to the target")
			}
			if len(fake.Commands) != 0 || len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
				t.Fatalf("schema-less plan reached a mutation path: commands=%v uploads=%v inputs=%v", fake.Commands, fake.Uploads, fake.Inputs)
			}
			if result.Status != OperationStatusError {
				t.Fatalf("schema-less plan result status = %q, want failed", result.Status)
			}
		})
	}
}

func assertSchemaLessPlanGuidance(t *testing.T, err error, wantReason string) {
	t.Helper()
	if err == nil {
		t.Fatal("schema-less plan was accepted")
	}
	for _, fragment := range []string{
		wantReason,
		ExecutableDeployPlanSchemaVersion,
		"upgrade `ob`",
		"`ob plan`",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("schema-less-plan error %q does not contain actionable guidance %q", err, fragment)
		}
	}
}

func TestExecuteRejectsExpiredDeployBeforeConnecting(t *testing.T) {
	now := time.Date(2026, 7, 12, 19, 0, 0, 0, time.UTC)
	plan := sealedTestDeployPlan(t, now.Add(-20*time.Minute), now.Add(-5*time.Minute))
	connected := false
	svc := New(Options{
		ConfigPath: filepath.Join(t.TempDir(), "must-not-be-read.yml"),
		Now:        func() time.Time { return now },
		Connect: func(context.Context, transport.Route) (transport.Transport, error) {
			connected = true
			return serviceFake(), nil
		},
	})
	var events []OperationEvent
	result, err := svc.Execute(context.Background(), ExecuteRequest{
		Kind: KindDeploy,
		Plan: &plan,
		Events: func(event OperationEvent) {
			events = append(events, event)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "deployment plan expired") {
		t.Fatalf("expired plan was not rejected: %v", err)
	}
	if connected {
		t.Fatal("expired plan connected to the target")
	}
	if result.Status != OperationStatusError || result.ID != plan.Operation.ID {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(events) != 2 {
		t.Fatalf("events = %#v, want started then failed", events)
	}
	if events[0].Sequence != 1 || events[0].Phase != "operation" || events[0].Status != OperationStatusRunning ||
		events[1].Sequence != 2 || events[1].Phase != "operation" || events[1].Status != OperationStatusError {
		t.Fatalf("unexpected event order: %#v", events)
	}
	for _, event := range events {
		if event.OperationID != plan.Operation.ID || event.Kind != KindDeploy || event.SchemaVersion != OperationEventSchemaVersion {
			t.Fatalf("event lost operation identity: %#v", event)
		}
	}
}

func TestExecuteRejectsFutureDeployBeforeConnecting(t *testing.T) {
	now := time.Date(2026, 7, 12, 19, 0, 0, 0, time.UTC)
	// Created 5 minutes ahead — beyond the 1-minute skew tolerance — but not yet
	// expired. A broken runner clock must not silently disable the expiry window.
	plan := sealedTestDeployPlan(t, now.Add(5*time.Minute), now.Add(20*time.Minute))
	connected := false
	svc := New(Options{
		ConfigPath: filepath.Join(t.TempDir(), "must-not-be-read.yml"),
		Now:        func() time.Time { return now },
		Connect: func(context.Context, transport.Route) (transport.Transport, error) {
			connected = true
			return serviceFake(), nil
		},
	})
	_, err := svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "created in the future") {
		t.Fatalf("future-dated plan was not rejected: %v", err)
	}
	if connected {
		t.Fatal("future-dated plan connected to the target")
	}
}

func TestExecuteRejectsResealedRiskMismatch(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The graph is untouched, so only the re-derived risk classification can catch
	// a plan whose sealed risk was downgraded — the check the graph shadows in
	// TestExecuteRejectsResealedGraphMismatch.
	plan.Operation.Risk = RiskLow
	if err := plan.Operation.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	fake.Uploads, fake.Inputs = nil, nil
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "risk classification differs") {
		t.Fatalf("resealed risk mismatch was not rejected: %v", err)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("risk mismatch reached a write-capable transport operation: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
}

func TestExecuteRejectsResealedApplicationMismatch(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// A plan sealed for a different application must never execute here even
	// though its graph matches — the highest-blast-radius cross-target mistake.
	plan.Artifact.App = "tenant-b"
	plan.Operation.Binding.Application = "tenant-b"
	stateDigest, err := artifactDigest(plan.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	plan.Operation.Binding.StateDigest = stateDigest
	if err := plan.Operation.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	fake.Uploads, fake.Inputs = nil, nil
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "plan application is") {
		t.Fatalf("resealed application mismatch was not rejected: %v", err)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("application mismatch reached a write-capable transport operation: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
}

func TestExecuteRejectsResealedEnvironmentMismatch(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// A plan sealed for a different environment must not execute against
	// production, even though the resolved app and graph match.
	plan.Artifact.Env = "staging"
	plan.Operation.Binding.Environment = "staging"
	stateDigest, err := artifactDigest(plan.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	plan.Operation.Binding.StateDigest = stateDigest
	if err := plan.Operation.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	fake.Uploads, fake.Inputs = nil, nil
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "plan environment is") {
		t.Fatalf("resealed environment mismatch was not rejected: %v", err)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("environment mismatch reached a write-capable transport operation: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
}

func TestExecuteRejectsConfigDriftBeforeMutation(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// A byte-level config edit the operator never planned. The parsed graph,
	// risk, app, and env are all identical, so only the config digest can catch
	// it (e.g. an environment-policy or migration-policy change).
	source, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.configPath, append(source, []byte("\n# drift introduced after planning\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.Uploads, fake.Inputs = nil, nil
	result, err := svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "configuration changed since plan") {
		t.Fatalf("config drift was not rejected: result=%#v err=%v", result, err)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("config drift reached a write-capable transport operation: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
}

func TestExecuteRejectsComposeDriftBeforeMutation(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// An edit to a referenced Compose file that leaves the service set — and
	// thus the operation graph — intact, but changes what would actually run.
	// Only the runtime digest binds it.
	//
	// The digest covers the generated runtime, not the file that fed it, so a
	// comment-only edit is deliberately not drift: nothing about the release
	// would differ, and forcing a re-plan for it would teach operators that
	// re-planning is noise.
	composePath := filepath.Join(filepath.Dir(svc.configPath), "docker-compose.yaml")
	source, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(string(source), "ghcr.io/example/postgres:", "ghcr.io/example/other:", 1)
	if err := os.WriteFile(composePath, []byte(drifted), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.Uploads, fake.Inputs = nil, nil
	result, err := svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "not the one the plan bound") {
		t.Fatalf("compose drift was not rejected: result=%#v err=%v", result, err)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("compose drift reached a write-capable transport operation: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
}

func TestExecuteRejectsStagedPayloadDriftBeforeMutation(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(svc.configPath)
	if err := os.WriteFile(filepath.Join(root, "app.env"), []byte("RUNTIME_SECRET=rotated-after-plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	approval := approvalForTestPlan(t, &plan)
	result, err := svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})
	if err == nil || (!strings.Contains(err.Error(), "release payload differs from the plan") && !strings.Contains(err.Error(), "rendered Compose differs from the plan")) {
		t.Fatalf("payload drift was not rejected: result=%#v err=%v", result, err)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("payload drift reached a write-capable transport operation: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
}

func TestExecuteRejectsResealedGraphMismatch(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	plan.Operation.Steps = plan.Operation.Steps[:1]
	plan.Operation.Risk = RiskLow
	plan.Operation.Reversibility = ReversibilityReversible
	plan.Operation.Approval = ApprovalNone
	if err := plan.Operation.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	fake.Commands = nil
	approval := approvalForTestPlan(t, &plan)
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})
	if err == nil || !strings.Contains(err.Error(), "operation graph differs") {
		t.Fatalf("resealed graph mismatch was not rejected: %v", err)
	}
	if len(fake.Commands) != 0 || len(fake.Uploads) != 0 {
		t.Fatalf("graph mismatch reached target: commands=%v uploads=%v", fake.Commands, fake.Uploads)
	}
}

func TestExecuteRejectsLiveBaselineDrift(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	baseDynamic := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "docker ps -q") && strings.Contains(command, "'database'") {
			return transport.Result{Stdout: "PG1\n"}, true
		}
		if strings.Contains(command, "docker inspect") && strings.Contains(command, "Health") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		if strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml") {
			return transport.Result{Stdout: "services:\n  web:\n    image: changed-after-plan\n"}, true
		}
		return baseDynamic(command)
	}
	fake.Commands = nil
	approval := approvalForTestPlan(t, &plan)
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})
	if err == nil || !strings.Contains(err.Error(), "live Compose changed since plan") {
		t.Fatalf("live baseline drift was not rejected: %v", err)
	}
	if len(fake.Uploads) != 0 {
		t.Fatalf("live drift reached upload: %v", fake.Uploads)
	}
}

func TestExecuteRechecksBindingAfterTakingFence(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	baseDynamic := fake.Dynamic
	liveReads := 0
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "docker ps -q") && strings.Contains(command, "'database'") {
			return transport.Result{Stdout: "PG1\n"}, true
		}
		if strings.Contains(command, "docker inspect") && strings.Contains(command, "Health") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		if strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml") {
			liveReads++
			if liveReads > 1 {
				return transport.Result{Stdout: "services:\n  server:\n    image: raced-after-initial-check\n"}, true
			}
		}
		return baseDynamic(command)
	}
	fake.Commands = nil
	approval := approvalForTestPlan(t, &plan)
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})
	if err == nil || !strings.Contains(err.Error(), "deploy precondition under lock: live Compose changed") {
		t.Fatalf("under-lock drift was not rejected: %v", err)
	}
	commands := strings.Join(fake.Commands, "\n")
	if !strings.Contains(commands, "/fence'") {
		t.Fatalf("test did not reach the fenced precondition:\n%s", commands)
	}
	if len(fake.Uploads) != 0 {
		t.Fatalf("under-lock drift reached transfer: %v", fake.Uploads)
	}
}

func TestOperationIDsAreKindQualifiedAndCollisionResistant(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	svc := New(Options{Entropy: bytes.NewReader(bytes.Repeat([]byte{7}, 12))})
	first := svc.newOperationID(now, "abcdef0", KindBootstrap)
	second := svc.newOperationID(now, "abcdef0", KindServiceApply)
	if first == second || !strings.Contains(first, "-bootstrap-") || !strings.Contains(second, "-service_apply-") {
		t.Fatalf("operation IDs are not disambiguated: %q %q", first, second)
	}
}

func TestExecuteNoOpEmitsOrderedEvidenceWithoutMutation(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	first, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	baseDynamic := fake.Dynamic
	redactedValue := regexp.MustCompile(`redacted:sha256:[0-9a-f]{12}`)
	liveCompose := redactedValue.ReplaceAllString(first.Artifact.RenderedCompose, testSecret)
	fake.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml"):
			return transport.Result{Stdout: liveCompose}, true
		case strings.Contains(command, "find . -type f"):
			return transport.Result{Stdout: first.Operation.Binding.PayloadDigest + "\n"}, true
		default:
			return baseDynamic(command)
		}
	}
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.NoOp {
		t.Fatalf("fixture did not produce a no-op plan: diff=%q", plan.Diff)
	}
	for _, step := range plan.Operation.Steps {
		if step.Kind == StepWorkloadRelease && (step.Action != step.Strategy || !step.Mutation || step.Reason != "redeploy_only") {
			t.Fatalf("no-op plan does not seal --redeploy as a fresh roll: %#v", step)
		}
	}
	var events []OperationEvent
	result, err := svc.Execute(context.Background(), ExecuteRequest{
		Kind: KindDeploy,
		Plan: &plan,
		Events: func(event OperationEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "no_op" || !result.NoOp {
		t.Fatalf("unexpected no-op result: %#v", result)
	}
	if result.EvidenceID != "" {
		t.Fatalf("no-op advertised nonexistent journal evidence: %#v", result)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("no-op reached a write-capable transport operation: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
	want := []string{
		"operation:running",
		"binding:running",
		"binding:running",
		"stage:running",
		"stage:running",
		"operation:no_op",
	}
	got := make([]string, len(events))
	for i, event := range events {
		got[i] = event.Phase + ":" + string(event.Status)
		if event.Sequence != i+1 {
			t.Fatalf("event %d sequence = %d", i, event.Sequence)
		}
		if event.EvidenceID != "" {
			t.Fatalf("no-op event advertised nonexistent journal evidence: %#v", event)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}

func TestExecuteRecoveryCorrelatesOperationWithJournalEvidence(t *testing.T) {
	fake := serviceFake()
	baseDynamic := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "for f in *.jsonl") {
			return transport.Result{Stdout: "@@ob-journal@@R-INCOMPLETE.jsonl\n" +
				`{"deploy_id":"R-INCOMPLETE","phase":"deploy","event":"start","ts":"2026-07-12T19:00:00Z"}` + "\n"}, true
		}
		return baseDynamic(command)
	}
	svc := newTestService(t, fake)
	var events []OperationEvent
	result, err := svc.Execute(context.Background(), ExecuteRequest{
		Kind: KindAbort,
		Events: func(event OperationEvent) {
			events = append(events, event)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "abort refused") {
		t.Fatalf("expected the safe abort refusal, got result=%#v err=%v", result, err)
	}
	if result.ID == "" || result.ID == "R-INCOMPLETE" || result.EvidenceID != "R-INCOMPLETE" {
		t.Fatalf("operation and journal identities were not separated: %#v", result)
	}
	last := events[len(events)-1]
	if last.OperationID != result.ID || last.EvidenceID != "R-INCOMPLETE" || last.Status != OperationStatusError {
		t.Fatalf("final event cannot correlate to journal evidence: %#v", last)
	}
}

func TestExecuteNoOpRechecksUnderLock(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	first, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	baseDynamic := fake.Dynamic
	redactedValue := regexp.MustCompile(`redacted:sha256:[0-9a-f]{12}`)
	liveCompose := redactedValue.ReplaceAllString(first.Artifact.RenderedCompose, testSecret)
	fake.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml"):
			return transport.Result{Stdout: liveCompose}, true
		case strings.Contains(command, "find . -type f"):
			return transport.Result{Stdout: first.Operation.Binding.PayloadDigest + "\n"}, true
		default:
			return baseDynamic(command)
		}
	}
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil || !plan.NoOp {
		t.Fatalf("create no-op plan: no_op=%v err=%v", plan.NoOp, err)
	}
	stableDynamic := fake.Dynamic
	liveReads := 0
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml") {
			liveReads++
			if liveReads > 1 {
				return transport.Result{Stdout: "services:\n  server:\n    image: raced-before-no-op\n"}, true
			}
		}
		return stableDynamic(command)
	}
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "deploy precondition under lock: live Compose changed") {
		t.Fatalf("no-op under-lock drift was not rejected: %v", err)
	}
	if len(fake.Uploads) != 0 {
		t.Fatalf("no-op drift reached transfer: %v", fake.Uploads)
	}
}

func TestExecuteRejectsChangedConfirmedBinding(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	binding, err := svc.ResolveExecutionBinding(context.Background(), KindDestroy)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(source), "server: deploy@example.invalid", "server: deploy@other.invalid", 1)
	if err := os.WriteFile(svc.configPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Execute(context.Background(), ExecuteRequest{
		Kind: KindDestroy, ExpectedBinding: &binding,
	})
	if err == nil || !strings.Contains(err.Error(), "execution binding changed after confirmation") {
		t.Fatalf("changed confirmed binding was not rejected: result=%#v err=%v", result, err)
	}
	if len(fake.Commands) != 0 || len(fake.Uploads) != 0 {
		t.Fatalf("binding mismatch reached target: commands=%v uploads=%v", fake.Commands, fake.Uploads)
	}
}

func TestExecuteEarlyFailureHasCorrelationIdentity(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	svc := New(Options{
		ConfigPath: filepath.Join(t.TempDir(), "missing.yml"),
		Now:        func() time.Time { return now },
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{3}, 6)),
	})
	var events []OperationEvent
	result, err := svc.Execute(context.Background(), ExecuteRequest{
		Kind: KindDestroy,
		Events: func(event OperationEvent) {
			events = append(events, event)
		},
	})
	if err == nil || result.ID == "" {
		t.Fatalf("early failure lacks identity: result=%#v err=%v", result, err)
	}
	if len(events) != 1 || events[0].OperationID != result.ID || events[0].Status != OperationStatusError {
		t.Fatalf("early failure event identity mismatch: result=%#v events=%#v", result, events)
	}
	if strings.Contains(events[0].Message, "missing.yml") {
		t.Fatalf("structured event leaked local diagnostics: %#v", events[0])
	}
}

func TestExecuteRequestRejectsIrrelevantAuthorityAndSafetyFields(t *testing.T) {
	deployPlan := &DeployPlan{}
	jobPlan := &JobPlan{}
	for _, test := range []struct {
		name    string
		request ExecuteRequest
	}{
		{name: "two plans", request: ExecuteRequest{Kind: KindDeploy, Plan: deployPlan, JobPlan: jobPlan}},
		{name: "job plan on deploy", request: ExecuteRequest{Kind: KindDeploy, JobPlan: jobPlan}},
		{name: "deploy plan on job", request: ExecuteRequest{Kind: KindJobRun, Plan: deployPlan}},
		{name: "plan on unplanned operation", request: ExecuteRequest{Kind: KindRollback, Plan: deployPlan}},
		{name: "mount authority on deploy", request: ExecuteRequest{Kind: KindDeploy, AllowDestructiveMounts: true}},
		{name: "migration gate on rollback", request: ExecuteRequest{Kind: KindRollback, BreakMigrationGate: true}},
		{name: "deploy controls on resume", request: ExecuteRequest{Kind: KindResume, NoRollback: true}},
		{name: "destroy controls on bootstrap", request: ExecuteRequest{Kind: KindBootstrap, RemoveVolumes: true}},
		{name: "approval on service apply", request: ExecuteRequest{Kind: KindServiceApply, Approval: &ApprovalGrant{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.request.Validate(); err == nil {
				t.Fatalf("invalid execution request was accepted: %+v", test.request)
			}
		})
	}
}

// A plan binds the runtime it would produce. Anything that feeds generation is
// therefore bound too, and each of these is a way the runtime could differ from
// what was reviewed while every other fact about the plan stayed identical.

// The base path decides the entire remote layout: where the release lands, where
// the journal is, where a service's credential lives.
func TestExecuteRejectsRelocatedBasePathBeforeMutation(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(source), "app: demo\n", "app: demo\nbase_path: /srv/onebox\n", 1)
	if err := os.WriteFile(svc.configPath, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.Uploads, fake.Inputs = nil, nil
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	// Refused by the configuration digest: the base path is part of the
	// project, and the runtime digest would catch it too.
	if err == nil || !strings.Contains(err.Error(), "configuration changed since plan") {
		t.Fatalf("a relocated base path must be refused: the plan bound a different layout: %v", err)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("a relocated base path reached a write: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
}

// A plan pins a digest, and execution deploys that digest. A tag that moves
// between planning and applying therefore changes nothing — which is stronger
// than refusing, and is the entire reason for pinning.
//
// The approval below is what makes this test about images: without it the run
// stops at the approval gate and proves nothing.
func TestAMovedTagCannotChangeWhatIsDeployed(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	planned := map[string]string{}
	for k, v := range plan.Artifact.PinnedImages {
		planned[k] = v
	}
	if len(planned) == 0 {
		t.Fatal("the plan pinned nothing; this proves nothing")
	}

	// The registry now answers with a different digest for the same tag.
	base := fake.Dynamic
	fake.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker buildx imagetools inspect") {
			return transport.Result{Stdout: "sha256:" + strings.Repeat("cd", 32) + "\n"}, true
		}
		return base(cmd)
	}

	approval := approvalForTestPlan(t, &plan)
	fake.Commands = nil
	_, _ = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})

	// Execution must not ask the registry again: the answer it would get is
	// not the one that was reviewed.
	for _, c := range fake.Commands {
		// The Buildx capability probe reads local `--help` only and contacts no
		// registry, so it is not a re-resolution.
		if strings.Contains(c, "imagetools inspect") && !strings.Contains(c, "--help") {
			t.Fatalf("execution re-resolved an image the plan had already pinned: %s", c)
		}
	}
	for name, digest := range plan.Artifact.PinnedImages {
		if digest != planned[name] {
			t.Errorf("%s: the plan's pin changed during execution: %q became %q", name, planned[name], digest)
		}
		if strings.Contains(digest, strings.Repeat("cd", 32)) {
			t.Errorf("%s: execution adopted the registry's new digest instead of the pinned one", name)
		}
	}
}

// A runner whose generation changed must not deploy a runtime nobody reviewed.
// The plan carries the digest of what it would produce; a binary that renders
// differently produces a different one.
func TestExecuteRejectsAGenerationChangeBeforeMutation(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)
	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Standing in for a generator change: the same project, rendering to
	// something else. Nothing else about the plan differs.
	plan.Operation.Binding.ComposeDigest = "sha256:" + strings.Repeat("ef", 32)
	if err := plan.Operation.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	fake.Uploads, fake.Inputs = nil, nil
	_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "not the one the plan bound") {
		t.Fatalf("a generation change must be refused, naming what to do: %v", err)
	}
	if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
		t.Fatalf("a generation change reached a write: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
	}
}

// A build-sourced workload can be released from a saved plan.
//
// Production never builds, so `build:` has no image until whatever built it
// says what it produced. The plan records that answer, and execution reloads
// with the plan's build images so the render resolves. Reloading without them
// would fail `ob deploy --plan` with image_unresolved unless --image were
// supplied a second time — which defeats the point of a plan being the thing
// reviewed.
func TestASavedPlanCarriesTheImageForABuiltWorkload(t *testing.T) {
	// Both forms of what a build can produce. The tagged one is the case that
	// matters: pinning turns it into a digest *after* the render, so a reload
	// that used the pin would bind a different document than the plan did. A
	// digest alone proves nothing about that.
	for _, built := range []string{
		"ghcr.io/example/app:ci-1234",
		"ghcr.io/example/app@sha256:" + strings.Repeat("ab", 32),
	} {
		t.Run(built, func(t *testing.T) { savedPlanCarriesImage(t, built) })
	}
}

func savedPlanCarriesImage(t *testing.T, built string) {
	fake := serviceFake()

	// The fixture has siblings — a referenced compose file among them — so the
	// whole directory travels, not just the project.
	origin := writeServiceProject(t)
	dir := t.TempDir()
	entries, err := os.ReadDir(filepath.Dir(origin))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(filepath.Dir(origin), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, filepath.Base(origin))
	source, err := os.ReadFile(origin)
	if err != nil {
		t.Fatal(err)
	}
	// Same project, but the web workload is built rather than pulled.
	from := "image: ghcr.io/example/app:v1"
	body := strings.Replace(string(source), from, "build: .", 1)
	if body == string(source) {
		t.Skipf("fixture no longer contains %q; update this test", from)
	}
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	tick := 0
	newSvc := func(images app.Images) *Service {
		return New(Options{
			ConfigPath: configPath,
			Images:     images,
			Now: func() time.Time {
				tick++
				return base.Add(time.Duration(tick) * time.Minute)
			},
			Connect: func(_ context.Context, _ transport.Route) (transport.Transport, error) { return fake, nil },
		})
	}

	// Planning supplies what the build produced, as CI would.
	plan, err := newSvc(app.Images{"web": built}).PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatalf("planning a build-sourced workload with --image must succeed: %v", err)
	}
	if plan.Artifact.PinnedImages["web"] == "" {
		t.Fatal("the plan recorded no image for the built workload; it cannot be released from")
	}

	// Applying the plan supplies nothing: the plan is the record.
	approval := approvalForTestPlan(t, &plan)
	_, err = newSvc(nil).Execute(context.Background(),
		ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})
	if err != nil {
		for _, refusal := range []string{"image_unresolved", "is not the one the plan bound", "configuration changed since plan"} {
			if strings.Contains(err.Error(), refusal) {
				t.Fatalf("a saved plan must be applicable without re-supplying the image: %v", err)
			}
		}
	}
}

// 10.2 — the runtime a plan binds is the runtime generation produces.
//
// A plan is only a promise if the bytes it bound can be reproduced from the
// same inputs. If planning rendered through any path that a later render does
// not take, the binding check would be comparing the generator against itself
// and would pass no matter what shipped.
func TestAPlanBindsTheSameBytesGenerationProduces(t *testing.T) {
	fake := serviceFake()
	svc := newTestService(t, fake)

	plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}

	// Render again from the project the plan named, with the plan's own
	// release identity, and compare against the digest it bound.
	lp, err := loadProjectAt(context.Background(), svc.configPath, svc.environment, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.HashBytes(lp.composeBytes); got != plan.Operation.Binding.ComposeDigest {
		t.Errorf("a fresh render does not reproduce the bytes the plan bound:\n plan:  %s\n fresh: %s",
			plan.Operation.Binding.ComposeDigest, got)
	}
	if got := engine.HashBytes(lp.configBytes); got != plan.Artifact.ConfigHash {
		t.Errorf("the configuration digest does not reproduce:\n plan:  %s\n fresh: %s",
			plan.Artifact.ConfigHash, got)
	}

	// And twice more, to catch anything order-dependent.
	for i := range 5 {
		again, err := loadProjectAt(context.Background(), svc.configPath, svc.environment, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if engine.HashBytes(again.composeBytes) != plan.Operation.Binding.ComposeDigest {
			t.Fatalf("render %d does not reproduce the plan's bytes", i)
		}
	}
}
