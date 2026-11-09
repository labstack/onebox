package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

const mainTestProject = `apiVersion: onebox.run/v1alpha2
kind: Application
metadata:
  name: demo
spec:
  environments: {production: {server: deploy@example.invalid}}
  proxy: {kind: None}
  workloads:
    demo:
      image: nginx:1.27
`
const mainTestBuildProject = `apiVersion: onebox.run/v1alpha2
kind: Application
metadata:
  name: demo
spec:
  environments: {production: {server: deploy@example.invalid}}
  workloads:
    api:
      role: Application
      build: {context: ., dockerfile: Dockerfile}
  proxy: {kind: None}
`

func TestRootHelpListsVerbs(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ob") {
		t.Fatalf("help output missing binary name: %s", out.String())
	}
}

func TestRetiredExecutionCommandsAreRejected(t *testing.T) {
	for _, verb := range []string{"list", "inspect", "resume", "abandon"} {
		root := newRootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"execution", verb})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), `unknown command "execution"`) {
			t.Fatalf("retired execution %s command accepted: %v", verb, err)
		}
	}
}

func TestImplicitProjectFileAcceptsYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ob.yaml"), []byte(mainTestProject), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"validate"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("implicit ob.yaml: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("implicit ob.yaml was not validated: %s", out.String())
	}
}

func TestImplicitProjectFilePrefersCanonicalYML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(mainTestProject), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ob.yaml"), []byte("not: a onebox project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"validate"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("implicit project selection did not prefer ob.yml: %v\n%s", err, out.String())
	}
}

func TestValidateAcceptsBuildSourceWithoutReleaseImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(mainTestBuildProject), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"validate"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("build metadata is a valid contract without a release image: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("build-sourced project was not validated: %s", out.String())
	}
}

func TestInitTreatsImplicitYAMLAsExistingProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ob.yaml"), []byte(mainTestProject), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cmd := newRootCmd()
	cmd.SetArgs([]string{"init"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "ob.yaml already exists") {
		t.Fatalf("init must not overwrite an implicit ob.yaml project: %v", err)
	}
}

func TestExplicitProjectPathDoesNotFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ob.yaml"), []byte(mainTestProject), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cmd := newRootCmd()
	cmd.SetArgs([]string{"-c", "ob.yml", "validate"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "cannot read project file") {
		t.Fatalf("explicit missing ob.yml must not fall back to ob.yaml: %v", err)
	}
}

func TestTestHostStateOverrideIsNeverSilent(t *testing.T) {
	var out bytes.Buffer
	t.Setenv(app.TestHostStateDirEnv, "/tmp/fixture-host")
	t.Cleanup(applyTestHostStateOverride(&out))
	if !strings.Contains(out.String(), "unset it on real hosts") {
		t.Fatalf("override warning = %q", out.String())
	}
	if got := (app.Names{}).HostDir(); got != "/tmp/fixture-host" {
		t.Fatalf("override not applied: %s", got)
	}
	out.Reset()
	t.Setenv(app.TestHostStateDirEnv, "relative")
	applyTestHostStateOverride(&out)
	if !strings.Contains(out.String(), "is ignored") {
		t.Fatalf("relative override warning = %q", out.String())
	}
}

// Every top-level command must land in a help group; an ungrouped command
// falls into Cobra's "Additional Commands" bucket and breaks the ordering
// newcomers rely on.
func TestEveryTopLevelCommandHasAHelpGroup(t *testing.T) {
	for _, c := range newRootCmd().Commands() {
		if c.GroupID == "" && c.Name() != "help" && c.Name() != "completion" {
			t.Errorf("command %q has no help group; add it to commandGroups", c.Name())
		}
	}
}

// A removed command or flag names its replacement rather than failing as if
// it had never existed.
func TestRemovedCommandAndFlagNameTheirReplacement(t *testing.T) {
	dir := writeProject(t)
	if _, err := run(t, dir, "abort"); err == nil || !strings.Contains(err.Error(), "ob rollback") {
		t.Errorf("ob abort: %v, want a pointer to ob rollback", err)
	}
	for _, flag := range []string{"-y", "--yes"} {
		if _, err := run(t, dir, "deploy", flag); err == nil || !strings.Contains(err.Error(), "--yes was removed") || !strings.Contains(err.Error(), "ob deploy --plan") {
			t.Errorf("ob deploy %s: %v, want the removal note and the unattended path", flag, err)
		}
	}
	if _, err := run(t, dir, "bogus"); err == nil || !strings.Contains(err.Error(), `unknown command "bogus"`) {
		t.Errorf("an unknown command lost its plain error: %v", err)
	}
}
