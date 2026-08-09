package engine

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/release"
)

// Destroy tears the app down: containers via compose, then ob's own state
// dir. Volumes survive unless removeVolumes — data loss is opt-in. The shared
// managed proxy is refcounted: destroy deregisters this app; the proxy itself
// goes only with removeProxy AND an empty registry (it may serve other apps).
func (e *Engine) Destroy(ctx context.Context, removeVolumes, removeProxy bool) error {
	if removeProxy && !e.Spec.Proxy.Managed {
		return fmt.Errorf("--proxy: this app's proxy is not managed — nothing shared to remove")
	}
	hp := proxy.HostPaths(e.names())
	if removeProxy {
		// refuse BEFORE any teardown: other registered apps depend on it
		others, err := e.proxyRegistryOthers(ctx)
		if err != nil {
			return err
		}
		if len(others) > 0 {
			return fmt.Errorf("--proxy refused: the shared proxy still serves %s — destroy those apps first (or drop --proxy)",
				strings.Join(others, ", "))
		}
	}
	epoch, err := e.AcquireLock(ctx, "destroy", e.Opts.ForceLock)
	if err != nil {
		return err
	}
	if err := e.WriteFence(ctx, "destroy", epoch); err != nil {
		return err
	}
	cur, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return err
	}
	if cur != "" {
		down := e.composeCmd(release.PathsFor(e.names()).Releases+"/"+cur+"/compose.yaml") + " down --remove-orphans"
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
		ids, err := e.T.Run(ctx, "docker ps -aq --filter label=com.docker.compose.project="+q(e.Spec.Name))
		if err != nil {
			return err
		}
		for _, id := range strings.Fields(ids.Stdout) {
			if validID.MatchString(id) {
				// Fail closed, exactly as the service sweep does. Warning and
				// continuing would go on to delete the volumes, the schedules
				// and the state directory while the container is still alive,
				// and then report a clean teardown.
				if res, err := e.mutate(ctx, "docker rm -f "+id); err != nil {
					return err
				} else if res.ExitCode != 0 {
					return fmt.Errorf("cannot remove container %s: %s — nothing further was destroyed",
						id, strings.TrimSpace(res.Stderr))
				}
			}
		}
		// `docker rm -f` never removes named volumes — honor --volumes here
		// too (compose labels volumes with the project, same as containers)
		if removeVolumes {
			res, err := e.T.Run(ctx, "docker volume ls -q --filter label=com.docker.compose.project="+q(e.Spec.Name))
			if err != nil {
				return err
			}
			var vols []string
			for _, v := range strings.Fields(res.Stdout) {
				if volName.MatchString(v) { // volume names are never interpolated unvalidated
					vols = append(vols, v)
				}
			}
			if len(vols) > 0 {
				if res, err := e.mutate(ctx, "docker volume rm "+strings.Join(vols, " ")); err != nil || res.ExitCode != 0 {
					return fmt.Errorf("volume rm: %v %s", err, res.Stderr)
				}
			}
		}
	}
	// A timer outlives the release directory it invokes, so it has to be
	// removed explicitly or it fails every minute forever.
	if err := e.RemoveSchedules(ctx); err != nil {
		return err
	}
	// Supporting services run in their own Compose projects precisely so that
	// no release can stop them. Destroy is not a release: the app is going
	// away, and a database left listening with nothing to serve is not a
	// safety property, it is a leak. The volume still survives unless the
	// operator asked for it too.
	if err := e.removeServices(ctx, removeVolumes); err != nil {
		return err
	}
	// state dir last (takes the lock, fence, and journals with it — that is
	// the point of destroy)
	base := release.PathsFor(e.names()).Base
	sweep := "rm -rf " + q(base)
	keepingCredentials := !removeVolumes && len(e.Spec.Services) > 0
	if keepingCredentials {
		// A service credential is generated once, on the target, and exists
		// nowhere else. Deleting it while deliberately keeping the volume
		// leaves data nothing can ever open again: the next bootstrap
		// generates a fresh password, the database keeps the one baked into
		// its data directory, and the application fails to authenticate
		// against a service that reports itself perfectly healthy.
		//
		// So the key stays with the lock. Everything else — releases,
		// journals, locks, fences — goes.
		sweep = fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 ! -name services -exec rm -rf {} +", q(base))
	}
	if res, err := e.mutate(ctx, sweep); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("remove state dir: %s", res.Stderr)
	}
	if keepingCredentials {
		e.logf("kept %s — the service volumes survive and these credentials are the only thing that can open them", e.names().ServiceDir())
	}
	if e.Spec.Proxy.Managed {
		// host scope, after the app fence is gone: plain Run, under host lock
		if err := e.acquireHostLock(ctx, e.Opts.ForceLock); err != nil {
			return err
		}
		defer e.releaseHostLock(ctx)
		// The app state (and its fence) is intentionally gone; subsequent
		// mutations are protected solely by the host-scoped lock token.
		e.fenceVal = ""
		if res, err := e.hostMutate(ctx, "rm -f "+q(hp.Apps+"/"+e.Spec.Name)); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("deregister from proxy: %v %s", err, res.Stderr)
		}
		others, err := e.proxyRegistryOthers(ctx)
		if err != nil {
			return err
		}
		switch {
		case removeProxy && len(others) == 0:
			if res, err := e.hostMutate(ctx, "docker compose -p "+proxy.Project+" -f "+q(hp.Compose)+" down"); err != nil {
				return err
			} else if res.ExitCode != 0 {
				return fmt.Errorf("proxy down: %s", strings.TrimSpace(res.Stderr))
			}
			if res, err := e.hostMutate(ctx, "rm -rf "+q(hp.Dir)); err != nil || res.ExitCode != 0 {
				return fmt.Errorf("remove proxy dir: %v %s", err, res.Stderr)
			}
			e.logf("shared proxy removed (no apps remain)")
		case removeProxy:
			// raced: another app registered between the upfront check and now
			e.logf("--proxy skipped: %s registered with the shared proxy during teardown — it stays", strings.Join(others, ", "))
		case len(others) == 0:
			e.logf("shared proxy kept with no registered apps — `ob destroy --proxy` removes it, or clean %s manually", hp.Dir)
		}
	}
	e.logf("destroyed %s (volumes %s)", e.Spec.Name, map[bool]string{true: "REMOVED", false: "kept"}[removeVolumes])
	return nil
}

