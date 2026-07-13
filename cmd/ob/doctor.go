package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/agent"

	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/onebox"
)

const doctorReportSchemaVersion = "onebox.run/doctor-report/v1alpha1"

var errDoctorFailed = errors.New("doctor found failing checks")

type doctorStatus string

const (
	doctorPass    doctorStatus = "pass"
	doctorWarning doctorStatus = "warning"
	doctorFail    doctorStatus = "fail"
)

type doctorReport struct {
	SchemaVersion string                  `json:"schema_version"`
	Status        doctorStatus            `json:"status"`
	Binary        doctorBinaryReport      `json:"binary"`
	SSHAgent      doctorSSHAgentReport    `json:"ssh_agent"`
	Project       doctorProjectReport     `json:"project"`
	Approval      doctorApprovalReport    `json:"approval"`
	Protections   doctorProtectionsReport `json:"protections"`
}

type doctorBinaryReport struct {
	Status       doctorStatus            `json:"status"`
	Message      string                  `json:"message"`
	RunningPath  string                  `json:"running_path"`
	SelectedPath string                  `json:"selected_path"`
	Shadowed     bool                    `json:"shadowed"`
	Superseded   bool                    `json:"superseded"`
	Runner       buildinfo.Runner        `json:"runner"`
	Candidates   []doctorBinaryCandidate `json:"path_candidates"`
}

type doctorBinaryCandidate struct {
	Path              string         `json:"path"`
	Running           bool           `json:"running"`
	Selected          bool           `json:"selected"`
	Inspectable       bool           `json:"inspectable"`
	MetadataSource    string         `json:"metadata_source"`
	MetadataCaveat    string         `json:"metadata_caveat"`
	ReleaseVerified   bool           `json:"release_verified"`
	BuildTimeVerified bool           `json:"build_time_verified"`
	Stale             bool           `json:"stale"`
	StaleReason       string         `json:"stale_reason"`
	SupersedesRunning bool           `json:"supersedes_running"`
	SupersedesReason  string         `json:"supersedes_reason"`
	InspectionError   string         `json:"inspection_error"`
	Provenance        buildinfo.Info `json:"provenance"`
}

type doctorSSHAgentReport struct {
	Status     doctorStatus `json:"status"`
	Message    string       `json:"message"`
	Socket     string       `json:"socket"`
	Available  bool         `json:"available"`
	Identities int          `json:"identities"`
}

type doctorProjectReport struct {
	Status               doctorStatus `json:"status"`
	Message              string       `json:"message"`
	Path                 string       `json:"path"`
	Environment          string       `json:"environment"`
	Found                bool         `json:"found"`
	Valid                bool         `json:"valid"`
	Compatible           bool         `json:"compatible"`
	APIVersion           string       `json:"api_version"`
	Application          string       `json:"application"`
	MinimumOneboxVersion string       `json:"minimum_onebox_version"`
	MinimumPlanSchema    string       `json:"minimum_plan_schema"`
}

type doctorApprovalReport struct {
	Status             doctorStatus `json:"status"`
	Message            string       `json:"message"`
	PolicyKnown        bool         `json:"policy_known"`
	Required           bool         `json:"required"`
	Available          bool         `json:"available"`
	Source             string       `json:"source"`
	GrantSchemaVersion string       `json:"grant_schema_version"`
}

type doctorProtectionsReport struct {
	Status  doctorStatus            `json:"status"`
	Message string                  `json:"message"`
	Checks  []doctorProtectionCheck `json:"checks"`
}

type doctorProtectionCheck struct {
	Status    doctorStatus `json:"status"`
	Component string       `json:"component"`
	Mechanism string       `json:"mechanism"`
	Available bool         `json:"available"`
	Message   string       `json:"message"`
}

type doctorDependencies struct {
	runner        buildinfo.Runner
	executable    func() (string, error)
	lookPath      func(string) (string, error)
	pathValue     string
	sshSocket     string
	stat          func(string) (os.FileInfo, error)
	inspectBinary func(string) (buildinfo.Info, error)
	querySSHAgent func(context.Context, string) (int, error)
	loadConfig    func(string) (*config.Config, error)
}

var newDoctorDependencies = defaultDoctorDependencies

