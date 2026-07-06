package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/release"
)

// Status prints recorded vs actual per role — divergence is the point
// (design §05). Recorded = the current symlink; actual = what each role's
// container says via its ob.release label and health.
func (e *Engine) Status(ctx context.Context) error {
	recorded, err := release.Current(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	if recorded == "" {
		recorded = "(none — never deployed)"
	}
	fmt.Fprintf(e.Opts.Out, "app:      %s @ %s\n", e.Cfg.App, e.T.Host())
	fmt.Fprintf(e.Opts.Out, "recorded: %s\n\n", recorded)
	e.ui.Println(e.ui.Bold(fmt.Sprintf("%-12s %-10s %-32s %-10s %s", "ROLE", "MODE", "ACTUAL RELEASE", "HEALTH", "STATE")))

	// One docker ps for the whole project, then one inspect per container —
	// instead of a docker ps + two inspects for every role and accessory.
	byService, err := e.projectContainers(ctx)
	if err != nil {
		return err
	}

	diverged := false
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		ids := byService[role.Service]
		if len(ids) == 0 {
			diverged = true
			e.ui.Println(fmt.Sprintf("%-12s %-10s %-32s %-10s %s", roleName, role.Mode, "-", "-", e.ui.Warn("NOT RUNNING ⚠")))
			continue
		}
		for _, id := range ids {
			actual, health, err := e.containerStatus(ctx, id)
			if err != nil {
				return err
			}
			if actual == "" || actual == "<no value>" {
				actual = "(not ob-deployed)"
			}
			state := e.ui.OK("in sync")
			if actual != strings.TrimSpace(recorded) {
				state = e.ui.Warn("DIVERGED ⚠")
				diverged = true
			}
			if health != "healthy" && health != "none" {
				state += e.ui.Warn(" (" + health + ")")
				diverged = true
			}
			e.ui.Println(fmt.Sprintf("%-12s %-10s %-32s %-10s %s", roleName, role.Mode, actual, health, state))
		}
	}

	// accessories: running/health only — they converge separately
	fmt.Fprintln(e.Opts.Out)
	for _, acc := range e.Cfg.Accessories {
		ids := byService[acc]
		if len(ids) == 0 {
			e.ui.Println(fmt.Sprintf("accessory %-12s %s", acc, e.ui.Warn("NOT RUNNING ⚠")))
			diverged = true
			continue
		}
		health, _ := e.healthOf(ctx, ids[0])
		fmt.Fprintf(e.Opts.Out, "accessory %-12s %s\n", acc, health)
	}

	if e.Cfg.Proxy.Managed {
		fmt.Fprintln(e.Opts.Out)
		d, err := e.proxyStatus(ctx)
		if err != nil {
			return err
		}
		diverged = diverged || d
	}

	// an unfinished deploy is the loudest divergence there is
	if s, err := e.FindIncomplete(ctx); err == nil {
		diverged = true
		fmt.Fprintln(e.Opts.Out)
		e.warnf("INCOMPLETE deploy %s (started %s by %s) — `ob resume` or `ob abort`",
			s.DeployID, s.StartedAt, s.Operator)
	}
	if diverged {
		return fmt.Errorf("status: divergence detected")
	}
	fmt.Fprintln(e.Opts.Out)
	e.ui.Successf("all in sync")
	return nil
}
