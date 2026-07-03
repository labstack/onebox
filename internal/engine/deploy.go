package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/release"
)

// Deploy runs the M0 lifecycle:
// preflight → transfer → pre-release → release → verify → finalize.
func (e *Engine) Deploy(ctx context.Context, releaseID, localStagingDir string) error {
	e.logf("phase preflight")
	if err := e.Preflight(ctx); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	e.logf("phase transfer (%s)", releaseID)
	remoteDir, err := release.Push(ctx, e.T, localStagingDir, e.Cfg.App, releaseID)
	if err != nil {
		return fmt.Errorf("transfer: %w", err)
	}
	remoteCompose := remoteDir + "/compose.yaml"

	e.logf("phase pre-release")
	if err := e.RunHook(ctx, "migrate", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}
	if err := e.RunHook(ctx, "pre_release", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}

	e.logf("phase release")
	if err := e.releaseRoles(ctx, remoteCompose); err != nil {
		return fmt.Errorf("release: %w (deploy halted — resume/abort are M2; fix and redeploy)", err)
	}
	if err := e.RunHook(ctx, "post_release", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("post-release: %w", err)
	}

	e.logf("phase verify")
	if err := e.Verify(ctx); err != nil {
		return fmt.Errorf("verify: %w (release NOT activated; previous release still current)", err)
	}

	e.logf("phase finalize")
	if err := release.Activate(ctx, e.T, e.Cfg.App, releaseID); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	removed, err := release.Prune(ctx, e.T, e.Cfg.App, e.Cfg.Retain)
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	if len(removed) > 0 {
		e.logf("pruned %d old releases", len(removed))
	}
	if err := e.RunHook(ctx, "post_deploy", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("post-deploy: %w", err)
	}
	e.logf("deployed %s", releaseID)
	return nil
}

func (e *Engine) releaseRoles(ctx context.Context, remoteCompose string) error {
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		e.logf("release %s (%s, %s)", roleName, role.Service, role.Mode)
		var err error
		if role.Mode == "rolling" {
			err = e.RollRole(ctx, roleName, remoteCompose)
		} else {
			err = e.RecreateRole(ctx, roleName, remoteCompose)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", roleName, err)
		}
	}
	return nil
}

// Rollback re-releases the previous release dir: its compose.yaml pins the
// old image locally (design §06 "rollback never pulls" — images retained by
// Prune's window), and its own yeet.yml snapshot drives the choreography —
// old release, old config, old modes.
func (e *Engine) Rollback(ctx context.Context) error {
	prev, err := release.Previous(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	prevDir := release.PathsFor(e.Cfg.App).Releases + "/" + prev
	remoteCompose := prevDir + "/compose.yaml"

	// replay engine: the snapshot's choreography when available
	replay := e
	res, err := e.T.Run(ctx, "cat "+q(prevDir+"/yeet.snapshot.yml"))
	if err != nil {
		return err
	}
	if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
		snapCfg, serr := config.LoadBytes([]byte(res.Stdout), prev+"/yeet.snapshot.yml")
		if serr == nil {
			serr = snapCfg.Validate()
		}
		if serr != nil {
			e.logf("warn: snapshot unusable (%v) — replaying with CURRENT yeet.yml choreography", serr)
		} else {
			cp := *e
			cp.Cfg = snapCfg
			replay = &cp
		}
	} else {
		e.logf("warn: no yeet.snapshot.yml in %s (pre-M1 release?) — replaying with CURRENT yeet.yml choreography", prev)
	}

	e.logf("rolling back to %s", prev)
	if err := replay.releaseRoles(ctx, remoteCompose); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	if err := replay.Verify(ctx); err != nil {
		return fmt.Errorf("rollback verify: %w", err)
	}
	return release.Activate(ctx, e.T, e.Cfg.App, prev)
}