func defaultDoctorDependencies() doctorDependencies {
	return doctorDependencies{
		runner:        onebox.CurrentRunnerProvenance(),
		executable:    os.Executable,
		lookPath:      exec.LookPath,
		pathValue:     os.Getenv("PATH"),
		sshSocket:     os.Getenv("SSH_AUTH_SOCK"),
		stat:          os.Stat,
		inspectBinary: buildinfo.ReadFile,
		querySSHAgent: queryLocalSSHAgent,
		loadConfig:    config.Load,
	}
}

func addDoctorCommand(root *cobra.Command, g *globalFlags) {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "check local runner provenance and deployment safety capabilities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := buildDoctorReport(cmd.Context(), g, newDoctorDependencies())
			mode := g.Output
			if jsonOutput {
				mode = "json"
			}
			if err := writeDoctorReport(cmd.OutOrStdout(), report, mode); err != nil {
				return err
			}
			if report.Status == doctorFail {
				return errDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the doctor report as JSON")
	root.AddCommand(cmd)
}

func buildDoctorReport(ctx context.Context, g *globalFlags, deps doctorDependencies) doctorReport {
	binary := inspectDoctorBinary(deps)
	sshAgent := inspectDoctorSSHAgent(ctx, deps)
	project, cfg, environment := inspectDoctorProject(g, deps)
	approval := inspectDoctorApproval(environment)
	protections := inspectDoctorProtections(cfg, project.Path, deps)
	report := doctorReport{
		SchemaVersion: doctorReportSchemaVersion,
		Status:        doctorPass,
		Binary:        binary,
		SSHAgent:      sshAgent,
		Project:       project,
		Approval:      approval,
		Protections:   protections,
	}
	for _, status := range []doctorStatus{binary.Status, sshAgent.Status, project.Status, approval.Status, protections.Status} {
		report.Status = worseDoctorStatus(report.Status, status)
	}
	return report
}

func inspectDoctorBinary(deps doctorDependencies) doctorBinaryReport {
	report := doctorBinaryReport{Status: doctorPass, Runner: deps.runner, Candidates: []doctorBinaryCandidate{}}
	var issues []string
	runningPath, err := deps.executable()
	if err != nil {
		report.Status = doctorFail
		issues = append(issues, "cannot resolve the running executable: "+err.Error())
	} else {
		report.RunningPath = absolutePath(runningPath)
	}
	if deps.runner.Version == "" {
		report.Status = doctorFail
		issues = append(issues, "runner version is missing")
	}
	if deps.runner.VCSRevision == "" {
		report.Status = worseDoctorStatus(report.Status, doctorWarning)
		issues = append(issues, "VCS revision is unavailable")
	}
	if deps.runner.BuildTime == "" {
		report.Status = worseDoctorStatus(report.Status, doctorWarning)
		issues = append(issues, "build time is unavailable")
	}
	if deps.runner.Dirty {
		report.Status = worseDoctorStatus(report.Status, doctorWarning)
		issues = append(issues, "the runner was built from a dirty checkout")
	}

	selected, lookupErr := deps.lookPath("ob")
	if lookupErr != nil {
		report.Status = worseDoctorStatus(report.Status, doctorWarning)
		issues = append(issues, "PATH does not select an ob executable")
	} else {
		report.SelectedPath = absolutePath(selected)
	}
	runningCanonical := canonicalPath(report.RunningPath)
	selectedCanonical := canonicalPath(report.SelectedPath)
	if runningCanonical != "" && selectedCanonical != "" && runningCanonical != selectedCanonical {
		report.Shadowed = true
		report.Status = worseDoctorStatus(report.Status, doctorWarning)
		issues = append(issues, "PATH selects a different ob executable than the running binary")
	}

	paths := discoverExecutableCandidates(deps.pathValue, "ob", deps.stat)
	if report.SelectedPath != "" && !containsCanonicalPath(paths, report.SelectedPath) {
		paths = append(paths, report.SelectedPath)
	}
	staleCount := 0
	newerCount := 0
	for _, path := range paths {
		candidate := doctorBinaryCandidate{
			Path:     path,
			Running:  runningCanonical != "" && canonicalPath(path) == runningCanonical,
			Selected: selectedCanonical != "" && canonicalPath(path) == selectedCanonical,
		}
		if candidate.Running {
			candidate.Inspectable = true
			candidate.MetadataSource = "running_process"
			candidate.ReleaseVerified = deps.runner.Version != ""
			candidate.BuildTimeVerified = deps.runner.BuildTime != ""
			candidate.Provenance = deps.runner.Info
		} else {
			candidate.MetadataSource = "go_build_info"
			candidate.MetadataCaveat = "linker-only Onebox release and build-time values are unavailable without executing the candidate"
			candidate.Provenance, err = deps.inspectBinary(path)
			if err != nil {
				candidate.InspectionError = err.Error()
			} else {
				candidate.Inspectable = true
			}
		}
		if candidate.Inspectable && !candidate.Running {
			relation, reason := compareBinaryCandidate(candidate.Provenance, deps.runner.Info)
			switch relation {
			case -1:
				candidate.Stale = true
				candidate.StaleReason = reason
				staleCount++
			case 1:
				candidate.SupersedesRunning = true
				candidate.SupersedesReason = reason
				newerCount++
			}
		}
		report.Candidates = append(report.Candidates, candidate)
	}
	if staleCount > 0 {
		report.Status = worseDoctorStatus(report.Status, doctorWarning)
		issues = append(issues, fmt.Sprintf("%d stale PATH candidate(s) found", staleCount))
	}
	if newerCount > 0 {
		report.Superseded = true
		report.Status = worseDoctorStatus(report.Status, doctorWarning)
		issues = append(issues, fmt.Sprintf("running binary is superseded by %d newer PATH candidate(s)", newerCount))
	}
	if len(issues) == 0 {
		report.Message = "running binary provenance and PATH selection are consistent"
	} else {
		report.Message = strings.Join(issues, "; ")
	}
	return report
}

func discoverExecutableCandidates(pathValue, name string, stat func(string) (os.FileInfo, error)) []string {
	seen := map[string]bool{}
	var candidates []string
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		path := absolutePath(filepath.Join(dir, name))
		info, err := stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		canonical := canonicalPath(path)
		if canonical == "" {
			canonical = path
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		candidates = append(candidates, path)
	}
	return candidates
}

func containsCanonicalPath(paths []string, target string) bool {
	target = canonicalPath(target)
	for _, path := range paths {
		if canonicalPath(path) == target {
			return true
		}
	}
	return false
}

func absolutePath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	path = absolutePath(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}
	return path
}

