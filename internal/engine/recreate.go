package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RecreateRole replaces a role's container in place: a stated brief gap, the
// mode for workers, singletons, and anything that can't run two copies.
func (e *Engine) RecreateRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Cfg.Roles[roleName]
	svc := role.Service
	cc := e.composeCmd(remoteComposePath)

	if res, err := e.T.Run(ctx, cc+" pull --quiet "+svc); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("pull %s: %v %s", svc, err, res.Stderr)
	}
	// bleed before recreate for non-TERM signals (TERM is what stop sends anyway)
	if role.Drain != nil && role.Drain.Wait > 0 && role.Drain.Signal != "" && role.Drain.Signal != "TERM" {
		if id, _ := e.containerID(ctx, svc); id != "" {
			_, _ = e.T.Run(ctx, "docker kill --signal="+role.Drain.Signal+" "+id)
			e.Opts.Sleep(time.Duration(role.Drain.Wait))
		}
	}
	if res, err := e.T.Run(ctx, cc+" up -d --no-deps --force-recreate "+svc); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("recreate %s: %v %s", svc, err, res.Stderr)
	}
	id, err := e.containerID(ctx, svc)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("recreate %s: no container after up", svc)
	}
	if role.Ready != nil {
		return e.waitHealth(ctx, id, "healthy", time.Duration(role.Ready.Within), time.Duration(role.Ready.Interval))
	}
	res, err := e.T.Run(ctx, "docker inspect -f '{{.State.Status}}' "+id)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(res.Stdout); s != "running" {
		return fmt.Errorf("recreate %s: container %s is %s", svc, id, s)
	}
	return nil
}

// RunHook executes a user hook verbatim (design §01: hooks are unplannable
// commands — the operator's own, same trust level as their shell) with
// compose env exported so `docker compose ...` targets this release.
// Nonzero exit halts the deploy — M0 has no migration gate, so the only safe
// behavior is to stop.
func (e *Engine) RunHook(ctx context.Context, name, remoteReleaseDir, remoteComposePath string) error {
	hook, ok := e.Cfg.Hooks[name]
	if !ok || hook == "" {
		return nil
	}
	e.logf("hook %s: %s", name, hook)
	cmd := "cd " + q(remoteReleaseDir) +
		" && COMPOSE_PROJECT_NAME=" + e.Cfg.App +
		" COMPOSE_FILE=" + q(remoteComposePath) + " " + hook
	res, err := e.T.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("hook %s failed (exit %d): %s", name, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
