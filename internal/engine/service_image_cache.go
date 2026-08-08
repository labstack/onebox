package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/distribution/reference"
	"github.com/labstack/onebox/internal/imageref"
	"github.com/labstack/onebox/internal/transport"
)

// ExactServiceImageCached verifies Docker has the requested immutable image
// and that Docker associates it with the requested manifest digest. Docker's
// .Id is a configuration-object digest, not the manifest/index digest carried
// by an OCI @sha256 reference, so only RepoDigests are acceptable evidence.
func (e *Engine) ExactServiceImageCached(ctx context.Context, image string) (bool, error) {
	return ExactServiceImageCached(ctx, e.T, image)
}

// ExactServiceImageCached performs the same observation before an Engine can
// be constructed, allowing lifecycle state to be injected before rendering.
func ExactServiceImageCached(ctx context.Context, target transport.Transport, image string) (bool, error) {
	if target == nil {
		return false, errors.New("service image cache target is nil")
	}
	if err := imageref.Validate(image); err != nil {
		return false, err
	}
	separator := strings.LastIndex(image, "@sha256:")
	if separator < 1 || len(image)-separator != len("@sha256:")+64 {
		return false, errors.New("service image cache check requires an immutable sha256 reference")
	}
	digest := image[separator+1:]
	expected, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false, err
	}
	expectedReference := expected.String()
	result, err := target.Run(ctx, "docker image inspect --format '{{json .RepoDigests}}' "+q(image))
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	var repoDigests []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &repoDigests); err != nil {
		return false, errors.New("docker returned invalid repository digest metadata")
	}
	for _, repoDigest := range repoDigests {
		observed, err := reference.ParseNormalizedNamed(repoDigest)
		if err == nil && observed.String() == expectedReference && strings.HasSuffix(repoDigest, "@"+digest) {
			return true, nil
		}
	}
	return false, nil
}

// ServiceImageDigestAvailable observes the exact immutable manifest in the
// configured Docker registry context. It says availability only; publication
// provenance remains separately bound in durable lifecycle state.
func ServiceImageDigestAvailable(ctx context.Context, target transport.Transport, image string) (bool, error) {
	if target == nil {
		return false, errors.New("service image registry target is nil")
	}
	if err := imageref.Validate(image); err != nil {
		return false, err
	}
	separator := strings.LastIndex(image, "@sha256:")
	if separator < 1 || len(image)-separator != len("@sha256:")+64 {
		return false, errors.New("service image registry check requires an immutable sha256 reference")
	}
	result, err := target.Run(ctx, "docker manifest inspect "+q(image)+" >/dev/null 2>&1")
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}
