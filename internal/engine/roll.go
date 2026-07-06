package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/config"
)

func (e *Engine) composeCmd(remoteComposePath string) string {
	return "docker compose -p " + e.Cfg.App + " -f " + q(remoteComposePath)
}

// newcomerIDs finds containers of a specific release — the ob.release label
// render injects is what makes resume possible.
func (e *Engine) newcomerIDs(ctx context.Context, svc, releaseID string) ([]string, error) {
	res, err := e.T.Run(ctx,
		"docker ps -q --filter label=com.docker.compose.project="+q(e.Cfg.App)+
			" --filter label=com.docker.compose.service="+q(svc)+
			" --filter label=ob.release="+q(releaseID))
	if err != nil {
		return nil, err
	}
	return splitIDs(res.Stdout)
}

// RollRole rolls a role to its desired replica count of the new release with
// zero downtime (design §03 + the rev 5 traffic-shift protocol: join →
// converged → drain → converged → bleed → SIGTERM → remove). It surges ONE new
// replica at a time, waits it healthy, retires one old, and hands the newcomer
// a clean slot name — never compose's <project>-<svc>-<n> default. Repeats until
// every replica is the new release. Resume-aware: already-running newcomers of
// this release are adopted, not duplicated.
func (e *Engine) RollRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Cfg.Roles[roleName]
	svc := role.Service
	cc := e.composeCmd(remoteComposePath)
	releaseID := filepath.Base(filepath.Dir(remoteComposePath))
	desired := role.Count()
	within, pollEvery := readyTiming(role)

	pulled := false
	// Each pass converges by one step: add a missing new replica, or retire a
	// surplus/old one. The guard bounds a pathological non-converging loop.
	for guard := 0; ; guard++ {
		news, err := e.newcomerIDs(ctx, svc, releaseID)
		if err != nil {
			return err
		}
		cur, err := e.containerIDs(ctx, svc)
		if err != nil {
			return err
		}
		olds := subtract(cur, news)
		if guard > 4*(desired+len(olds))+8 {
			return fmt.Errorf("role %s: roll did not converge (news=%d olds=%d)", roleName, len(news), len(olds))
		}

		if len(news) >= desired && len(olds) == 0 {
			if len(news) == desired {
				break
			}
			// surplus new replicas (count reduced) — drain one down to desired
			if err := e.retireContainer(ctx, role, news[desired], pollEvery); err != nil {
				return err
			}
			continue
		}

		// surge one new replica if we still need more of the new release
		if len(news) < desired {
			if !pulled {
				if res, err := e.mutate(ctx, cc+" pull --quiet "+svc); err != nil {
					return err
				} else if res.ExitCode != 0 {
					return fmt.Errorf("pull %s: %s", svc, res.Stderr)
				}
				pulled = true
			}
			known := idSet(news)
			scale := len(cur) + 1
			if res, err := e.mutate(ctx, fmt.Sprintf("%s up -d --no-deps --no-recreate --scale %s=%d %s", cc, svc, scale, svc)); err != nil {
				return err
			} else if res.ExitCode != 0 {
				return fmt.Errorf("up --scale %s: %s", svc, res.Stderr)
			}
			after, err := e.newcomerIDs(ctx, svc, releaseID)
			if err != nil {
				return err
			}
			newID := ""
			for _, id := range after {
				if !known[id] {
					newID = id
					break
				}
			}
			if newID == "" {
				return fmt.Errorf("role %s: scale up produced no new container", roleName)
			}
			// strip the <project>-<svc>-<n> name immediately so the project
			// prefix never appears; a transient name until it takes a slot.
			e.renameContainer(ctx, newID, svc+"-new")
			// join: the newcomer becomes a routable endpoint via its healthcheck
			if err := e.waitHealth(ctx, newID, "healthy", within, pollEvery); err != nil {
				e.logf("join failed for %s — removing new container, existing keep serving", roleName)
				_, _ = e.mutate(ctx, "docker rm -f "+newID)
				return fmt.Errorf("role %s: new container never became healthy: %w", roleName, err)
			}
		}

		// retire one old, freeing its slot for the newcomer just added
		if len(olds) > 0 {
			if err := e.retireContainer(ctx, role, olds[0], pollEvery); err != nil {
				return err
			}
		}

		// hand clean slot names to any new container that doesn't have one yet
		if err := e.reslot(ctx, svc, releaseID, desired); err != nil {
			return err
		}
	}
	if err := e.reslot(ctx, svc, releaseID, desired); err != nil {
		return err
	}
	return nil
}

