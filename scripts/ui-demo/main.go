// Run with `go run ./scripts/ui-demo` in a terminal. This simulates deployment
// output through the real UI renderer; it never contacts or modifies a host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/onebox/internal/ui"
)

func main() {
	fail := flag.Bool("fail", false, "show a failed healthcheck and recovery message")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	u := ui.New(os.Stdout, false)
	err := demo(ctx, u, *fail)
	ui.RestoreCursor(os.Stdout)
	if err != nil {
		u.Failf("%s", err)
		os.Exit(1)
	}
}

func demo(ctx context.Context, u *ui.UI, fail bool) error {
	started := time.Now()
	u.Header("deploy catalog → production · R42")
	u.Infof("Simulated deployment")
	for _, step := range []string{"transfer", "job migrate"} {
		done := u.Step(step, true)
		err := wait(ctx, 700*time.Millisecond)
		done(err)
		if err != nil {
			return err
		}
	}
	if err := roll(ctx, u, fail); err != nil {
		return err
	}
	for _, step := range []string{"verify", "activate"} {
		done := u.Step(step, true)
		err := wait(ctx, 650*time.Millisecond)
		done(err)
		if err != nil {
			return err
		}
	}
	u.Successf("deployed R42 in %s", ui.FmtDur(time.Since(started)))
	return nil
}

func roll(ctx context.Context, u *ui.UI, fail bool) (err error) {
	done := u.Step("server rolling ×3", true)
	defer func() { done(err) }()
	update, stop := u.Progress("server rolling", 3)
	defer stop()
	for replica := 1; replica <= 3; replica++ {
		update(replica-1, fmt.Sprintf("starting replica %d", replica))
		if err := wait(ctx, 350*time.Millisecond); err != nil {
			return err
		}
		_, stopWait := u.Busy(fmt.Sprintf("replica %d · waiting for healthcheck", replica))
		err = wait(ctx, 850*time.Millisecond)
		stopWait()
		if err != nil {
			return err
		}
		if fail && replica == 2 {
			u.Warnf("Existing replicas continue serving traffic")
			return errors.New("replica 2 healthcheck failed; inspect the service, then use ob resume or ob abort")
		}
		_, stopDrain := u.Busy("draining old replica")
		err = wait(ctx, 600*time.Millisecond)
		stopDrain()
		if err != nil {
			return err
		}
		update(replica, "replica replaced")
		if replica == 1 {
			u.Infof("server: replica 1 replaced; existing traffic remains available")
		}
		if err := wait(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
