package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The host software Onebox requires and deliberately never installs. Declaring
// the set in one place is what keeps the four gates that assert it — bootstrap,
// `ob preflight`, `ob doctor` and the deploy preflight step — from each
// checking a different subset, which let a host pass bootstrap and fail two
// commands later.
const (
	dockerVersionCommand  = "docker version --format '{{.Server.Version}}'"
	composeVersionCommand = "docker compose version --short"
	buildxVersionCommand  = "docker buildx version"
)

// Prerequisite names are stable: they appear in `ob preflight` output, in
// `ob doctor`, and in the refusal bootstrap raises, so an operator reading any
// of the three sees the same vocabulary.
const (
	PrerequisiteRuntime  = "container runtime"
	PrerequisiteCompose  = "compose plugin"
	PrerequisiteResolver = "image resolver"
)

// A refusal that names no command is a dead end at the worst moment: first
// contact with a fresh host. Each remedy is an action, and the three causes a
// failing `docker version` actually has are distinguished, because "install
// Docker" is the wrong advice for the common case where Docker is installed and
// the deploy account simply cannot reach its socket.
const (
	prerequisiteDocs = "https://onebox.run/start/install"

	runtimeAbsentRemedy      = "install Docker Engine, the Compose plugin and Buildx on the server (" + prerequisiteDocs + "), then rerun ob preflight"
	runtimeDeniedRemedy      = "add the deploy account to the docker group on the server and reconnect so the new membership applies, then rerun ob preflight"
	runtimeUnreachableRemedy = "start the Docker daemon on the server, then rerun ob preflight"
	composeRemedy            = "install the Docker Compose plugin on the server (" + prerequisiteDocs + "), then rerun ob preflight"
)

// runtimeRemedyFor reads the cause out of what the runtime said. Docker reports
// all three through the same non-zero exit, and only the text separates them.
func runtimeRemedyFor(detail string) string {
	lowered := strings.ToLower(detail)
	switch {
	case strings.Contains(lowered, "permission denied"):
		return runtimeDeniedRemedy
	case strings.Contains(lowered, "cannot connect to the docker daemon"),
		strings.Contains(lowered, "is the docker daemon running"):
		return runtimeUnreachableRemedy
	default:
		return runtimeAbsentRemedy
	}
}

// CheckHostPrerequisites asks the server for every piece of host software a
// deploy needs, and reports each as a Check rather than as an error, so a
// caller sees the whole set at once.
//
// An error is returned only when the server cannot be reached at all. A
// container runtime that is absent or unusable short-circuits the rest: without
// it the remaining answers are noise, not diagnosis.
//
// Nothing here mutates, and nothing contacts a registry.
func CheckHostPrerequisites(ctx context.Context, run Runner) ([]Check, error) {
	res, err := run.Run(ctx, dockerVersionCommand)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(firstLine(strings.Join([]string{res.Stderr, res.Stdout}, "\n")))
		return []Check{{
			Name:   PrerequisiteRuntime,
			Detail: detail,
			Remedy: runtimeRemedyFor(detail),
		}}, nil
	}
	checks := []Check{{
		Name: PrerequisiteRuntime, OK: true,
		Detail: "docker " + strings.TrimSpace(res.Stdout),
	}}

	// Compose is what actually applies a release. It was previously asserted
	// only by the deploy step, so `ob preflight` reported a host ready that
	// `ob deploy` then refused.
	res, err = run.Run(ctx, composeVersionCommand)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		checks = append(checks, Check{
			Name:   PrerequisiteCompose,
			Detail: strings.TrimSpace(firstLine(strings.Join([]string{res.Stderr, res.Stdout}, "\n"))),
			Remedy: composeRemedy,
		})
	} else {
		checks = append(checks, Check{
			Name: PrerequisiteCompose, OK: true,
			Detail: "compose " + strings.TrimSpace(res.Stdout),
		})
	}

	detail, err := CheckBuildxDigestSupport(ctx, run)
	if err != nil {
		var capabilityErr *BuildxCapabilityError
		if !errors.As(err, &capabilityErr) {
			return nil, err
		}
		return append(checks, Check{
			Name:   PrerequisiteResolver,
			Detail: withBuildxVersion(ctx, run, capabilityErr.Error()),
			Remedy: BuildxRemedy,
		}), nil
	}
	return append(checks, Check{
		Name: PrerequisiteResolver, OK: true,
		Detail: withBuildxVersion(ctx, run, detail),
	}), nil
}

// withBuildxVersion appends the client version to a capability result. The
// capability probe is the authority — a client that advertises `--format` and
// ignores it passes a version comparison — but the version is what makes a bug
// report actionable, so both are reported.
func withBuildxVersion(ctx context.Context, run Runner, detail string) string {
	res, err := run.Run(ctx, buildxVersionCommand)
	if err != nil || res.ExitCode != 0 {
		return detail
	}
	version := strings.TrimSpace(firstLine(res.Stdout))
	if version == "" {
		return detail
	}
	return fmt.Sprintf("%s (%s)", detail, version)
}

// RequireHostPrerequisites is the refusing form of CheckHostPrerequisites, for
// bootstrap and the deploy preflight step, which stop at the first problem
// instead of rendering a report.
//
// The refusal is typed rather than prose, so a structured caller reads a code
// and a command rather than parsing a sentence, and `ob preflight` is the
// honest next step: it is read-only and reports every unmet prerequisite at
// once, where this path stops at the first. It is never circular — the only
// callers are bootstrap and the deploy step, not `ob preflight` itself.
func RequireHostPrerequisites(ctx context.Context, run Runner) error {
	checks, err := CheckHostPrerequisites(ctx, run)
	if err != nil {
		return err
	}
	for _, check := range checks {
		if check.OK {
			continue
		}
		if check.Remedy == "" {
			return errf("host_prerequisite_unmet", "", "ob preflight",
				"%s unavailable: %s", check.Name, check.Detail)
		}
		return errf("host_prerequisite_unmet", "", "ob preflight",
			"%s unavailable: %s — %s", check.Name, check.Detail, check.Remedy)
	}
	return nil
}
