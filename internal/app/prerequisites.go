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

const (
	runtimeRemedy = "install Docker on the server, or grant this account permission to use it"
	composeRemedy = "install the Docker Compose plugin on the server, then rerun ob preflight"
)

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
		return []Check{{
			Name:   PrerequisiteRuntime,
			Detail: strings.TrimSpace(firstLine(strings.Join([]string{res.Stderr, res.Stdout}, "\n"))),
			Remedy: runtimeRemedy,
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

// HostPrerequisiteError reports the first unmet prerequisite to a caller that
// wants a refusal rather than a report.
type HostPrerequisiteError struct {
	Check Check
}

func (err *HostPrerequisiteError) Error() string {
	if err.Check.Remedy == "" {
		return err.Check.Name + " unavailable: " + err.Check.Detail
	}
	return fmt.Sprintf("%s unavailable: %s — %s", err.Check.Name, err.Check.Detail, err.Check.Remedy)
}

// RequireHostPrerequisites is the refusing form of CheckHostPrerequisites, for
// bootstrap and the deploy preflight step, which stop at the first problem
// instead of rendering a report.
func RequireHostPrerequisites(ctx context.Context, run Runner) error {
	checks, err := CheckHostPrerequisites(ctx, run)
	if err != nil {
		return err
	}
	for _, check := range checks {
		if !check.OK {
			return &HostPrerequisiteError{Check: check}
		}
	}
	return nil
}
