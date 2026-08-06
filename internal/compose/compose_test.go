package compose

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Interpolation in a referenced Compose source resolves from the project's
// declared environment files — and from nothing else.
//
// The runner's own environment must never reach it: a document that parsed one
// way on a developer's laptop and another way on the target would differ
// exactly where nobody is looking.
func TestInterpolationComesFromDeclaredFilesOnly(t *testing.T) {
	t.Setenv("OB_RUNNER_ONLY", "from-the-laptop")
	doc := []byte("services:\n  web:\n    image: nginx:${TAG}\n    environment:\n      FROM_RUNNER: ${OB_RUNNER_ONLY}\n")

	p, err := LoadBytes(context.Background(), doc, "probe", t.TempDir(), map[string]string{"TAG": "1.27"})
	if err != nil {
		t.Fatalf("declared value: %v", err)
	}
	if p.Services["web"].Image != "nginx:1.27" {
		t.Errorf("declared value did not reach interpolation: %q", p.Services["web"].Image)
	}
	if v := p.Services["web"].Environment["FROM_RUNNER"]; v != nil && *v != "" {
		t.Errorf("the runner's environment reached interpolation: %q", *v)
	}
}

// A required variable nobody supplies is the author's to fix, and the failure
// has to say so rather than reporting it as a defect in generated output.
func TestAnUnsatisfiedRequiredVariableIsAttributedToTheAuthor(t *testing.T) {
	doc := []byte("services:\n  web:\n    image: nginx:${TAG:?the tag must be set}\n")
	_, err := LoadBytes(context.Background(), doc, "probe", t.TempDir(), nil)
	if err == nil {
		t.Fatal("a required variable with no value must fail")
	}
	var interpolation *InterpolationError
	if !errors.As(err, &interpolation) {
		t.Fatalf("must be reported as an interpolation failure, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "runtime.env_files") {
		t.Errorf("the failure must name what feeds interpolation: %v", err)
	}
}
