package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/onebox"
)

func executeRoot(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestVersionJSON(t *testing.T) {
	out := executeRoot(t, "--output", "json", "version")
	var envelope struct {
		SchemaVersion string        `json:"schema_version"`
		Command       string        `json:"command"`
		Outcome       string        `json:"outcome"`
		Data          versionReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode version JSON: %v\n%s", err, out)
	}
	if envelope.SchemaVersion != cliSchemaVersion || envelope.Command != "ob version" || envelope.Outcome != cliOutcomeSuccess {
		t.Fatalf("version envelope = %+v", envelope)
	}
	got := envelope.Data
	want := buildinfo.Read()
	if got.Info != want {
		t.Fatalf("build info = %+v, want %+v", got.Info, want)
	}
	wantSchemas := []string{onebox.ExecutableDeployPlanSchemaVersion, onebox.ExecutableJobPlanSchemaVersion}
	if len(got.SupportedExecutablePlanSchemas) != len(wantSchemas) ||
		got.SupportedExecutablePlanSchemas[0] != wantSchemas[0] || got.SupportedExecutablePlanSchemas[1] != wantSchemas[1] {
		t.Fatalf("supported schemas = %v", got.SupportedExecutablePlanSchemas)
	}
}

func TestVersionHumanReadable(t *testing.T) {
	out := executeRoot(t, "version")
	for _, want := range []string{
		"ob version " + buildinfo.Read().Version,
		"vcs revision:",
		"dirty:",
		"vcs time:",
		"build time:",
		"go version:",
		onebox.ExecutableDeployPlanSchemaVersion,
		onebox.ExecutableJobPlanSchemaVersion,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("version output missing %q:\n%s", want, out)
		}
	}
}

func TestRootVersionFlagIsRetained(t *testing.T) {
	out := executeRoot(t, "--version")
	want := "ob version " + buildinfo.Read().Version
	if !strings.Contains(out, want) {
		t.Fatalf("--version output missing %q: %s", want, out)
	}
}
