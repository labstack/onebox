package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/proxy"
)

// The HOST lock serializes mutations of host-shared state (the managed proxy)
// across ALL yeet apps on the box — same noclobber + TTL + holder-JSON
// protocol as the app lock, at _host/lock. No epoch and no fence: proxy
// converge is one short idempotent critical section, not a resumable
// multi-phase deploy. No deadlock with app locks is possible: every acquirer
// holds either the host lock alone (proxy apply) or its OWN app lock first
// (bootstrap, destroy) — two apps never contend on an app lock, so no cycle.
func (e *Engine) acquireHostLock(ctx context.Context, force bool) error {
	hp := proxy.HostPaths()
	meta := lockMeta{
		Owner: journal.DefaultOperator(), DeployID: e.Cfg.App,
		TTLSeconds: int(e.lockTTL().Seconds()), AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(meta)
	create := "mkdir -p " + q(hp.Base) + " && set -C; echo " + q(string(b)) + " > " + q(hp.Lock) + " 2>/dev/null"

	for attempt := 0; attempt < 2; attempt++ {
		res, err := e.T.Run(ctx, create)
		if err != nil {
			return err
		}
		if res.ExitCode == 0 {
			return nil
		}
		if attempt == 1 {
			break
		}
		hres, err := e.T.Run(ctx, "cat "+q(hp.Lock)+" 2>/dev/null || true")
		if err != nil {
			return err
		}
		var holder lockMeta
		_ = json.Unmarshal([]byte(strings.TrimSpace(hres.Stdout)), &holder)
		ares, err := e.T.Run(ctx, "echo $(( $(date +%s) - $(stat -c %Y "+q(hp.Lock)+" 2>/dev/null || echo 0) ))")
		if err != nil {
			return err
		}
		age, _ := strconv.Atoi(strings.TrimSpace(ares.Stdout))
		switch {
		case age > int(e.lockTTL().Seconds()):
			e.logf("host lock: holder %s (app %s) expired %ds ago — taking over", holder.Owner, holder.DeployID, age-int(e.lockTTL().Seconds()))
		case force:
			e.logf("host lock: FORCE-breaking %s (app %s, age %ds)", holder.Owner, holder.DeployID, age)
		default:
			return fmt.Errorf("host lock held by %s (app %s, age %ds, ttl %ds) — another app is converging the shared proxy; wait, or --force to break it",
				holder.Owner, holder.DeployID, age, int(e.lockTTL().Seconds()))
		}
		if res, err := e.T.Run(ctx, "rm -f "+q(hp.Lock)); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("break host lock: %v %s", err, res.Stderr)
		}
	}
	return fmt.Errorf("could not acquire host lock")
}

func (e *Engine) releaseHostLock(ctx context.Context) {
	_, _ = e.T.Run(ctx, "rm -f "+q(proxy.HostPaths().Lock))
}
