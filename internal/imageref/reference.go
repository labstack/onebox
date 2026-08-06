// Package imageref validates and rewrites container image references using the
// same grammar as the registry ecosystem.
package imageref

import (
	"fmt"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// Pattern is the registry reference grammar, exposed for the generated JSON
// schema. Validate remains the runtime authority because it also enforces
// constraints such as the maximum repository-name length.
var Pattern = reference.ReferenceRegexp

// Validate reports whether value is a complete named image reference.
func Validate(value string) error {
	parsed, err := reference.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid image reference %q: %w", value, err)
	}
	if _, ok := parsed.(reference.Named); !ok {
		return fmt.Errorf("invalid image reference %q: repository name is required", value)
	}
	return nil
}

// WithDigest replaces any tag or digest on value with digestValue. Parse does
// not normalize familiar names, so nginx stays nginx instead of being expanded
// to docker.io/library/nginx in plans and generated files.
func WithDigest(value, digestValue string) (string, error) {
	parsed, err := reference.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse image reference %q: %w", value, err)
	}
	named, ok := parsed.(reference.Named)
	if !ok {
		return "", fmt.Errorf("parse image reference %q: repository name is required", value)
	}
	parsedDigest, err := digest.Parse(digestValue)
	if err != nil {
		return "", fmt.Errorf("parse image digest %q: %w", digestValue, err)
	}
	pinned, err := reference.WithDigest(reference.TrimNamed(named), parsedDigest)
	if err != nil {
		return "", fmt.Errorf("pin image reference %q: %w", value, err)
	}
	return pinned.String(), nil
}
