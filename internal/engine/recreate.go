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

// RecreateRole replaces a role's containers in place: a stated brief gap, the
// mode for workers, singletons, and anything that can't roll. Honors replicas —
// recreates the whole fleet at the desired count and gives each a clean slot
// name.
func (e *Engine) RecreateRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Cfg.Roles[roleName]
	svc := role.Service
	cc := e.composeCmd(remoteComposePath)
	releaseID := filepath.Base(filepath.Dir(remoteComposePath))
	desired := role.Count()

	if res, err := e.mutate(ctx, cc+" pull --quiet "+svc); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("pull %s: %s", svc, res.Stderr)
	}
	// bleed before recreate for non-TERM signals (TERM is what stop sends anyway)
	if role.Drain != nil && role.Drain.Wait > 0 && role.Drain.Signal != "" && role.Drain.Signal != "TERM" {
		ids, _ := e.containerIDs(ctx, svc)
		for _, id := range ids {
			_, _ = e.mutate(ctx, "docker kill --signal="+role.Drain.Signal+" "+id)
		}
		if len(ids) > 0 {
			e.Opts.Sleep(time.Duration(role.Drain.Wait))
		}
	}
	scaleArg := ""
	if desired > 1 {
		scaleArg = fmt.Sprintf(" --scale %s=%d", svc, desired)
	}
	if res, err := e.mutate(ctx, cc+" up -d --no-deps --force-recreate"+scaleArg+" "+svc); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("recreate %s: %s", svc, res.Stderr)
	}
	ids, err := e.containerIDs(ctx, svc)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("recreate %s: no container after up", svc)
	}
	within, pollEvery := readyTiming(role)
	for _, id := range ids {
		if role.Ready != nil {
			if err := e.waitHealth(ctx, id, "healthy", within, pollEvery); err != nil {
				return err
			}
			continue
		}
		res, err := e.T.Run(ctx, "docker inspect -f '{{.State.Status}}' "+id)
		if err != nil {
			return err
		}
		if s := strings.TrimSpace(res.Stdout); s != "running" {
			return fmt.Errorf("recreate %s: container %s is %s", svc, id, s)
		}
	}
	return e.reslot(ctx, svc, releaseID, desired)
}

// RunHook executes a user hook verbatim (design §01: hooks are unplannable
// commands — the operator's own, same trust level as their shell). Host
// hooks get compose env exported so `docker compose ...` targets this
// release; local hooks run on the runner (publish-style steps) with OB_*
// env. Nonzero exit halts the deploy — no migration gate until M2, so the
// only safe behavior is to stop.
func (e *Engine) RunHook(ctx context.Context, name, remoteReleaseDir, remoteComposePath string) error {
	hook, ok := e.Cfg.Hooks[name]
	if !ok || hook.Run == "" {
		return nil
	}
	if hook.Local {
		st := e.ui.Step("hook "+name+" (local)", true)
		e.ui.Cmd("local", hook.Run) // the verbatim command: verbose only — the plan already lists it
		err := e.runLocalHook(ctx, name, hook.Run, remoteReleaseDir)
		st(err)
		return err
	}
	st := e.ui.Step("hook "+name, true)
	cmd := "cd " + q(remoteReleaseDir) +
		" && COMPOSE_PROJECT_NAME=" + e.Cfg.App +
		" COMPOSE_FILE=" + q(remoteComposePath) + " " + hook.Run
	res, err := e.mutate(ctx, cmd)
	if err != nil {
		st(err)
		return err
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("hook %s failed (exit %d): %s", name, res.ExitCode, strings.TrimSpace(res.Stderr))
		st(err)
		return err
	}
	st(nil)
	return nil
}

func (e *Engine) runLocalHook(ctx context.Context, name, run, remoteReleaseDir string) error {
	c := exec.CommandContext(ctx, "sh", "-c", run) // verbatim by design
	c.Dir = e.Opts.LocalDir
	c.Env = append(os.Environ(),
		"OB_APP="+e.Cfg.App,
		"OB_HOST="+e.T.Host(),
		"OB_TARGET="+e.T.Target(), // user@host — for ssh/rsync in hooks
		"OB_RELEASE_DIR="+remoteReleaseDir,
		"OB_RELEASE_ID="+filepath.Base(remoteReleaseDir),
	)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("hook %s (local) failed: %v: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}