// compareBinaryCandidate returns -1 when candidate is older, 1 when it is
// newer, and 0 when the ordering is equal or cannot be established safely.
func compareBinaryCandidate(candidate, running buildinfo.Info) (int, string) {
	if candidate.VCSRevision == "" || running.VCSRevision == "" || candidate.VCSRevision == running.VCSRevision {
		return 0, ""
	}
	candidateTime, candidateErr := time.Parse(time.RFC3339Nano, candidate.VCSTime)
	runningTime, runningErr := time.Parse(time.RFC3339Nano, running.VCSTime)
	if candidateErr != nil || runningErr != nil {
		return 0, ""
	}
	if candidateTime.Before(runningTime) {
		return -1, "VCS revision has an older commit timestamp than the running binary"
	}
	if candidateTime.After(runningTime) {
		return 1, "VCS revision has a newer commit timestamp than the running binary"
	}
	return 0, ""
}

func inspectDoctorSSHAgent(ctx context.Context, deps doctorDependencies) doctorSSHAgentReport {
	report := doctorSSHAgentReport{Status: doctorWarning, Socket: deps.sshSocket}
	if strings.TrimSpace(deps.sshSocket) == "" {
		report.Message = "SSH_AUTH_SOCK is not set"
		return report
	}
	identities, err := deps.querySSHAgent(ctx, deps.sshSocket)
	if err != nil {
		report.Message = "SSH agent is not usable: " + err.Error()
		return report
	}
	report.Available = true
	report.Identities = identities
	if identities == 0 {
		report.Message = "SSH agent is reachable but has no identities"
		return report
	}
	report.Status = doctorPass
	report.Message = fmt.Sprintf("SSH agent is usable with %d identity(s)", identities)
	return report
}

