package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/yeet/internal/release"
)

// Status prints recorded vs actual per role — divergence is the point
// (design §05). Recorded = the current symlink; actual = what each role's
// container says via its yeet.release label and health.
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
	fmt.Fprintf(e.Opts.Out, "%-12s %-10s %-32s %-10s %s\n", "ROLE", "MODE", "ACTUAL RELEASE", "HEALTH", "STATE")

	diverged := false
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		ids, err := e.containerIDs(ctx, role.Service)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			diverged = true
			fmt.Fprintf(e.Opts.Out, "%-12s %-10s %-32s %-10s %s\n", roleName, role.Mode, "-", "-", "NOT RUNNING ⚠")
			continue
		}
		for _, id := range ids {
			res, err := e.T.Run(ctx, "docker inspect -f '{{index .Config.Labels \"yeet.release\"}}' "+id)
			if err != nil {
				return err
			}
			actual := strings.TrimSpace(res.Stdout)
			if actual == "" || actual == "<no value>" {
				actual = "(not yeet-deployed)"
			}
			health, err := e.healthOf(ctx, id)
			if err != nil {
				return err
			}
			state := "in sync"
			if actual != strings.TrimSpace(recorded) {
				state = "DIVERGED ⚠"
				diverged = true
			}
			if health != "healthy" && health != "none" {
				state += " (" + health + ")"
				diverged = true
			}
			fmt.Fprintf(e.Opts.Out, "%-12s %-10s %-32s %-10s %s\n", roleName, role.Mode, actual, health, state)
		}
	}

	// accessories: running/health only — they converge separately
	fmt.Fprintln(e.Opts.Out)
	for _, acc := range e.Cfg.Accessories {
		id, err := e.containerID(ctx, acc)
		if err != nil {
			return err
		}
		if id == "" {
			fmt.Fprintf(e.Opts.Out, "accessory %-12s NOT RUNNING ⚠\n", acc)
			diverged = true
			continue
		}
		health, _ := e.healthOf(ctx, id)
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
		fmt.Fprintf(e.Opts.Out, "\n⚠ INCOMPLETE deploy %s (started %s by %s) — `yeet resume` or `yeet abort`\n",
			s.DeployID, s.StartedAt, s.Operator)
	}
	if diverged {
		return fmt.Errorf("status: divergence detected")
	}
	fmt.Fprintln(e.Opts.Out, "\nall in sync")
	return nil
}
