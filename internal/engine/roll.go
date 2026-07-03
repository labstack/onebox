package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/labstack/yeet/internal/compose"
)

const stopGraceSeconds = 30

func (e *Engine) composeCmd(remoteComposePath string) string {
	return "docker compose -p " + e.Cfg.App + " -f " + q(remoteComposePath)
}

// newcomerIDs finds containers of a specific release — the yeet.release
// label render injects is what makes resume possible.
func (e *Engine) newcomerIDs(ctx context.Context, svc, releaseID string) ([]string, error) {
	res, err := e.T.Run(ctx,
		"docker ps -q --filter label=com.docker.compose.project="+e.Cfg.App+
			" --filter label=com.docker.compose.service="+svc+
			" --filter label=yeet.release="+releaseID)
	if err != nil {
		return nil, err
	}
	return splitIDs(res.Stdout)
}

// RollRole executes scale–health–drain for one role (design §03 + the rev 5
// traffic-shift protocol: join → converged → drain → converged → bleed →
// SIGTERM → remove; SIGTERM never races the proxy). Resume-aware: an already
// running newcomer of this release is adopted, not duplicated.
func (e *Engine) RollRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Cfg.Roles[roleName]
	svc := role.Service
	cc := e.composeCmd(remoteComposePath)
	releaseID := filepath.Base(filepath.Dir(remoteComposePath))

	all, err := e.containerIDs(ctx, svc)
	if err != nil {
		return err
	}
	newcomers, err := e.newcomerIDs(ctx, svc, releaseID)
	if err != nil {
		return err
	}
	old := subtract(all, newcomers)
	if len(old) > 1 {
		return fmt.Errorf("role %s: %d pre-existing containers; expected ≤1", roleName, len(old))
	}
	if len(newcomers) > 1 {
		return fmt.Errorf("role %s: %d containers of release %s already running; clean up manually", roleName, len(newcomers), releaseID)
	}

	var newID string
	if len(newcomers) == 1 {
		newID = newcomers[0]
		e.logf("%s: newcomer of %s already running (resume) — continuing from health gate", roleName, releaseID)
	} else {
		if res, err := e.mutate(ctx, cc+" pull --quiet "+svc); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("pull %s: %s", svc, res.Stderr)
		}
		scale := len(all) + 1
		if res, err := e.mutate(ctx, fmt.Sprintf("%s up -d --no-deps --no-recreate --scale %s=%d %s", cc, svc, scale, svc)); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("up --scale %s: %s", svc, res.Stderr)
		}
		fresh, err := e.newcomerIDs(ctx, svc, releaseID)
		if err != nil {
			return err
		}
		if len(fresh) != 1 {
			return fmt.Errorf("role %s: expected exactly one new container, found %d", roleName, len(fresh))
		}
		newID = fresh[0]
	}

	// join: the newcomer becomes a routable endpoint via its healthcheck
	if err := e.waitHealth(ctx, newID, "healthy", time.Duration(role.Ready.Within), time.Duration(role.Ready.Interval)); err != nil {
		e.logf("join failed for %s — removing new container, old keeps serving", roleName)
		_, _ = e.mutate(ctx, "docker rm -f "+newID)
		return fmt.Errorf("role %s: new container never became healthy: %w", roleName, err)
	}
	if len(old) == 0 {
		e.logf("%s: no old container to drain", roleName)
		return nil
	}
	oldID := old[0]

	// converged: proxy has observed the healthy newcomer
	e.Opts.Sleep(e.Opts.ConvergeBuffer)

	// drain: poison old health so the proxy drops it BEFORE any signal
	if _, err := e.mutate(ctx, "docker exec "+oldID+" touch "+compose.DrainFile); err != nil {
		return err
	}
	drainBudget := 5 * time.Duration(role.Ready.Interval)
	if err := e.waitHealth(ctx, oldID, "unhealthy", drainBudget, time.Duration(role.Ready.Interval)); err != nil {
		e.logf("warn: old container never reported unhealthy (%v); proceeding after buffer", err)
	}
	e.Opts.Sleep(e.Opts.ConvergeBuffer) // converged: proxy dropped it

	// bleed: optional long-connection window (WebSocket/SSE etc.)
	if role.Drain != nil && role.Drain.Wait > 0 {
		if role.Drain.Signal != "" && role.Drain.Signal != "TERM" {
			_, _ = e.mutate(ctx, "docker kill --signal="+role.Drain.Signal+" "+oldID)
		}
		e.Opts.Sleep(time.Duration(role.Drain.Wait))
	}

	if res, err := e.mutate(ctx, fmt.Sprintf("docker stop -t %d %s", stopGraceSeconds, oldID)); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("stop old %s: %s", oldID, res.Stderr)
	}
	if _, err := e.mutate(ctx, "docker rm "+oldID); err != nil {
		return err
	}
	e.logf("%s: rolled %s -> %s", roleName, oldID, newID)
	return nil
}

func subtract(all, remove []string) []string {
	drop := map[string]bool{}
	for _, r := range remove {
		drop[r] = true
	}
	var out []string
	for _, a := range all {
		if !drop[a] {
			out = append(out, a)
		}
	}
	return out
}

func (e *Engine) waitHealth(ctx context.Context, id, want string, budget, interval time.Duration) error {
	deadline := e.Opts.Now().Add(budget)
	for {
		h, err := e.healthOf(ctx, id)
		if err != nil {
			return err
		}
		if h == want {
			return nil
		}
		if h == "none" && want == "healthy" {
			return fmt.Errorf("container %s has no healthcheck — rolling requires one (generated from ready:)", id)
		}
		if e.Opts.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s to be %s (last: %s)", id, want, h)
		}
		e.Opts.Sleep(interval)
	}
}
