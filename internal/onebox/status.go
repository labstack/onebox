package onebox

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/engine"
)

func sanitizeStatus(status engine.StatusSnapshot) engine.StatusSnapshot {
	status.RecordedRelease = safeStatusToken(status.RecordedRelease, "<invalid-release>")
	status.Warnings = append([]engine.StatusWarning(nil), status.Warnings...)
	for i := range status.Warnings {
		// Remote output is untrusted. Keep the component identity actionable but
		// never forward arbitrary transport/parser prose into an agent context.
		status.Warnings[i].Message = "observation unavailable; use the CLI for diagnostic detail"
	}
	status.Roles = append([]engine.StatusRole(nil), status.Roles...)
	for i := range status.Roles {
		status.Roles[i].Containers = sanitizeContainers(status.Roles[i].Containers)
		status.Roles[i].Issues = statusIssueCodes("role", status.Roles[i].Issues)
	}
	status.Services = append([]engine.StatusService(nil), status.Services...)
	for i := range status.Services {
		status.Services[i].Containers = sanitizeContainers(status.Services[i].Containers)
		status.Services[i].Issues = statusIssueCodes("service", status.Services[i].Issues)
	}
	if status.Incomplete != nil {
		incomplete := *status.Incomplete
		incomplete.DeployID = safeStatusToken(incomplete.DeployID, "<invalid-deployment>")
		incomplete.PreviousRelease = safeStatusToken(incomplete.PreviousRelease, "<invalid-release>")
		if !safeOperator.MatchString(incomplete.Operator) {
			incomplete.Operator = ""
		}
		if !safeGitSHA.MatchString(incomplete.GitSHA) {
			incomplete.GitSHA = ""
		}
		if _, err := time.Parse(time.RFC3339, incomplete.StartedAt); incomplete.StartedAt != "" && err != nil {
			incomplete.StartedAt = ""
		}
		completed := make([]string, 0, len(incomplete.Completed))
		for _, step := range incomplete.Completed {
			if safeStep.MatchString(step) {
				completed = append(completed, step)
			}
		}
		incomplete.Completed = completed
		status.Incomplete = &incomplete
	}
	if status.Proxy != nil {
		proxyCopy := *status.Proxy
		if proxyCopy.Container != nil {
			container := *proxyCopy.Container
			container.Release = safeStatusToken(container.Release, "<invalid-release>")
			proxyCopy.Container = &container
		}
		proxyCopy.Issues = statusIssueCodes("proxy", proxyCopy.Issues)
		apps := make([]string, 0, len(proxyCopy.RegisteredApps))
		for _, app := range proxyCopy.RegisteredApps {
			if safeApp.MatchString(app) {
				apps = append(apps, app)
			}
		}
		proxyCopy.RegisteredApps = apps
		proxyCopy.Certificates = append([]engine.StatusCertificate(nil), proxyCopy.Certificates...)
		for i := range proxyCopy.Certificates {
			if !safeDomain.MatchString(proxyCopy.Certificates[i].Domain) {
				proxyCopy.Certificates[i].Domain = "<invalid-domain>"
			}
		}
		status.Proxy = &proxyCopy
	}
	return status
}

