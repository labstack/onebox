package release

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// MountedReleases reports every release whose directory a container of this app
// still has mounted. A container's bind mounts are resolved against the release
// directory it was created in and keep pointing there for its whole life, so
// deleting that directory unlinks files out from under a running container —
// and a container that is merely stopped re-resolves the same paths on its next
// start, where a missing bind source is silently recreated as an empty
// directory. That is why this reads `-a` rather than only what is running.
//
// The mounts are the evidence, not the ob.release label. A retained container
// keeps the label of the release it was created in, and most retained workloads
// mount nothing out of that release — trusting the label would pin a directory
// nothing reads, for as long as that container lives, and quietly stop
// retain_releases from bounding the store.
//
// A path that is not inside the release store, or that names something which is
// not a release id, contributes nothing: the store only ever offers valid ids as
// deletion candidates, so such a mount can protect nothing.
func MountedReleases(ctx context.Context, target transport.Transport, names app.Names) ([]string, error) {
	command := "docker ps -a --no-trunc --filter label=ob.app=" + q(names.App) + " --format '{{.Mounts}}'"
	result, err := target.Run(ctx, command)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("inspect container release mounts failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	prefix := PathsFor(names).Releases + "/"
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		for _, source := range strings.Split(strings.TrimSpace(line), ",") {
			rest, inStore := strings.CutPrefix(strings.TrimSpace(source), prefix)
			if !inStore {
				continue
			}
			id, _, _ := strings.Cut(rest, "/")
			if !IsID(id) || seen[id] {
				continue
			}
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}
