package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/onebox"
)

func doctorTestDependencies(t *testing.T) doctorDependencies {
	t.Helper()
	root := t.TempDir()
	oldDir := filepath.Join(root, "old")
	currentDir := filepath.Join(root, "current")
	for _, dir := range []string{oldDir, currentDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ob"), []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldBinary := filepath.Join(oldDir, "ob")
	currentBinary := filepath.Join(currentDir, "ob")
	cfg := &app.Spec{
		APIVersion: "onebox.run/v1",
		Name:       "demo",
		Environments: map[string]app.Environment{
			"production": {
				Policy: app.Policy{
					RequireApproval:  true,
					MinOneboxVersion: "v2026.7.1",
					MinPlanSchema:    "onebox.run/executable-deploy-plan/v1alpha1",
				},
			},
		},
		Workloads: map[string]app.Workload{
			"database": {Persistence: &app.Persistence{Mode: "durable"}},
		},
	}
	runner := buildinfo.Runner{
		Info: buildinfo.Info{
			Version: "v2026.7.2", VCSRevision: "new-revision", Dirty: false,
			VCSTime: "2026-07-13T12:00:00Z", BuildTime: "2026-07-13T12:01:00Z", GoVersion: "go-test",
		},
		SupportedExecutablePlanSchemas: onebox.SupportedExecutableDeployPlanSchemas(),
	}
	return doctorDependencies{
		runner:     runner,
		executable: func() (string, error) { return currentBinary, nil },
		lookPath: func(name string) (string, error) {
			if name == "ob" {
				return oldBinary, nil
			}
			return "", errors.New("not found")
		},
		pathValue: oldDir + string(os.PathListSeparator) + currentDir,
		sshSocket: "/fixture/agent.sock",
		stat:      os.Stat,
		inspectBinary: func(path string) (buildinfo.Info, error) {
			if canonicalPath(path) == canonicalPath(oldBinary) {
				return buildinfo.Info{
					Version: "v2026.7.1", VCSRevision: "old-revision",
					VCSTime: "2026-07-01T12:00:00Z", GoVersion: "go-test",
				}, nil
			}
			return buildinfo.Info{}, errors.New("unexpected candidate")
		},
		querySSHAgent: func(context.Context, string) (int, error) { return 2, nil },
		loadConfig:    func(string) (*app.Spec, error) { return cfg, nil },
	}
}

func TestBuildDoctorReportFindsShadowingPolicyAndBackupGaps(t *testing.T) {
	deps := doctorTestDependencies(t)
	report := buildDoctorReport(context.Background(), &globalFlags{ConfigPath: "/project/ob.yml", Env: "production"}, deps)

	// Durable data with no backup is a warning, not a failure: the
	// configuration is sound, the risk is real, and refusing to run would be
	// the wrong answer to "you should copy this off the box".
	if report.Status != doctorWarning {
		t.Fatalf("unexpected report envelope: %+v", report)
	}
	if !report.Binary.Shadowed || len(report.Binary.Candidates) != 2 {
		t.Fatalf("binary report did not identify PATH shadowing: %+v", report.Binary)
	}
	if !report.Binary.Candidates[0].Selected || !report.Binary.Candidates[0].Stale {
		t.Fatalf("selected stale candidate not identified: %+v", report.Binary.Candidates)
	}
	if report.Binary.Candidates[0].ReleaseVerified || report.Binary.Candidates[0].BuildTimeVerified {
		t.Fatalf("static candidate release metadata must remain unverified: %+v", report.Binary.Candidates[0])
	}
	if report.SSHAgent.Status != doctorPass || report.SSHAgent.Identities != 2 {
		t.Fatalf("SSH agent report = %+v", report.SSHAgent)
	}
	if report.Project.Status != doctorPass || !report.Project.Compatible {
		t.Fatalf("project report = %+v", report.Project)
	}
	if !report.Approval.PolicyKnown || !report.Approval.Required || !report.Approval.Available {
		t.Fatalf("approval report = %+v", report.Approval)
	}
	// Durable data with nothing copying it off the box must be said out loud;
	// silence would read as approval.
	if report.Backups.Status != doctorWarning || len(report.Backups.Checks) != 1 {
		t.Fatalf("backup report = %+v", report.Backups)
	}
	for _, mechanism := range []string{"backup"} {
		found := false
		for _, check := range report.Backups.Checks {
			found = found || check.Mechanism == mechanism && !check.Available
		}
		if !found {
			t.Fatalf("missing unavailable %s check: %+v", mechanism, report.Backups.Checks)
		}
	}
}

func TestDoctorCommandOutputModes(t *testing.T) {
	deps := doctorTestDependencies(t)
	previous := newDoctorDependencies
	newDoctorDependencies = func() doctorDependencies { return deps }
	t.Cleanup(func() { newDoctorDependencies = previous })

	for name, args := range map[string][]string{
		"json output": {"-c", "/project/ob.yml", "--output", "json", "doctor"},
		"root json":   {"--output", "json", "-c", "/project/ob.yml", "doctor"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newRootCmd()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute doctor error = %v\n%s", err, out.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("structured doctor output polluted stderr: %q", stderr.String())
			}
			var envelope struct {
				SchemaVersion string       `json:"schema_version"`
				Command       string       `json:"command"`
				Outcome       string       `json:"outcome"`
				Data          doctorReport `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatalf("decode doctor output: %v\n%s", err, out.String())
			}
			if envelope.SchemaVersion != cliSchemaVersion || envelope.Command != "ob doctor" || envelope.Outcome != cliOutcomeSuccess || envelope.Data.Status != doctorWarning {
				t.Fatalf("doctor envelope = %+v", envelope)
			}
		})
	}
}

func TestDoctorHumanOutputNamesEveryDiagnosticArea(t *testing.T) {
	report := buildDoctorReport(context.Background(), &globalFlags{ConfigPath: "/project/ob.yml", Env: "production"}, doctorTestDependencies(t))
	out := formatDoctorReport(report)
	for _, want := range []string{
		"Onebox doctor: WARNING",
		"binary:",
		"ssh-agent:",
		"project:",
		"approval:",
		"backups:",
		"database/backup",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorReportsMissingProjectWithoutAborting(t *testing.T) {
	deps := doctorTestDependencies(t)
	deps.loadConfig = func(string) (*app.Spec, error) { return nil, os.ErrNotExist }
	report := buildDoctorReport(context.Background(), &globalFlags{ConfigPath: "missing.yml", Env: "production"}, deps)
	if report.Project.Status != doctorWarning || report.Project.Found {
		t.Fatalf("project report = %+v", report.Project)
	}
	if report.Backups.Status != doctorWarning {
		t.Fatalf("backup report = %+v", report.Backups)
	}
}

func TestDoctorReportsIncompatibleProjectPolicy(t *testing.T) {
	deps := doctorTestDependencies(t)
	deps.loadConfig = func(string) (*app.Spec, error) {
		return &app.Spec{
			APIVersion: "onebox.run/v1",
			Environments: map[string]app.Environment{
				"production": {Policy: app.Policy{MinOneboxVersion: "v2027.1.0"}},
			},
			Workloads: map[string]app.Workload{},
		}, nil
	}
	report := buildDoctorReport(context.Background(), &globalFlags{ConfigPath: "/project/ob.yml", Env: "production"}, deps)
	if report.Project.Status != doctorFail || report.Project.Compatible {
		t.Fatalf("project report = %+v", report.Project)
	}
	if !strings.Contains(report.Project.Message, "below environment minimum") {
		t.Fatalf("unexpected compatibility message: %s", report.Project.Message)
	}
}

func TestDoctorSSHAgentUnavailableAndEmpty(t *testing.T) {
	tests := []struct {
		name       string
		socket     string
		identities int
		err        error
		available  bool
	}{
		{name: "unset"},
		{name: "unusable", socket: "/agent.sock", err: errors.New("connection refused")},
		{name: "empty", socket: "/agent.sock", available: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := doctorTestDependencies(t)
			deps.sshSocket = tt.socket
			deps.querySSHAgent = func(context.Context, string) (int, error) { return tt.identities, tt.err }
			report := inspectDoctorSSHAgent(context.Background(), deps)
			if report.Status != doctorWarning || report.Available != tt.available {
				t.Fatalf("SSH agent report = %+v", report)
			}
		})
	}
}

func TestStaleCandidateDoesNotFlagSameRevision(t *testing.T) {
	running := buildinfo.Info{
		Version: "v2026.7.2", VCSRevision: "same", VCSTime: "2026-07-13T12:00:00Z",
	}
	candidate := buildinfo.Info{
		Version: "v2026.7.1-4-gdeadbeef", VCSRevision: "same", VCSTime: "2026-07-01T00:00:00Z",
	}
	if relation, reason := compareBinaryCandidate(candidate, running); relation != 0 {
		t.Fatalf("same VCS revision reported stale: %s", reason)
	}
}

func TestDoctorFlagsRunningBinarySupersededByLaterPATHCandidate(t *testing.T) {
	deps := doctorTestDependencies(t)
	paths := discoverExecutableCandidates(deps.pathValue, "ob", deps.stat)
	if len(paths) != 2 {
		t.Fatalf("fixture PATH candidates = %v", paths)
	}
	oldBinary, newBinary := paths[0], paths[1]
	deps.executable = func() (string, error) { return oldBinary, nil }
	deps.lookPath = func(name string) (string, error) {
		if name == "ob" {
			return oldBinary, nil
		}
		return "", errors.New("not found")
	}
	deps.runner.Info = buildinfo.Info{
		Version: "v2026.7.1", VCSRevision: "old-revision", VCSTime: "2026-07-01T12:00:00Z",
		BuildTime: "2026-07-01T12:01:00Z", GoVersion: "go-test",
	}
	deps.inspectBinary = func(path string) (buildinfo.Info, error) {
		if canonicalPath(path) != canonicalPath(newBinary) {
			return buildinfo.Info{}, errors.New("unexpected candidate")
		}
		return buildinfo.Info{
			Version: "v2026.7.2", VCSRevision: "new-revision", VCSTime: "2026-07-13T12:00:00Z", GoVersion: "go-test",
		}, nil
	}

	report := inspectDoctorBinary(deps)
	if report.Shadowed || !report.Superseded || report.Status != doctorWarning {
		t.Fatalf("binary report = %+v", report)
	}
	if !report.Candidates[0].Running || !report.Candidates[0].Selected {
		t.Fatalf("old selected candidate = %+v", report.Candidates[0])
	}
	if !report.Candidates[1].SupersedesRunning {
		t.Fatalf("newer candidate not identified: %+v", report.Candidates[1])
	}
}