// proxyRegistryOthers lists apps other than this one registered with the
// shared proxy.
func (e *Engine) proxyRegistryOthers(ctx context.Context) ([]string, error) {
	res, err := e.T.Run(ctx, "ls -1 "+q(proxy.HostPaths(e.names()).Apps)+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var others []string
	for _, name := range strings.Fields(res.Stdout) {
		if name != e.Spec.Name && appNameRe.MatchString(name) {
			others = append(others, name)
		}
	}
	return others, nil
}

// Logs streams compose logs for one role/service (or all) from the current
// release.
func (e *Engine) Logs(ctx context.Context, name string, follow bool, tail int, out io.Writer) error {
	cur, err := release.Current(ctx, e.T, e.names())
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
	cmd := e.composeCmd(release.PathsFor(e.names()).Releases+"/"+cur+"/compose.yaml") + " logs --tail " + strconv.Itoa(tail)
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
		return fmt.Errorf("exec needs a component or service name")
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

// resolveService maps a name the operator typed to a Compose service. A
// workload's name IS its service name, so there is no indirection to undo: this
// checks the name means something, and passes through a service that only the
// rendered runtime knows about.
func (e *Engine) resolveService(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if _, ok := e.Spec.Workloads[name]; ok {
		return name, nil
	}
	if _, ok := e.Compose.Services[name]; ok {
		return name, nil
	}
	return "", fmt.Errorf("%q is neither a workload nor a compose service", name)
}

// volName: docker volume names — never interpolated back into a shell
// command without matching this (injection rule).
var volName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