var (
	safeStatusValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	safeOperator    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,63}$`)
	safeGitSHA      = regexp.MustCompile(`^[0-9a-f]{4,40}$`)
	safeImageID     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	safeStep        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	safeApp         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	safeDomain      = regexp.MustCompile(`^(?:\*\.)?[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

func safeStatusToken(value, invalid string) string {
	if value == "" || safeStatusValue.MatchString(value) {
		return value
	}
	return invalid
}

func sanitizeContainers(containers []engine.StatusContainer) []engine.StatusContainer {
	out := append([]engine.StatusContainer(nil), containers...)
	for i := range out {
		out[i].Release = safeStatusToken(out[i].Release, "<invalid-release>")
	}
	return out
}

func statusIssueCodes(component string, issues []string) []string {
	out := make([]string, 0, len(issues))
	seen := map[string]bool{}
	for _, issue := range issues {
		code := component + "_diverged"
		switch {
		case strings.HasPrefix(issue, "replica count is "):
			code = "replica_count_mismatch"
		case strings.Contains(issue, "no release is recorded"):
			code = "release_not_recorded"
		case strings.Contains(issue, "not onebox-deployed"):
			code = "unmanaged_container"
		case strings.Contains(issue, "runs release "):
			code = "release_mismatch"
		case strings.Contains(issue, "health is "), strings.Contains(issue, " is unhealthy"), strings.Contains(issue, " is starting"), strings.Contains(issue, " is down"):
			code = "container_health_unready"
		case issue == "not running":
			code = "not_running"
		case issue == "local and applied configuration hashes differ":
			code = "configuration_drift"
		case strings.HasPrefix(issue, "certificate renewal is overdue"):
			code = "certificate_renewal_overdue"
		case issue == "certificate store is unreadable":
			code = "certificate_store_unreadable"
		}
		if !seen[code] {
			seen[code] = true
			out = append(out, code)
		}
	}
	return out
}

func canonicalStatus(status engine.StatusSnapshot) engine.StatusSnapshot {
	status.CapturedAt = time.Time{}
	status.Warnings = append([]engine.StatusWarning(nil), status.Warnings...)
	for i := range status.Warnings {
		status.Warnings[i].Message = ""
	}
	if status.Proxy != nil {
		proxyCopy := *status.Proxy
		proxyCopy.Certificates = append([]engine.StatusCertificate(nil), status.Proxy.Certificates...)
		for i := range proxyCopy.Certificates {
			// DaysRemaining is a presentation countdown. NotAfter and the overdue
			// threshold state carry the operational fact without daily digest noise.
			proxyCopy.Certificates[i].DaysRemaining = 0
		}
		status.Proxy = &proxyCopy
	}
	return status
}

func statusDigest(status engine.StatusSnapshot) (string, error) {
	encoded, err := json.Marshal(canonicalStatus(status))
	if err != nil {
		return "", err
	}
	return engine.HashBytes(encoded), nil
}

func proposalPreconditions(status engine.StatusSnapshot, refreshedRelease string) (ProposalPreconditions, error) {
	digest, err := statusDigest(status)
	if err != nil {
		return ProposalPreconditions{}, err
	}
	preconditions := ProposalPreconditions{
		StatusComplete: status.Complete,
		StatusDigest:   digest,
		Blockers:       []string{},
	}
	if !status.Complete {
		preconditions.Blockers = append(preconditions.Blockers, "target status observation is incomplete")
	}
	if status.RecordedRelease != refreshedRelease {
		preconditions.Blockers = append(preconditions.Blockers, "current release changed while the proposal was being built")
	}
	if status.Incomplete != nil {
		preconditions.Blockers = append(preconditions.Blockers, fmt.Sprintf("deployment %s is incomplete", status.Incomplete.DeployID))
	}
	firstDeploy := refreshedRelease == ""
	if !firstDeploy {
		for _, role := range status.Roles {
			for _, issue := range role.Issues {
				preconditions.Blockers = append(preconditions.Blockers, fmt.Sprintf("role %s: %s", role.Name, issue))
			}
		}
	}
	for _, service := range status.Services {
		for _, issue := range service.Issues {
			preconditions.Blockers = append(preconditions.Blockers, fmt.Sprintf("service %s: %s", service.Name, issue))
		}
	}
	if status.Proxy != nil {
		for _, issue := range status.Proxy.Issues {
			preconditions.Blockers = append(preconditions.Blockers, "proxy: "+issue)
		}
	}
	if status.Diverged && len(preconditions.Blockers) == 0 && !firstDeploy {
		preconditions.Blockers = append(preconditions.Blockers, "target state is divergent")
	}
	preconditions.Ready = len(preconditions.Blockers) == 0
	return preconditions, nil
}
