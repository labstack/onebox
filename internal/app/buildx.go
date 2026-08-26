package app

import (
	"context"
	"fmt"
	"strings"
)

// BuildxCapabilityCommand is deliberately local-only. Preflight must prove
// that the installed client can format an image manifest without contacting a
// registry (and consuming pull quota) or changing the host.
const BuildxCapabilityCommand = "docker buildx imagetools inspect --help"

// BuildxRemedy is shared by preflight and planning so a client-side failure is
// never presented as a registry problem.
const BuildxRemedy = "install or upgrade the Docker Buildx plugin on the server, then rerun ob preflight"

// BuildxCapabilityError means the server was reachable but its local Docker
// client cannot perform the digest-resolution operation planning requires.
type BuildxCapabilityError struct {
	Detail string
}

func (err *BuildxCapabilityError) Error() string { return err.Detail }

// CheckBuildxDigestSupport verifies the local CLI surface used by PinImages.
// It does not inspect an image and therefore never contacts a registry.
func CheckBuildxDigestSupport(ctx context.Context, run Runner) (string, error) {
	res, err := run.Run(ctx, BuildxCapabilityCommand)
	if err != nil {
		return "", err
	}
	output := strings.TrimSpace(strings.Join([]string{res.Stdout, res.Stderr}, "\n"))
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(firstLine(output))
		if detail == "" {
			detail = fmt.Sprintf("docker buildx imagetools inspect --help exited with status %d", res.ExitCode)
		}
		return "", &BuildxCapabilityError{Detail: "Docker Buildx is unavailable: " + detail}
	}
	if !strings.Contains(output, "--format") {
		return "", &BuildxCapabilityError{
			Detail: "Docker Buildx is incompatible: imagetools inspect does not advertise --format support",
		}
	}
	return "imagetools inspect --format available", nil
}
