package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RecreateRole replaces a role's containers in place: a stated brief gap, the
// mode for workers and anything that can't roll. Honors replicas —
// recreates the whole fleet at the desired count and gives each a clean slot
// name.
func (e *Engine) RecreateRole(ctx context.Context, roleName, remoteComposePath string) error {
	return e.recreateRoleForRelease(ctx, roleName, remoteComposePath, filepath.Base(filepath.Dir(remoteComposePath)))
}

// recreateRoleForRelease is the same guaranteed replacement with an explicit
// release identity. Secret-generation Compose files live below a release, so
// deriving the release label from their parent directory would mistake the
// opaque generation for the release and leave stable slots unverified.
func (e *Engine) recreateRoleForRelease(ctx context.Context, roleName, remoteComposePath, releaseID string) error {
	role := e.Spec.Workloads[roleName]
	svc := roleName
	cc := e.composeCmd(remoteComposePath)
	desired := role.Count()

	if err := e.pullBeforeRelease(ctx, svc, cc); err != nil {
		return err
	}
	// Signal before recreate whenever the contract declares a fixed drain wait.
	// Compose sends TERM during replacement too, but doing it only then skipped
	// drain.wait entirely and left recreate workers at Compose's default timeout.
	if wait := role.DrainWait(); role.Drain != nil && role.Drain.Wait != "" && wait > 0 {
		ids, err := e.containerIDs(ctx, svc)
		if err != nil {
			return err
		}
		for _, id := range ids {
			// A per-container non-zero exit (vanished container, or a misspelled
			// drain.signal that docker rejects) must not silently degrade every
			// recreate to an abrupt stop — surface it. A transport/fence error
			// aborts: recreating under a lost lock is never correct.
			if res, err := e.mutate(ctx, "docker kill --signal="+role.DrainSignal()+" "+id); err != nil {
				return err
			} else if res.ExitCode != 0 {
				e.warnf("drain signal %s to %s failed: %s", role.DrainSignal(), svc, strings.TrimSpace(res.Stderr))
			}
		}
		if len(ids) > 0 {
			e.Opts.Sleep(wait)
		}
	}
	scaleArg := ""
	if desired > 1 {
		scaleArg = fmt.Sprintf(" --scale %s=%d", svc, desired)
	}
	timeoutArg := fmt.Sprintf(" --timeout %d", role.StopGraceSeconds())
	if res, err := e.mutate(ctx, cc+" up -d --no-deps --force-recreate"+timeoutArg+scaleArg+" "+svc); err != nil {
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
	within, pollEvery := role.ReadyTiming()
	for _, id := range ids {
		if role.Health != nil {
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

// RunHook executes a user hook verbatim. Hooks are unplannable
// commands — the operator's own, same trust level as their shell). Release
// hooks get compose env exported so `docker compose ...` targets that release;
// the host-only bootstrap hook has no application Compose file. Local hooks run
// on the runner (publish-style steps) with OB_* env. A nonzero exit halts the
// deploy; migration-gate evaluation determines whether later automatic
// rollback remains safe.
func (e *Engine) RunHook(ctx context.Context, name, remoteReleaseDir, remoteComposePath string) error {
	hook, ok := e.Spec.Hooks[name]
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
	cmd := "cd " + q(remoteReleaseDir) + " && "
	if remoteComposePath != "" {
		cmd += "COMPOSE_PROJECT_NAME=" + e.Spec.Name +
			" COMPOSE_FILE=" + q(remoteComposePath) + " "
	}
	cmd += hook.Run
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
		"OB_APP="+e.Spec.Name,
		"OB_HOST="+e.T.Host(),
		"OB_SERVER="+e.T.Destination(), // OpenSSH user@host (IPv6 unbracketed)
		"OB_SSH_USER="+e.T.SSHUser(),
		"OB_SSH_PORT="+e.T.SSHPort(),
		"OB_SSH_JUMP="+e.T.SSHJump(), // empty when the target is reached directly
		"OB_RELEASE_DIR="+remoteReleaseDir,
		"OB_RELEASE_ID="+filepath.Base(remoteReleaseDir),
	)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("hook %s (local) failed: %w: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}