func queryLocalSSHAgent(ctx context.Context, socket string) (int, error) {
	// This only queries the local Unix-domain agent socket; it never contacts a
	// deployment host or any other network service.
	dialer := net.Dialer{Timeout: time.Second}
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return 0, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	identities, err := agent.NewClient(connection).List()
	if err != nil {
		return 0, err
	}
	return len(identities), nil
}

func inspectDoctorProject(g *globalFlags, deps doctorDependencies) (doctorProjectReport, *config.Config, *config.Environment) {
	path := absolutePath(g.ConfigPath)
	report := doctorProjectReport{
		Status: doctorWarning, Path: path, Environment: g.Env,
	}
	cfg, err := deps.loadConfig(g.ConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.Message = "project config was not found; project policy checks were skipped"
			return report, nil, nil
		}
		report.Status = doctorFail
		report.Found = true
		report.Message = "project config is invalid: " + err.Error()
		return report, nil, nil
	}
	report.Found = true
	report.Valid = true
	report.APIVersion = cfg.APIVersion
	report.Application = cfg.App
	environment, err := cfg.Environment(g.Env)
	if err != nil {
		report.Status = doctorFail
		report.Message = err.Error()
		return report, cfg, nil
	}
	report.MinimumOneboxVersion = environment.Policy.MinimumOneboxVersion
	report.MinimumPlanSchema = environment.Policy.MinimumPlanSchema
	if err := onebox.CheckRunnerCompatibility(environment.Policy, deps.runner); err != nil {
		report.Status = doctorFail
		report.Message = err.Error()
		return report, cfg, &environment
	}
	report.Status = doctorPass
	report.Compatible = true
	report.Message = "project policy is compatible with this runner"
	return report, cfg, &environment
}

func inspectDoctorApproval(environment *config.Environment) doctorApprovalReport {
	report := doctorApprovalReport{
		Status:             doctorPass,
		Available:          true,
		Source:             onebox.ApprovalSourceLocalCLI,
		GrantSchemaVersion: onebox.ApprovalGrantSchemaVersion,
	}
	if environment == nil {
		report.Message = "local plan-bound approval grants are available; project policy was not resolved"
		return report
	}
	report.PolicyKnown = true
	report.Required = environment.Policy.ApprovalRequired()
	if report.Required {
		report.Message = "project requires approval and this runner can create plan-bound local grants"
	} else {
		report.Message = "project does not require approval; plan-bound local grants remain available"
	}
	return report
}

func inspectDoctorProtections(cfg *config.Config, configPath string, deps doctorDependencies) doctorProtectionsReport {
	report := doctorProtectionsReport{Status: doctorPass, Checks: []doctorProtectionCheck{}}
	if cfg == nil {
		report.Status = doctorWarning
		report.Message = "declared protection mechanisms were not evaluated because project config is unavailable"
		return report
	}

	componentNames := make([]string, 0, len(cfg.Components))
	for name := range cfg.Components {
		componentNames = append(componentNames, name)
	}
	sort.Strings(componentNames)
	for _, name := range componentNames {
		protection := cfg.Components[name].Protection
		if protection == nil {
			continue
		}
		if protection.Backup != nil {
			report.Checks = append(report.Checks, doctorProtectionCheck{
				Status: doctorFail, Component: name, Mechanism: "backup", Available: false,
				Message: "component backup schedule is declared, but Onebox has no scheduler or provider availability probe for it",
			})
		}
		if protection.RestoreDrill != nil {
			report.Checks = append(report.Checks, doctorProtectionCheck{
				Status: doctorFail, Component: name, Mechanism: "restore_drill", Available: false,
				Message: "component restore-drill schedule is declared, but Onebox has no scheduler or provider availability probe for it",
			})
		}
	}

	if cfg.Secrets != nil {
		source := cfg.Secrets.Sops
		if !filepath.IsAbs(source) {
			source = filepath.Join(filepath.Dir(configPath), source)
		}
		_, sourceErr := deps.stat(source)
		sopsPath, sopsErr := deps.lookPath("sops")
		check := doctorProtectionCheck{Component: "", Mechanism: "sops_secrets"}
		switch {
		case sourceErr != nil:
			check.Status = doctorFail
			check.Message = "declared SOPS source is unavailable: " + sourceErr.Error()
		case sopsErr != nil:
			check.Status = doctorFail
			check.Message = "secrets are declared, but the sops executable is unavailable on PATH"
		default:
			check.Status = doctorPass
			check.Available = true
			check.Message = "SOPS source and executable are available at " + sopsPath
		}
		report.Checks = append(report.Checks, check)
	}

	if len(cfg.Preflight) > 0 {
		check := doctorProtectionCheck{Mechanism: "runtime_preflight"}
		if err := cfg.RunPreflight(filepath.Dir(configPath)); err != nil {
			check.Status = doctorFail
			check.Message = err.Error()
		} else {
			check.Status = doctorPass
			check.Available = true
			check.Message = "all declared local preflight files and keys are available"
		}
		report.Checks = append(report.Checks, check)
	}

	for _, check := range report.Checks {
		report.Status = worseDoctorStatus(report.Status, check.Status)
	}
	if len(report.Checks) == 0 {
		report.Message = "no local protection prerequisites or unsupported protection policies are declared"
	} else if report.Status == doctorFail {
		report.Message = "one or more declared protection mechanisms are unavailable"
	} else {
		report.Message = "declared local protection mechanisms are available"
	}
	return report
}