// retireContainer drains one container out of rotation (poison its health so the
// proxy drops it BEFORE any signal), waits the drain, optionally bleeds long
// connections, then stops and removes it.
func (e *Engine) retireContainer(ctx context.Context, role config.Role, id string, pollEvery time.Duration) error {
	e.sleepBusy("converge (proxy observes the newcomer)", e.Opts.ConvergeBuffer)

	if _, err := e.mutate(ctx, "docker exec "+id+" touch "+compose.DrainFile); err != nil {
		return err
	}
	drainBudget := 5 * pollEvery
	if err := e.waitHealth(ctx, id, "unhealthy", drainBudget, pollEvery); err != nil {
		e.warnf("container never reported unhealthy (%v); proceeding after buffer", err)
	}
	e.sleepBusy("converge (proxy drops the drained container)", e.Opts.ConvergeBuffer)

	if role.Drain != nil && role.Drain.Wait > 0 {
		if role.Drain.Signal != "" && role.Drain.Signal != "TERM" {
			_, _ = e.mutate(ctx, "docker kill --signal="+role.Drain.Signal+" "+id)
		}
		e.sleepBusy("drain wait ("+time.Duration(role.Drain.Wait).String()+")", time.Duration(role.Drain.Wait))
	}

	if res, err := e.mutate(ctx, fmt.Sprintf("docker stop -t %d %s", role.StopGraceSeconds(), id)); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("stop %s: %s", id, res.Stderr)
	}
	_, err := e.mutate(ctx, "docker rm "+id)
	return err
}

// reslot gives each new-release container a clean, stable slot name: the plain
// service name for a single replica, or <service>-1..<service>-N for a fleet.
// A slot still held by an old container counts as taken, so names never clash;
// as olds retire their slots free and the next reslot fills them.
func (e *Engine) reslot(ctx context.Context, svc, releaseID string, desired int) error {
	news, err := e.newcomerIDs(ctx, svc, releaseID)
	if err != nil {
		return err
	}
	all, err := e.containerIDs(ctx, svc)
	if err != nil {
		return err
	}
	slots := slotNames(svc, desired)
	slotSet := map[string]bool{}
	for _, s := range slots {
		slotSet[s] = true
	}
	newSet := idSet(news)
	nameByID := map[string]string{}
	taken := map[string]bool{}
	for _, id := range all {
		n, err := e.nameOf(ctx, id)
		if err != nil {
			return err
		}
		nameByID[id] = n
		if slotSet[n] {
			taken[n] = true
		}
	}
	for _, id := range all {
		if !newSet[id] || slotSet[nameByID[id]] {
			continue // not ours, or already correctly slotted
		}
		target := ""
		for _, s := range slots {
			if !taken[s] {
				target = s
				break
			}
		}
		if target == "" {
			continue // no free slot yet (an old still holds it)
		}
		_, _ = e.mutate(ctx, "docker rename "+id+" "+target)
		taken[target] = true
	}
	return nil
}

// renameContainer renames a container, idempotent and best-effort — ob
// identifies containers by label, so a failed rename never affects correctness.
func (e *Engine) renameContainer(ctx context.Context, id, name string) {
	if cur, err := e.nameOf(ctx, id); err == nil && cur == name {
		return
	}
	_, _ = e.mutate(ctx, "docker rename "+id+" "+name)
}

// nameOf returns a container's name without the leading slash docker reports.
func (e *Engine) nameOf(ctx context.Context, id string) (string, error) {
	res, err := e.T.Run(ctx, "docker inspect -f '{{.Name}}' "+id)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(res.Stdout), "/"), nil
}

// slotNames is the target name set: the plain service name for one replica,
// else <service>-1..<service>-N.
func slotNames(svc string, desired int) []string {
	if desired <= 1 {
		return []string{svc}
	}
	out := make([]string, desired)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d", svc, i+1)
	}
	return out
}

func idSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// readyTiming: gate budget and poll interval, with defaults for roles whose
// readiness is ADOPTED from the compose healthcheck (ready absent or
// timing-only).
func readyTiming(role config.Role) (within, interval time.Duration) {
	within, interval = 120*time.Second, 5*time.Second
	if role.Ready != nil {
		if role.Ready.Within > 0 {
			within = time.Duration(role.Ready.Within)
		}
		if role.Ready.Interval > 0 {
			interval = time.Duration(role.Ready.Interval)
		}
	}
	return within, interval
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
	_, stop := e.ui.Busy(fmt.Sprintf("waiting %.12s → %s", id, want))
	defer stop()
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
