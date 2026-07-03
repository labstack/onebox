package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/labstack/yeet/internal/compose"
)

const stopGraceSeconds = 30

func (e *Engine) composeCmd(remoteComposePath string) string {
	return "docker compose -p " + e.Cfg.App + " -f " + q(remoteComposePath)
}

// RollRole executes scale–health–drain for one role (design §03 + the rev 5
// traffic-shift protocol: join → converged → drain → converged → bleed →
// SIGTERM → remove; SIGTERM never races the proxy).
func (e *Engine) RollRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Cfg.Roles[roleName]
	svc := role.Service
	cc := e.composeCmd(remoteComposePath)

	if res, err := e.T.Run(ctx, cc+" pull --quiet "+svc); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("pull %s: %v %s", svc, err, res.Stderr)
	}
	old, err := e.containerIDs(ctx, svc)
	if err != nil {
		return err
	}
	if len(old) > 1 {
		return fmt.Errorf("role %s: %d running containers; expected ≤1 (resume is M2 — clean up manually)", roleName, len(old))
	}

	scale := len(old) + 1
	if res, err := e.T.Run(ctx, fmt.Sprintf("%s up -d --no-deps --no-recreate --scale %s=%d %s", cc, svc, scale, svc)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("up --scale %s: %v %s", svc, err, res.Stderr)
	}

	newID, err := e.newcomer(ctx, svc, old)
	if err != nil {
		return err
	}

	// join: the newcomer becomes a routable endpoint via its healthcheck
	if err := e.waitHealth(ctx, newID, "healthy", time.Duration(role.Ready.Within), time.Duration(role.Ready.Interval)); err != nil {
		e.logf("join failed for %s — removing new container, old keeps serving", roleName)
		_, _ = e.T.Run(ctx, "docker rm -f "+newID)
		return fmt.Errorf("role %s: new container never became healthy: %w", roleName, err)
	}
	if len(old) == 0 {
		e.logf("%s: first deploy, no old container to drain", roleName)
		return nil
	}
	oldID := old[0]

	// converged: proxy has observed the healthy newcomer
	e.Opts.Sleep(e.Opts.ConvergeBuffer)

	// drain: poison old health so the proxy drops it BEFORE any signal
	if _, err := e.T.Run(ctx, "docker exec "+oldID+" touch "+compose.DrainFile); err != nil {
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
			_, _ = e.T.Run(ctx, "docker kill --signal="+role.Drain.Signal+" "+oldID)
		}
		e.Opts.Sleep(time.Duration(role.Drain.Wait))
	}

	if res, err := e.T.Run(ctx, fmt.Sprintf("docker stop -t %d %s", stopGraceSeconds, oldID)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("stop old %s: %v %s", oldID, err, res.Stderr)
	}
	if _, err := e.T.Run(ctx, "docker rm "+oldID); err != nil {
		return err
	}
	e.logf("%s: rolled %s -> %s", roleName, oldID, newID)
	return nil
}

func (e *Engine) newcomer(ctx context.Context, svc string, old []string) (string, error) {
	ids, err := e.containerIDs(ctx, svc)
	if err != nil {
		return "", err
	}
	prev := map[string]bool{}
	for _, o := range old {
		prev[o] = true
	}
	for _, id := range ids {
		if !prev[id] {
			return id, nil
		}
	}
	return "", fmt.Errorf("no new container appeared for %s", svc)
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
