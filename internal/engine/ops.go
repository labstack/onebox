package engine

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/labstack/yeet/internal/release"
)

// Destroy tears the app down: containers via compose, then yeet's own state
// dir. Volumes survive unless removeVolumes — data loss is opt-in.
func (e *Engine) Destroy(ctx context.Context, removeVolumes bool) error {
	epoch, err := e.AcquireLock(ctx, "destroy", e.Opts.ForceLock)
	if err != nil {
		return err
	}
	if err := e.WriteFence(ctx, "destroy", epoch); err != nil {
		return err
	}
	cur, err := release.Current(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	if cur != "" {
		down := e.composeCmd(release.PathsFor(e.Cfg.App).Releases+"/"+cur+"/compose.yaml") + " down --remove-orphans"
		if removeVolumes {
			down += " -v"
		}
		if res, err := e.mutate(ctx, down); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("compose down: %s", strings.TrimSpace(res.Stderr))
		}
	} else {
		// no release ever activated: sweep by project label
		ids, err := e.T.Run(ctx, "docker ps -aq --filter label=com.docker.compose.project="+e.Cfg.App)
		if err != nil {
			return err
		}
		for _, id := range strings.Fields(ids.Stdout) {
			if validID.MatchString(id) {
				_, _ = e.mutate(ctx, "docker rm -f "+id)
			}
		}
	}
	// state dir last (takes the lock, fence, and journals with it — that is
	// the point of destroy)
	if res, err := e.mutate(ctx, "rm -rf "+q(release.PathsFor(e.Cfg.App).Base)); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("remove state dir: %s", res.Stderr)
	}
	e.logf("destroyed %s (volumes %s)", e.Cfg.App, map[bool]string{true: "REMOVED", false: "kept"}[removeVolumes])
	return nil
}

// Logs streams compose logs for one role/service (or all) from the current
// release.
func (e *Engine) Logs(ctx context.Context, name string, follow bool, tail int, out io.Writer) error {
	cur, err := release.Current(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	if cur == "" {
		return fmt.Errorf("nothing deployed")
	}
	svc, err := e.resolveService(name)
	if err != nil {
		return err
	}
	cmd := e.composeCmd(release.PathsFor(e.Cfg.App).Releases+"/"+cur+"/compose.yaml") + " logs --tail " + strconv.Itoa(tail)
	if follow {
		cmd += " --follow"
	}
	if svc != "" {
		cmd += " " + svc
	}
	return e.T.RunStream(ctx, cmd, out)
}

// ExecIn runs a command inside a role's container — the operator's own
// command, verbatim (same trust level as their shell).
func (e *Engine) ExecIn(ctx context.Context, name, command string, out io.Writer) error {
	svc, err := e.resolveService(name)
	if err != nil {
		return err
	}
	if svc == "" {
		return fmt.Errorf("exec needs a role or service name")
	}
	id, err := e.containerID(ctx, svc)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("%s: no running container", svc)
	}
	return e.T.RunStream(ctx, "docker exec "+id+" sh -c "+q(command), out)
}

// resolveService maps a role name to its compose service; a raw service or
// accessory name passes through; empty means "all" (logs only).
func (e *Engine) resolveService(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if r, ok := e.Cfg.Roles[name]; ok {
		return r.Service, nil
	}
	if _, ok := e.Project.Services[name]; ok {
		return name, nil
	}
	return "", fmt.Errorf("%q is neither a role nor a compose service", name)
}