func worseDoctorStatus(left, right doctorStatus) doctorStatus {
	rank := map[doctorStatus]int{doctorPass: 0, doctorWarning: 1, doctorFail: 2}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func writeDoctorReport(out io.Writer, report doctorReport, mode string) error {
	switch mode {
	case "json":
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	case "ndjson":
		return json.NewEncoder(out).Encode(report)
	default:
		_, err := io.WriteString(out, formatDoctorReport(report))
		return err
	}
}

func formatDoctorReport(report doctorReport) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Onebox doctor: %s\n", strings.ToUpper(string(report.Status)))
	fmt.Fprintf(&out, "%s binary: %s\n", doctorStatusLabel(report.Binary.Status), report.Binary.Message)
	fmt.Fprintf(&out, "  running:  %s\n", known(report.Binary.RunningPath))
	fmt.Fprintf(&out, "  selected: %s\n", known(report.Binary.SelectedPath))
	fmt.Fprintf(&out, "  version: %s  revision: %s  dirty: %t  built: %s\n",
		known(report.Binary.Runner.Version), known(report.Binary.Runner.VCSRevision), report.Binary.Runner.Dirty, known(report.Binary.Runner.BuildTime))
	for _, candidate := range report.Binary.Candidates {
		var markers []string
		if candidate.Running {
			markers = append(markers, "running")
		}
		if candidate.Selected {
			markers = append(markers, "selected")
		}
		if candidate.Stale {
			markers = append(markers, "stale")
		}
		if candidate.SupersedesRunning {
			markers = append(markers, "newer")
		}
		marker := ""
		if len(markers) > 0 {
			marker = " [" + strings.Join(markers, ", ") + "]"
		}
		fmt.Fprintf(&out, "  candidate: %s%s\n", candidate.Path, marker)
	}
	fmt.Fprintf(&out, "%s ssh-agent: %s\n", doctorStatusLabel(report.SSHAgent.Status), report.SSHAgent.Message)
	fmt.Fprintf(&out, "%s project: %s\n", doctorStatusLabel(report.Project.Status), report.Project.Message)
	fmt.Fprintf(&out, "  config: %s  environment: %s\n", report.Project.Path, report.Project.Environment)
	fmt.Fprintf(&out, "%s approval: %s\n", doctorStatusLabel(report.Approval.Status), report.Approval.Message)
	fmt.Fprintf(&out, "%s protections: %s\n", doctorStatusLabel(report.Protections.Status), report.Protections.Message)
	for _, check := range report.Protections.Checks {
		name := check.Mechanism
		if check.Component != "" {
			name = check.Component + "/" + name
		}
		fmt.Fprintf(&out, "  %s %s: %s\n", doctorStatusLabel(check.Status), name, check.Message)
	}
	return out.String()
}

func doctorStatusLabel(status doctorStatus) string {
	switch status {
	case doctorPass:
		return "[PASS]"
	case doctorFail:
		return "[FAIL]"
	default:
		return "[WARN]"
	}
}
