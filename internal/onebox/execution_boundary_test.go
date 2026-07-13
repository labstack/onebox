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
			Target:            "deploy@example.invalid",
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

func TestLegacyDeployPlansAreRejectedBeforeConnecting(t *testing.T) {
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
			wantReason: "legacy executable deploy plan has no schema_version",
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
			path := filepath.Join(t.TempDir(), "legacy-plan.json")
			if err := os.WriteFile(path, document, 0o600); err != nil {
				t.Fatal(err)
			}

			_, err = LoadDeployPlan(path)
			assertLegacyPlanGuidance(t, err, tt.wantReason)

			plan := valid
			plan.SchemaVersion = tt.schemaVersion
			connected := false
			fake := serviceFake()
			svc := New(Options{
				ConfigPath: filepath.Join(t.TempDir(), "must-not-be-read.yml"),
				Now:        func() time.Time { return base },
				Connect: func(context.Context, string) (transport.Transport, error) {
					connected = true
					return fake, nil
				},
			})
			result, err := svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
			assertLegacyPlanGuidance(t, err, tt.wantReason)
			if connected {
				t.Fatal("legacy plan connected to the target")
			}
			if len(fake.Commands) != 0 || len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
				t.Fatalf("legacy plan reached a mutation path: commands=%v uploads=%v inputs=%v", fake.Commands, fake.Uploads, fake.Inputs)
			}
			if result.Status != "failed" {
				t.Fatalf("legacy plan result status = %q, want failed", result.Status)
			}
		})
	}
}

func assertLegacyPlanGuidance(t *testing.T, err error, wantReason string) {
	t.Helper()
	if err == nil {
		t.Fatal("legacy plan was accepted")
	}
	for _, fragment := range []string{
		wantReason,
		ExecutableDeployPlanSchemaVersion,
		"upgrade `ob`",
		"`ob plan`",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("legacy-plan error %q does not contain actionable guidance %q", err, fragment)
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
		Connect: func(context.Context, string) (transport.Transport, error) {
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
	if result.Status != "failed" || result.ID != plan.Operation.ID {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(events) != 2 {
		t.Fatalf("events = %#v, want started then failed", events)
	}
	if events[0].Sequence != 1 || events[0].Phase != "operation" || events[0].Status != "started" ||
		events[1].Sequence != 2 || events[1].Phase != "operation" || events[1].Status != "failed" {
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
		Connect: func(context.Context, string) (transport.Transport, error) {
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
	// A Compose edit that leaves the parsed service set (and thus the graph)
	// intact — only the Compose digest binds it.
	composePath := filepath.Join(filepath.Dir(svc.configPath), "docker-compose.yaml")
	source, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composePath, append(source, []byte("\n# drift introduced after planning\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.Uploads, fake.Inputs = nil, nil
	result, err := svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan})
	if err == nil || !strings.Contains(err.Error(), "Compose file changed since plan") {
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
	if err == nil || !strings.Contains(err.Error(), "release payload differs from the plan") {
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
		if strings.Contains(command, "docker ps -q") && strings.Contains(command, "postgres") {
			return transport.Result{Stdout: "PG1\n"}, true
		}
		if strings.Contains(command, "docker inspect") && strings.Contains(command, "PG1") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		if strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml") {
			return transport.Result{Stdout: "services:\n  server:\n    image: changed-after-plan\n"}, true
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
		if strings.Contains(command, "docker ps -q") && strings.Contains(command, "postgres") {
			return transport.Result{Stdout: "PG1\n"}, true
		}
		if strings.Contains(command, "docker inspect") && strings.Contains(command, "PG1") {
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
		"operation:started",
		"binding:started",
		"binding:succeeded",
		"stage:started",
		"stage:succeeded",
		"operation:no_op",
	}
	got := make([]string, len(events))
	for i, event := range events {
		got[i] = event.Phase + ":" + event.Status
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
				`{"deploy_id":"R-INCOMPLETE","event":"start","ts":"2026-07-12T19:00:00Z"}` + "\n"}, true
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
	if last.OperationID != result.ID || last.EvidenceID != "R-INCOMPLETE" || last.Status != "failed" {
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
	changed := strings.Replace(string(source), "target: deploy@example.invalid", "target: deploy@other.invalid", 1)
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
	if len(events) != 1 || events[0].OperationID != result.ID || events[0].Status != "failed" {
		t.Fatalf("early failure event identity mismatch: result=%#v events=%#v", result, events)
	}
	if strings.Contains(events[0].Message, "missing.yml") {
		t.Fatalf("structured event leaked local diagnostics: %#v", events[0])
	}
}
