package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
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

type foreignExitError struct{ code int }

func (err foreignExitError) Error() string { return "foreign command failed" }
func (err foreignExitError) ExitCode() int { return err.code }

type failingOutputWriter struct{ err error }

func (writer failingOutputWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestWithExitCodeDoesNotTrustForeignExitCodeMethods(t *testing.T) {
	err := withExitCode(foreignExitError{code: 17}, 2)
	var cliErr *cliExitError
	if !errors.As(err, &cliErr) || cliErr.ExitCode() != 2 {
		t.Fatalf("wrapped error = %T %v", err, err)
	}
}

func TestStructuredFailurePreservesCommandAndOutputErrors(t *testing.T) {
	commandErr := errors.New("original operation failure")
	outputErr := errors.New("output sink failed")
	cmd := &cobra.Command{}
	cmd.SetOut(failingOutputWriter{err: outputErr})
	err := writeStructuredCommandFailure(cmd, &globalFlags{Output: "json"}, "operation_failed", "operation failed", commandErr)
	if !errors.Is(err, commandErr) || !errors.Is(err, outputErr) {
		t.Fatalf("joined error = %v", err)
	}
	var exitErr *cliExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("joined failure lost exit code: %v", err)
	}
}

func TestStructuredCancellationPreservesOutputErrorAndExitCode(t *testing.T) {
	outputErr := errors.New("output sink failed")
	cmd := &cobra.Command{}
	cmd.SetOut(failingOutputWriter{err: outputErr})
	err := writeCancelled(cmd, &globalFlags{Output: "json"}, "operator cancelled")
	if !errors.Is(err, outputErr) || !strings.Contains(err.Error(), "operator cancelled") {
		t.Fatalf("joined cancellation = %v", err)
	}
	var exitErr *cliExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("joined cancellation lost exit code: %v", err)
	}
}

