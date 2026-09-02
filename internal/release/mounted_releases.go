package release

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// MountedReleases reports every release id a container of this app still refers
// to. A container's bind mounts are resolved against the release directory it
// was created in and keep pointing there for its whole life, so deleting that
// directory unlinks files out from under a running container — and a container
// that is merely stopped re-resolves the same paths on its next start, where a
// missing bind source is silently recreated as an empty directory. That is why
// this reads `-a` rather than only what is running.
//
// A container with no ob.release label contributes nothing: it predates the
// label or is not ours to reason about, and neither is evidence that a release
// is in use.
func MountedReleases(ctx context.Context, target transport.Transport, names app.Names) ([]string, error) {
	command := "docker ps -a --filter label=ob.app=" + q(names.App) + " --format '{{.Label \"ob.release\"}}'"
	result, err := target.Run(ctx, command)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("inspect container release mounts failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if !IsID(id) {
			return nil, fmt.Errorf("container reported invalid release id %q", id)
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}
