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

const recreateDrainPollInterval = 250 * time.Millisecond

// RecreateRole replaces a role's containers in place: a stated brief gap, the
// mode for workers and anything that can't roll. Honors replicas —
// recreates the whole fleet at the desired count and gives each a clean slot
// name.
func (e *Engine) RecreateRole(ctx context.Context, roleName, remoteComposePath string) error {
	projectDir := filepath.Dir(remoteComposePath)
	return e.recreateRoleForRelease(ctx, roleName, remoteComposePath, projectDir, filepath.Base(projectDir))
}

// recreateRoleForRelease is the same guaranteed replacement with an explicit
// Compose project directory and release identity. Secret-generation Compose
// files live below a release, so deriving either from their parent directory
// would resolve release-relative files below the generation and mistake the
// opaque generation for the release label.
func (e *Engine) recreateRoleForRelease(ctx context.Context, roleName, remoteComposePath, remoteProjectDir, releaseID string) error {
	role := e.Spec.Workloads[roleName]
	svc := roleName
	cc := e.composeCmdForProject(remoteComposePath, remoteProjectDir)
	desired := role.Count()

	if err := e.pullBeforeRelease(ctx, svc, cc); err != nil {
		return err
	}
	// Signal before recreate whenever the contract declares a bounded drain wait.
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
			if err := e.waitForContainersExit(ctx, svc, ids, wait); err != nil {
				return err
			}
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

// waitForContainersExit gives the exact containers signalled above up to wait
// to finish. It observes only runtime lifecycle state: an exited or vanished
// container is done, while an unknown inspection failure aborts rather than
// pretending graceful shutdown succeeded.
func (e *Engine) waitForContainersExit(ctx context.Context, svc string, ids []string, wait time.Duration) error {
	// Consume successful waits directly. Options.Now may deliberately be fixed for
	// deterministic operation metadata while Wait still uses a real timer.
	remaining := wait
	pending := append([]string(nil), ids...)
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		stillRunning := pending[:0]
		for _, id := range pending {
			running, err := e.drainingContainerRunning(ctx, id)
			if err != nil {
				return fmt.Errorf("wait for %s drain: %w", svc, err)
			}
			if running {
				stillRunning = append(stillRunning, id)
			}
		}
		pending = stillRunning
		if len(pending) == 0 {
			return nil
		}
		if remaining <= 0 {
			return nil
		}
		delay := min(remaining, recreateDrainPollInterval)
		if err := e.Opts.Wait(ctx, delay); err != nil {
			return err
		}
		remaining -= delay
	}
	return nil
}

func (e *Engine) drainingContainerRunning(ctx context.Context, id string) (bool, error) {
	res, err := e.T.Run(ctx, "docker inspect -f '{{.State.Running}}' "+id)
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		message := strings.TrimSpace(res.Stderr)
		lower := strings.ToLower(message)
		if strings.Contains(lower, "no such object") || strings.Contains(lower, "no such container") {
			return false, nil
		}
		return false, fmt.Errorf("inspect draining container %s failed (exit %d): %s", id, res.ExitCode, message)
	}
	switch strings.TrimSpace(res.Stdout) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("inspect draining container %s returned an invalid running state", id)
	}
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
		"ONEBOX_APP="+e.Spec.Name,
		"ONEBOX_HOST="+e.T.Host(),
		"ONEBOX_SERVER="+e.T.Destination(), // OpenSSH user@host (IPv6 unbracketed)
		"ONEBOX_SSH_USER="+e.T.SSHUser(),
		"ONEBOX_SSH_PORT="+e.T.SSHPort(),
		"ONEBOX_SSH_JUMP="+e.T.SSHJump(), // empty when the target is reached directly
		"ONEBOX_RELEASE_DIR="+remoteReleaseDir,
		"ONEBOX_RELEASE_ID="+filepath.Base(remoteReleaseDir),
	)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("hook %s (local) failed: %w: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}
