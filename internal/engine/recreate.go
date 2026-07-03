package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RecreateRole replaces a role's container in place: a stated brief gap, the
// mode for workers, singletons, and anything that can't run two copies.
func (e *Engine) RecreateRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Cfg.Roles[roleName]
	svc := role.Service
	cc := e.composeCmd(remoteComposePath)

	if res, err := e.mutate(ctx, cc+" pull --quiet "+svc); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("pull %s: %s", svc, res.Stderr)
	}
	// bleed before recreate for non-TERM signals (TERM is what stop sends anyway)
	if role.Drain != nil && role.Drain.Wait > 0 && role.Drain.Signal != "" && role.Drain.Signal != "TERM" {
		if id, _ := e.containerID(ctx, svc); id != "" {
			_, _ = e.mutate(ctx, "docker kill --signal="+role.Drain.Signal+" "+id)
			e.Opts.Sleep(time.Duration(role.Drain.Wait))
		}
	}
	if res, err := e.mutate(ctx, cc+" up -d --no-deps --force-recreate "+svc); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("recreate %s: %s", svc, res.Stderr)
	}
	id, err := e.containerID(ctx, svc)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("recreate %s: no container after up", svc)
	}
	if role.Ready != nil {
		within, pollEvery := readyTiming(role)
		return e.waitHealth(ctx, id, "healthy", within, pollEvery)
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
// commands — the operator's own, same trust level as their shell). Host
// hooks get compose env exported so `docker compose ...` targets this
// release; local hooks run on the runner (publish-style steps) with YEET_*
// env. Nonzero exit halts the deploy — no migration gate until M2, so the
// only safe behavior is to stop.
func (e *Engine) RunHook(ctx context.Context, name, remoteReleaseDir, remoteComposePath string) error {
	hook, ok := e.Cfg.Hooks[name]
	if !ok || hook.Run == "" {
		return nil
	}
	if hook.Local {
		e.logf("hook %s (local): %s", name, hook.Run)
		return e.runLocalHook(ctx, name, hook.Run, remoteReleaseDir)
	}
	e.logf("hook %s: %s", name, hook.Run)
	cmd := "cd " + q(remoteReleaseDir) +
		" && COMPOSE_PROJECT_NAME=" + e.Cfg.App +
		" COMPOSE_FILE=" + q(remoteComposePath) + " " + hook.Run
	res, err := e.mutate(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("hook %s failed (exit %d): %s", name, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (e *Engine) runLocalHook(ctx context.Context, name, run, remoteReleaseDir string) error {
	c := exec.CommandContext(ctx, "sh", "-c", run) // verbatim by design
	c.Dir = e.Opts.LocalDir
	c.Env = append(os.Environ(),
		"YEET_APP="+e.Cfg.App,
		"YEET_HOST="+e.T.Host(),
		"YEET_RELEASE_DIR="+remoteReleaseDir,
		"YEET_RELEASE_ID="+filepath.Base(remoteReleaseDir),
	)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("hook %s (local) failed: %v: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}
