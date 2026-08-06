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

// The structured stream carries one JSON document and nothing else.
//
// A diagnostic printed alongside it — a warning, a progress line, a hint —
// makes the stream unparseable for the consumer it exists to serve, and the
// failure appears at the consumer rather than here.
func TestStructuredOutputCarriesNoDiagnostics(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ob.yml", `api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@203.0.113.10}
runtime:
  env_files: [.env.production]
image: nginx
domain: shop.example.com
port: 3000
`)
	writeFile(t, dir, ".env.production", "API_TOKEN=super-secret-value\nPUBLIC_MODE=on\n")

	for _, verb := range []string{"validate", "canonical", "preview"} {
		out, err := run(t, dir, verb, "--output", "json")
		if err != nil {
			t.Fatalf("%s: %v\n%s", verb, err, out)
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Fatalf("%s: the stream is not one JSON document: %v\n%s", verb, err, out)
		}
		version, _ := envelope["schema_version"].(string)
		if !strings.HasPrefix(version, "onebox.run/cli-") {
			t.Errorf("%s: structured output must name its schema, got %q", verb, version)
		}
	}
}

// No plaintext secret reaches the structured stream. It is the form that gets
// piped into a file, a log or a CI artifact, where a value nobody meant to
// publish outlives the terminal it would have scrolled off.
func TestStructuredOutputCarriesNoPlaintextSecret(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ob.yml", `api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@203.0.113.10}
workloads:
  web:
    role: application
    image: nginx
    domain: shop.example.com
    port: 3000
    env:
      API_TOKEN: super-secret-value
`)
	for _, verb := range []string{"canonical", "preview"} {
		out, err := run(t, dir, verb, "--output", "json")
		if err != nil {
			t.Fatalf("%s: %v\n%s", verb, err, out)
		}
		if strings.Contains(out, "super-secret-value") {
			t.Errorf("%s: a declared value reached the structured stream:\n%s", verb, out)
		}
	}
	// And --raw cannot be used to defeat it.
	if _, err := run(t, dir, "preview", "--output", "json", "--raw"); err == nil {
		t.Error("--raw beside --output json must be refused, not silently ignored")
	}
}

func TestStructuredOutputIsRejectedWhenACommandDoesNotImplementIt(t *testing.T) {
	if _, err := run(t, t.TempDir(), "schema", "--output", "json"); err == nil {
		t.Fatal("schema silently accepted an output mode it does not implement")
	} else if !strings.Contains(err.Error(), "--output json is not supported by ob schema") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCommandGroupsValidateOutputBeforeRenderingHelp(t *testing.T) {
	for _, path := range [][]string{
		{},
		{"service"},
		{"proxy"},
		{"secrets"},
		{"backup-evidence"},
	} {
		args := append(append([]string(nil), path...), "--output", "json")
		out, err := run(t, t.TempDir(), args...)
		command := strings.TrimSpace("ob " + strings.Join(path, " "))
		if err == nil {
			t.Fatalf("%s silently rendered human help for JSON output", command)
		}
		if want := "--output json is not supported by " + command; !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", command, err, want)
		}
		if out != "" {
			t.Errorf("%s: unsupported structured output wrote human help:\n%s", command, out)
		}
	}
}

func TestEjectStructuredOutputIsVersioned(t *testing.T) {
	for _, mode := range []string{"json", "ndjson"} {
		dir := t.TempDir()
		writeFile(t, dir, "ob.yml", `api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@203.0.113.10}
image: nginx
`)
		out, err := run(t, dir, "eject", "--output", mode)
		if err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, out)
		}
		var envelope cliEjectEnvelope
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Fatalf("%s: decode structured output: %v\n%s", mode, err, out)
		}
		if envelope.SchemaVersion != cliEjectSchemaVersion {
			t.Errorf("%s: schema version = %q", mode, envelope.SchemaVersion)
		}
		if envelope.Runtime == "" || len(envelope.Workloads) != 1 || envelope.Workloads[0] != "shop" {
			t.Errorf("%s: incomplete eject envelope: %+v", mode, envelope)
		}
	}
}

func TestStructuredReadFailuresEmitTypedSafeRecords(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ob.yml", `api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@203.0.113.10}
workloads:
  web: {role: application, image: nginx, replicaz: 3}
`)
	for _, verb := range []string{"validate", "canonical", "preview", "eject"} {
		out, err := run(t, dir, verb, "--output", "json")
		if err == nil {
			t.Fatalf("%s: invalid project succeeded", verb)
		}
		var record struct {
			SchemaVersion string          `json:"schema_version"`
			Error         *cliPublicError `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &record); err != nil {
			t.Fatalf("%s: decode failure record: %v\n%s", verb, err, out)
		}
		if record.SchemaVersion == "" || record.Error == nil {
			t.Fatalf("%s: incomplete failure record: %+v", verb, record)
		}
		if record.Error.Code != "unknown_field" || record.Error.Path != "workloads.web.replicaz" {
			t.Errorf("%s: failure = %+v", verb, record.Error)
		}
		if strings.Contains(out, "did you mean") {
			t.Errorf("%s: detailed diagnostic leaked into the structured stream: %s", verb, out)
		}
	}
}
