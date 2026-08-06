package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/release"
)

// engineFromReleaseSnapshot returns a shallow engine copy whose deployment
// choreography comes from the immutable snapshot staged with releaseID.
// Recovery must never substitute the working-tree config: it may describe a
// different set of roles, ordering, strategies, hooks, or verification checks.
func (e *Engine) engineFromReleaseSnapshot(ctx context.Context, releaseID string) (*Engine, error) {
	names := e.names()
	environment := e.Opts.Environment
	if environment == "" {
		environment = e.Spec.Env
	}
	path := release.PathsFor(names).Releases + "/" + releaseID + "/ob.snapshot.yml"
	res, err := e.T.Run(ctx, "cat "+q(path))
	if err != nil {
		return nil, fmt.Errorf("read release %s snapshot: %w", releaseID, err)
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail != "" {
			detail = ": " + detail
		}
		return nil, fmt.Errorf("recovery refused: release %s snapshot unavailable (exit %d)%s", releaseID, res.ExitCode, detail)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return nil, fmt.Errorf("recovery refused: release %s snapshot is empty", releaseID)
	}

	snapshot, err := app.LoadBytes([]byte(res.Stdout), path)
	if err != nil {
		return nil, fmt.Errorf("recovery refused: release %s snapshot unusable: %w", releaseID, err)
	}
	resolved, err := snapshot.Resolve(environment)
	if err != nil {
		return nil, fmt.Errorf("recovery refused: release %s snapshot unusable: %w", releaseID, err)
	}
	if got := resolved.NamesFor(environment); got != names {
		return nil, fmt.Errorf("recovery refused: release %s snapshot resolves to app %q at %q, expected app %q at %q",
			releaseID, got.App, got.BasePath, names.App, names.BasePath)
	}

	replay := *e
	replay.Spec = resolved
	return &replay, nil
}
