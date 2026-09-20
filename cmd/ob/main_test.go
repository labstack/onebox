package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mainTestProject = `api_version: onebox.run/v1
app: demo
environments: {production: {server: deploy@example.invalid}}
image: nginx:1.27
proxy: {kind: none}
`

const mainTestBuildProject = `api_version: onebox.run/v1
app: demo
environments: {production: {server: deploy@example.invalid}}
workloads:
  api:
    role: application
    build: {context: ., dockerfile: Dockerfile}
proxy: {kind: none}
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
