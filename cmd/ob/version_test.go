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
	var got versionReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode version JSON: %v\n%s", err, out)
	}
	if got.SchemaVersion != versionReportSchemaVersion {
		t.Fatalf("schema version = %q, want %q", got.SchemaVersion, versionReportSchemaVersion)
	}
	want := buildinfo.Read()
	if got.Info != want {
		t.Fatalf("build info = %+v, want %+v", got.Info, want)
	}
	if len(got.SupportedExecutablePlanSchemas) != 1 || got.SupportedExecutablePlanSchemas[0] != onebox.ExecutableDeployPlanSchemaVersion {
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
