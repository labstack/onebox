package engine

import (
	"context"
	"errors"
	"strings"

	"github.com/labstack/onebox/internal/imageref"
)

// ExactServiceImageCached verifies Docker has the requested immutable image
// and that its content ID equals the requested digest. A tag-only cache hit is
// never accepted as protected-runtime evidence.
func (e *Engine) ExactServiceImageCached(ctx context.Context, image string) (bool, error) {
	if err := imageref.Validate(image); err != nil {
		return false, err
	}
	separator := strings.LastIndex(image, "@sha256:")
	if separator < 1 || len(image)-separator != len("@sha256:")+64 {
		return false, errors.New("service image cache check requires an immutable sha256 reference")
	}
	digest := image[separator+1:]
	result, err := e.T.Run(ctx, "docker image inspect --format '{{.Id}}' "+q(image))
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	return strings.TrimSpace(result.Stdout) == digest, nil
}
