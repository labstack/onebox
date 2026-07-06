package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// Status prints recorded vs actual per role — divergence is the point
// (design §05). Recorded = the current symlink; actual = what each role's
// container says via its ob.release label and health.
//
// The host is high-latency and every command is a full SSH round trip (the
// docker work itself is negligible), so status is round-trip-bound. It fires
// every read concurrently in one wave — SSH multiplexes channels over the
// single connection, so their latencies overlap instead of summing — behind a
// spinner, since nothing streams until the whole table renders at the end.
func (e *Engine) Status(ctx context.Context) error {
	managed := e.Cfg.Proxy.Managed
	var (
		recorded  string
		byService map[string][]svcContainer
		incS      journal.Summary
		incFound  bool
		px        proxyRaw
	)

	reads := []func() error{
		func() (err error) { recorded, err = release.Current(ctx, e.T, e.Cfg.App); return },
		func() (err error) { byService, err = e.projectContainers(ctx); return },
		func() error {
			s, err := e.FindIncomplete(ctx)
			if errors.Is(err, ErrNoIncomplete) {
				return nil // a clean journal is not an error
			}
			if err != nil {
				return err // a read failure must NOT be mistaken for "clean"
			}
			incS, incFound = s, true
			return nil
		},
	}
	if managed {
		reads = append(reads, e.proxyReads(ctx, &px)...)
	}

	_, stop := e.ui.Busy("querying " + e.T.Host())
	err := gather(reads...)
	stop()
	if err != nil {
		return err
	}

	if recorded == "" {
		recorded = "(none — never deployed)"
	}
	fmt.Fprintf(e.Opts.Out, "app:      %s @ %s\n", e.Cfg.App, e.T.Host())
	fmt.Fprintf(e.Opts.Out, "recorded: %s\n\n", recorded)
	e.ui.Println(e.ui.Bold(fmt.Sprintf("%-12s %-10s %-32s %-10s %s", "ROLE", "MODE", "ACTUAL RELEASE", "HEALTH", "STATE")))

	diverged := false
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		cs := byService[role.Service]
		if len(cs) == 0 {
			diverged = true
			e.ui.Println(fmt.Sprintf("%-12s %-10s %-32s %-10s %s", roleName, role.Mode, "-", "-", e.ui.Warn("NOT RUNNING ⚠")))
			continue
		}
		for _, c := range cs {
			actual := c.release
			if actual == "" || actual == "<no value>" {
				actual = "(not ob-deployed)"
			}
			state := e.ui.OK("in sync")
			if actual != strings.TrimSpace(recorded) {
				state = e.ui.Warn("DIVERGED ⚠")
				diverged = true
			}
			if c.health != "healthy" && c.health != "none" {
				state += e.ui.Warn(" (" + c.health + ")")
				diverged = true
			}
			e.ui.Println(fmt.Sprintf("%-12s %-10s %-32s %-10s %s", roleName, role.Mode, actual, c.health, state))
		}
	}

	// accessories: running/health only — they converge separately, so an
	// unhealthy/starting one is shown but not a divergence. Absent (NOT RUNNING)
	// and present-but-not-serving ("down": crash-looping/paused) are both real
	// problems — a fully-exited accessory already diverged, so a crash-looping
	// one must too.
	fmt.Fprintln(e.Opts.Out)
	for _, acc := range e.Cfg.Accessories {
		cs := byService[acc]
		if len(cs) == 0 {
			e.ui.Println(fmt.Sprintf("accessory %-12s %s", acc, e.ui.Warn("NOT RUNNING ⚠")))
			diverged = true
			continue
		}
		if cs[0].health == "down" {
			e.ui.Println(fmt.Sprintf("accessory %-12s %s", acc, e.ui.Warn("down ⚠")))
			diverged = true
			continue
		}
		fmt.Fprintf(e.Opts.Out, "accessory %-12s %s\n", acc, cs[0].health)
	}

	if managed {
		fmt.Fprintln(e.Opts.Out)
		d, err := e.renderProxy(px)
		if err != nil {
			return err
		}
		diverged = diverged || d
	}

	// an unfinished deploy is the loudest divergence there is
	if incFound {
		diverged = true
		fmt.Fprintln(e.Opts.Out)
		e.warnf("INCOMPLETE deploy %s (started %s by %s) — `ob resume` or `ob abort`",
			incS.DeployID, incS.StartedAt, incS.Operator)
	}
	if diverged {
		return fmt.Errorf("status: divergence detected")
	}
	fmt.Fprintln(e.Opts.Out)
	e.ui.Successf("all in sync")
	return nil
}

// gather runs order-independent reads concurrently over the transport (SSH
// multiplexes many channels on one connection) and joins their errors. Wall
// time collapses from the sum of the round trips to the slowest single one.
func gather(fns ...func() error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(fns))
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			errs[i] = fn()
		}(i, fn)
	}
	wg.Wait()
	return errors.Join(errs...)
}
