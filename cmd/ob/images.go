package main

import (
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/imageref"
)

// parseImages reads repeated --image workload=reference pairs.
//
// Production never builds. A workload declaring `build:` therefore has no
// image until whatever built it says what it produced, and this is how that
// answer arrives — for plan and deploy as much as for preview and eject,
// which is where it was missing: the project could be inspected but not
// released.
func parseImages(pairs []string) (app.Images, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	images := app.Images{}
	for _, pair := range pairs {
		name, ref, ok := strings.Cut(pair, "=")
		if !ok || name == "" || ref == "" {
			return nil, fmt.Errorf("--image expects workload=reference, got %q", pair)
		}
		if err := imageref.Validate(ref); err != nil {
			return nil, fmt.Errorf("--image for workload %q: %w", name, err)
		}
		if previous, seen := images[name]; seen && previous != ref {
			return nil, fmt.Errorf("--image names %q twice, as %q and %q", name, previous, ref)
		}
		images[name] = ref
	}
	return images, nil
}