func TestFiniteSuccessEnvelopeWireFormat(t *testing.T) {
	var out bytes.Buffer
	cmd := outputTestCommand(&out)
	if err := writeFiniteSuccess(cmd, &globalFlags{Output: "json"}, map[string]any{"value": "ok"}); err != nil {
		t.Fatal(err)
	}
	want := "{\n" +
		"  \"schema_version\": \"onebox.run/cli/v1alpha1\",\n" +
		"  \"command\": \"ob\",\n" +
		"  \"outcome\": \"success\",\n" +
		"  \"data\": {\n" +
		"    \"value\": \"ok\"\n" +
		"  }\n" +
		"}\n"
	if out.String() != want {
		t.Fatalf("success envelope changed:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestFiniteErrorEnvelopeWireFormat(t *testing.T) {
	var out bytes.Buffer
	cmd := outputTestCommand(&out)
	publicErr := &cliPublicError{
		Code: "state_diverged", SafeMessage: "runtime state diverged",
		DiagnosticCommand: "ob status", NextCommand: "ob plan",
		ResolvingCommand: "ob deploy --plan PLAN", Path: "workloads.api",
	}
	if err := writeFiniteOutcome(cmd, &globalFlags{Output: "json"}, cliOutcomeError, nil, publicErr); err != nil {
		t.Fatal(err)
	}
	want := "{\n" +
		"  \"schema_version\": \"onebox.run/cli/v1alpha1\",\n" +
		"  \"command\": \"ob\",\n" +
		"  \"outcome\": \"error\",\n" +
		"  \"error\": {\n" +
		"    \"code\": \"state_diverged\",\n" +
		"    \"safe_message\": \"runtime state diverged\",\n" +
		"    \"diagnostic_command\": \"ob status\",\n" +
		"    \"next_command\": \"ob plan\",\n" +
		"    \"resolving_command\": \"ob deploy --plan PLAN\",\n" +
		"    \"path\": \"workloads.api\"\n" +
		"  }\n" +
		"}\n"
	if out.String() != want {
		t.Fatalf("error envelope changed:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestNDJSONWireFormat(t *testing.T) {
	var out bytes.Buffer
	stream := newCLIRecordStream(&out, "ob exec")
	if err := stream.write("output", func(record *cliRecord) {
		record.Channel = "stdout"
		record.Chunk = "hello\n"
	}); err != nil {
		t.Fatal(err)
	}
	if err := stream.write("event", func(record *cliRecord) {
		record.Event = map[string]any{"phase": "verify", "status": "success"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := stream.terminal(cliOutcomeSuccess, map[string]any{"id": "op-1"}, nil); err != nil {
		t.Fatal(err)
	}
	want := "{\"schema_version\":\"onebox.run/cli/v1alpha1\",\"command\":\"ob exec\",\"sequence\":1,\"kind\":\"output\",\"channel\":\"stdout\",\"chunk\":\"hello\\n\"}\n" +
		"{\"schema_version\":\"onebox.run/cli/v1alpha1\",\"command\":\"ob exec\",\"sequence\":2,\"kind\":\"event\",\"event\":{\"phase\":\"verify\",\"status\":\"success\"}}\n" +
		"{\"schema_version\":\"onebox.run/cli/v1alpha1\",\"command\":\"ob exec\",\"sequence\":3,\"kind\":\"terminal\",\"outcome\":\"success\",\"data\":{\"id\":\"op-1\"}}\n"
	if out.String() != want {
		t.Fatalf("NDJSON wire format changed:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestJSONOperationOutputIsOrderedAndRedactsErrors(t *testing.T) {
	var out bytes.Buffer
	output := newCLIOperationOutput(outputTestCommand(&out), &globalFlags{Output: "json"})
	output.event(onebox.OperationEvent{Sequence: 2, Phase: "verify", Status: onebox.OperationStatusSuccess})
	output.event(onebox.OperationEvent{Sequence: 1, Phase: "verify", Status: onebox.OperationStatusRunning})
	result := onebox.OperationResult{ID: "op-1", Status: onebox.OperationStatusError}
	if err := output.finish(&result, errors.New("password=hunter2")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Fatalf("structured output leaked detailed error: %s", out.String())
	}
	var envelope struct {
		cliEnvelope
		Data struct {
			Events []onebox.OperationEvent `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out.String())
	}
	if envelope.SchemaVersion != cliSchemaVersion || envelope.Command != "ob" || envelope.Outcome != cliOutcomeError {
		t.Fatalf("schema = %q", envelope.SchemaVersion)
	}
	if envelope.Error == nil || envelope.Error.Details == nil {
		t.Fatalf("error details = %+v", envelope.Error)
	}
	if envelope.Error.Code != "operation_failed" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

func TestNDJSONOperationOutputEmitsEventsThenResult(t *testing.T) {
	var out bytes.Buffer
	output := newCLIOperationOutput(outputTestCommand(&out), &globalFlags{Output: "ndjson"})
	output.event(onebox.OperationEvent{Sequence: 1, Phase: "binding", Status: onebox.OperationStatusRunning})
	result := onebox.OperationResult{ID: "op-2", Status: onebox.OperationStatusSuccess}
	if err := output.finish(&result, nil); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d records:\n%s", len(lines), out.String())
	}
	for i, wantType := range []string{"event", "terminal"} {
		var record cliRecord
		if err := json.Unmarshal([]byte(lines[i]), &record); err != nil {
			t.Fatalf("decode record %d: %v", i, err)
		}
		if record.SchemaVersion != cliSchemaVersion || record.Kind != wantType || record.Sequence != uint64(i+1) {
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
	var envelope cliEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode output: %v\n%s", decodeErr, out.String())
	}
	if envelope.SchemaVersion != cliSchemaVersion || envelope.Command != "ob deploy" || envelope.Outcome != cliOutcomeError || envelope.Error == nil {
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
			Application: artifact.App, Environment: artifact.Env, Server: "deploy@example.invalid",
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
	if err == nil || !strings.Contains(err.Error(), "requires an explicit plan-bound local-confirmation artifact") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(out.String(), "Approve exact plan") || strings.Contains(out.String(), "not approved") {
		t.Fatalf("structured stdout contains interactive text: %s", out.String())
	}
	var envelope cliEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode output: %v\n%s", decodeErr, out.String())
	}
	if envelope.SchemaVersion != cliSchemaVersion || envelope.Command != "ob deploy" || envelope.Outcome != cliOutcomeError || envelope.Error == nil {
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
		if version != cliSchemaVersion {
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

func TestSchemaImplementsTheCommonJSONEnvelope(t *testing.T) {
	out, err := run(t, t.TempDir(), "schema", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope cliEnvelope
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || envelope.SchemaVersion != cliSchemaVersion || envelope.Command != "ob schema" {
		t.Fatalf("schema envelope = %+v, decode=%v", envelope, err)
	}
}

func TestCommandGroupsValidateOutputBeforeRenderingHelp(t *testing.T) {
	for _, path := range [][]string{
		{},
		{"service"},
		{"proxy"},
		{"secrets"},
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
		// A group executes nothing, so it has no matrix row — but a caller that
		// asked for JSON is parsing a stream, and human help or zero bytes are
		// both unreadable to it. It gets a refusal envelope in the mode it asked
		// for.
		var envelope cliEnvelope
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Fatalf("%s: output is not a JSON envelope (%v):\n%s", command, err, out)
		}
		if envelope.SchemaVersion != cliSchemaVersion || envelope.Command != command ||
			envelope.Outcome != cliOutcomeError || envelope.Error == nil ||
			envelope.Error.Code != "output_mode_incompatible" {
			t.Errorf("%s: envelope = %+v", command, envelope)
		}
		if envelope.Error != nil && envelope.Error.DiagnosticCommand == "" {
			t.Errorf("%s: refusal carries no diagnostic command", command)
		}
	}
}

func TestEjectStructuredOutputIsVersioned(t *testing.T) {
	for _, mode := range []string{"json"} {
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
		var envelope struct {
			cliEnvelope
			Data struct {
				Runtime   string   `json:"runtime"`
				Workloads []string `json:"workloads"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Fatalf("%s: decode structured output: %v\n%s", mode, err, out)
		}
		if envelope.SchemaVersion != cliSchemaVersion || envelope.Command != "ob eject" {
			t.Errorf("%s: schema version = %q", mode, envelope.SchemaVersion)
		}
		if envelope.Data.Runtime == "" || len(envelope.Data.Workloads) != 1 || envelope.Data.Workloads[0] != "shop" {
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
			Command       string          `json:"command"`
			Outcome       string          `json:"outcome"`
			Error         *cliPublicError `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &record); err != nil {
			t.Fatalf("%s: decode failure record: %v\n%s", verb, err, out)
		}
		if record.SchemaVersion != cliSchemaVersion || record.Command != "ob "+verb || record.Outcome != cliOutcomeError || record.Error == nil {
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

func TestLeafOutputMatrixIsClosedAndHasNoAliases(t *testing.T) {
	wantMatrix := map[string]cliOutputClass{
		"ob abort":          {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob approve":        {Class: "finite_envelope", JSON: true},
		"ob audit":          {Class: "finite_envelope", JSON: true},
		"ob bootstrap":      {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob canonical":      {Class: "finite_envelope", JSON: true},
		"ob deploy":         {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob destroy":        {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob doctor":         {Class: "finite_envelope", JSON: true},
		"ob eject":          {Class: "finite_envelope", JSON: true},
		"ob exec":           {Class: "operator_passthrough", NDJSON: true},
		"ob init":           {Class: "finite_envelope", JSON: true},
		"ob job plan":       {Class: "finite_envelope", JSON: true},
		"ob backup create":  {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob backup enable":  {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob backup disable": {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob backup drill":   {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob backup restore": {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob backup prune":   {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob backup verify":  {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob backup status":  {Class: "finite_envelope", JSON: true},
		"ob job run":        {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob logs":           {Class: "operator_passthrough", JSON: true, NDJSON: true},
		"ob plan":           {Class: "finite_envelope", JSON: true},
		"ob preflight":      {Class: "finite_envelope", JSON: true},
		"ob preview":        {Class: "finite_envelope", JSON: true},
		"ob proxy apply":    {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob resume":         {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob rollback":       {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob schedule apply": {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob schema":         {Class: "finite_envelope", JSON: true},
		"ob secrets edit":   {Class: "trusted_editor", JSON: true},
		"ob secrets list":   {Class: "finite_envelope", JSON: true},
		"ob secrets push":   {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob service apply":  {Class: "finite_stream", JSON: true, NDJSON: true},
		"ob status":         {Class: "finite_envelope", JSON: true},
		"ob validate":       {Class: "finite_envelope", JSON: true},
		"ob version":        {Class: "finite_envelope", JSON: true},
	}
	if !reflect.DeepEqual(cliOutputMatrix, wantMatrix) {
		t.Fatalf("CLI output matrix changed:\ngot  %#v\nwant %#v", cliOutputMatrix, wantMatrix)
	}
	root := newRootCmd()
	seen := map[string]bool{}
	validClasses := map[string]bool{
		cliClassFiniteEnvelope: true, cliClassFiniteStream: true,
		cliClassOperatorPassthrough: true, cliClassTrustedEditor: true,
	}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if len(cmd.Aliases) != 0 {
			t.Errorf("%s has pre-release aliases: %v", cmd.CommandPath(), cmd.Aliases)
		}
		children := cmd.Commands()
		for _, child := range children {
			if child.Name() == "help" || child.Name() == "completion" {
				continue
			}
			walk(child)
		}
		if cmd == root || len(children) != 0 || cmd.Name() == "help" || cmd.Name() == "completion" {
			return
		}
		path := cmd.CommandPath()
		seen[path] = true
		class, ok := cliOutputMatrix[path]
		if !ok {
			t.Errorf("leaf %s has no output class", path)
			return
		}
		if !validClasses[class.Class] {
			t.Errorf("leaf %s has invalid class %q", path, class.Class)
		}
		if !class.JSON && !class.NDJSON {
			t.Errorf("leaf %s has no machine output mode", path)
		}
	}
	walk(root)
	for path := range cliOutputMatrix {
		if !seen[path] {
			t.Errorf("output matrix contains no leaf %s", path)
		}
	}
}

func TestIncompatibleLeafModeReturnsTypedEnvelope(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"--output", "json", "exec", "--reason", "test output contract", "web", "--", "true"})
	err := root.Execute()
	if err == nil {
		t.Fatal("exec JSON unexpectedly succeeded")
	}
	var envelope cliEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode refusal: %v\n%s", decodeErr, out.String())
	}
	if envelope.Outcome != cliOutcomeError || envelope.Error == nil || envelope.Error.Code != "output_mode_incompatible" {
		t.Fatalf("refusal = %+v", envelope)
	}
	if envelope.Error.DiagnosticCommand != "ob help exec" || envelope.Error.ResolvingCommand != "" {
		t.Fatalf("guidance = %+v", envelope.Error)
	}
}

func TestStructuredDeployWithoutPlanCarriesTheTypedCodeAndNextStep(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"deploy", "--output", "json"})
	if err := root.Execute(); err == nil {
		t.Fatal("structured deploy without --plan succeeded")
	}
	var envelope cliEnvelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out.String())
	}
	// The instruction an agent needs is "go and plan first". Before this carried
	// a code it arrived as the generic operation_failed with the actual sentence
	// stranded on stderr, which is unreadable to the caller that asked for JSON.
	if envelope.Error == nil || envelope.Error.Code != "plan_required" {
		t.Fatalf("error = %+v", envelope.Error)
	}
	if envelope.Error.NextCommand != "ob plan --output json" {
		t.Fatalf("next command = %q", envelope.Error.NextCommand)
	}
}

func TestUnsupportedOutputModeStillAnswersInMachineForm(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"validate", "--output", "yaml"})
	if err := root.Execute(); err == nil {
		t.Fatal("unsupported output mode succeeded")
	}
	// No framing was successfully selected, so the refusal falls back to JSON
	// rather than to nothing: a caller that mistypes the mode must still be able
	// to read why.
	var envelope cliEnvelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out.String())
	}
	if envelope.Error == nil || envelope.Error.Code != "output_mode_incompatible" {
		t.Fatalf("error = %+v", envelope.Error)
	}
	if envelope.Error.DiagnosticCommand != "ob help validate" {
		t.Fatalf("diagnostic command = %q", envelope.Error.DiagnosticCommand)
	}
}

func TestEveryPublishedOperationCodeResolvesToItsEnvelopeMessage(t *testing.T) {
	// The envelope and the generated error reference read the same registry, so
	// a code cannot mean one thing to an operator and another in the docs.
	for _, code := range onebox.OperationFailureCodes() {
		failure, _ := onebox.OperationFailureFor(code)
		if got := safeMessageForCode(code, "FALLBACK"); got != failure.Message {
			t.Errorf("code %q: envelope message %q, published %q", code, got, failure.Message)
		}
		if got := guidanceCommandForCode(code); got != failure.Command {
			t.Errorf("code %q: envelope command %q, published %q", code, got, failure.Command)
		}
	}
}
