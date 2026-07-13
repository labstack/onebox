package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/onebox"
)

func outputTestCommand(out *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	return cmd
}

func TestJSONOperationOutputIsOrderedAndRedactsErrors(t *testing.T) {
	var out bytes.Buffer
	output := newCLIOperationOutput(outputTestCommand(&out), &globalFlags{Output: "json"})
	output.event(onebox.OperationEvent{Sequence: 2, Phase: "verify", Status: "succeeded"})
	output.event(onebox.OperationEvent{Sequence: 1, Phase: "verify", Status: "started"})
	result := onebox.OperationResult{ID: "op-1", Status: "failed"}
	if err := output.finish(&result, errors.New("password=hunter2")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Fatalf("structured output leaked detailed error: %s", out.String())
	}
	var envelope cliOperationEnvelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out.String())
	}
	if envelope.SchemaVersion != cliOperationSchemaVersion {
		t.Fatalf("schema = %q", envelope.SchemaVersion)
	}
	if len(envelope.Events) != 2 || envelope.Events[0].Sequence != 1 || envelope.Events[1].Sequence != 2 {
		t.Fatalf("events not sequence ordered: %+v", envelope.Events)
	}
	if envelope.Error == nil || envelope.Error.Code != "operation_failed" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

func TestNDJSONOperationOutputEmitsEventsThenResult(t *testing.T) {
	var out bytes.Buffer
	output := newCLIOperationOutput(outputTestCommand(&out), &globalFlags{Output: "ndjson"})
	output.event(onebox.OperationEvent{Sequence: 1, Phase: "binding", Status: "started"})
	result := onebox.OperationResult{ID: "op-2", Status: "succeeded"}
	if err := output.finish(&result, nil); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d records:\n%s", len(lines), out.String())
	}
	for i, wantType := range []string{"event", "result"} {
		var record cliOperationRecord
		if err := json.Unmarshal([]byte(lines[i]), &record); err != nil {
			t.Fatalf("decode record %d: %v", i, err)
		}
		if record.SchemaVersion != cliRecordSchemaVersion || record.Type != wantType {
			t.Fatalf("record %d = %+v", i, record)
		}
	}
}

func TestSafeStatusSnapshotRedactsObservationDetails(t *testing.T) {
	snapshot := safeStatusSnapshot(engine.StatusSnapshot{Warnings: []engine.StatusWarning{{
		Component: "containers",
		Message:   "remote failed with API_TOKEN=secret",
	}}})
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("status warning leaked details: %s", encoded)
	}
	if snapshot.Warnings[0].Component != "containers" {
		t.Fatalf("component was not preserved: %+v", snapshot.Warnings[0])
	}
}

func TestStructuredDeployWithoutPlanReturnsMachineReadableFailure(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"deploy", "--output", "json"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires --plan") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(out.String(), "requires --plan") {
		t.Fatalf("structured payload leaked detailed error: %s", out.String())
	}
	var envelope cliOperationEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode output: %v\n%s", decodeErr, out.String())
	}
	if envelope.SchemaVersion != cliOperationSchemaVersion || envelope.Error == nil {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestDeployApprovalRequiresSavedPlan(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"deploy", "--approval", "ob-approval.json"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--approval requires --plan") {
		t.Fatalf("error = %v", err)
	}
}

func TestStructuredDeployRequiresApprovalArtifactWithoutPrompting(t *testing.T) {
	createdAt := time.Now().UTC()
	artifact := engine.Artifact{
		ID: "R-structured", App: "demo", Env: "production", CreatedAt: createdAt,
		ConfigHash: "sha256:config",
		HostState: engine.HostState{
			Host: "example.invalid", CurrentRelease: "R0",
			ImageIDs: map[string]string{"server": "sha256:image"},
		},
		PinnedImages:    map[string]string{"server": "ghcr.io/example/app@sha256:digest"},
		RenderedCompose: "services: {}\n", Commands: []string{"deploy release R-structured"},
	}
	encodedArtifact, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	operation := onebox.OperationPlan{
		SchemaVersion: onebox.OperationPlanSchemaVersion,
		ID:            "operation-structured", Kind: onebox.KindDeploy, ReleaseID: artifact.ID,
		CreatedAt: createdAt.Format(time.RFC3339Nano), ExpiresAt: createdAt.Add(15 * time.Minute).Format(time.RFC3339Nano),
		Risk: onebox.RiskModerate, Reversibility: onebox.ReversibilityReversible, Approval: onebox.ApprovalOneTime,
		Binding: onebox.OperationBinding{
			Application: artifact.App, Environment: artifact.Env, Target: "deploy@example.invalid",
			ConfigDigest: artifact.ConfigHash, ComposeDigest: "sha256:compose",
			StateDigest: engine.HashBytes(encodedArtifact), PayloadDigest: "payload-digest",
			LiveComposeDigest: "sha256:live-compose", LivePayloadDigest: "live-payload",
		},
		Steps: []onebox.OperationStep{{ID: "preflight", Kind: onebox.StepPreflight, DataEffect: onebox.DataEffectNone}},
	}
	if err := operation.Seal(); err != nil {
		t.Fatal(err)
	}
	plan := onebox.DeployPlan{
		SchemaVersion: onebox.ExecutableDeployPlanSchemaVersion,
		Runner:        onebox.CurrentRunnerProvenance(), Operation: operation, Artifact: artifact,
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Save(planPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"deploy", "--output", "json", "--plan", planPath})
	err = root.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires an explicit plan-bound --approval") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(out.String(), "Approve exact plan") || strings.Contains(out.String(), "not approved") {
		t.Fatalf("structured stdout contains interactive text: %s", out.String())
	}
	var envelope cliOperationEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode output: %v\n%s", decodeErr, out.String())
	}
	if envelope.SchemaVersion != cliOperationSchemaVersion || envelope.Error == nil {
		t.Fatalf("envelope = %+v", envelope)
	}
}
