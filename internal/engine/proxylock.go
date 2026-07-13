package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

// The HOST lock serializes mutations of host-shared state (the managed proxy)
// across ALL ob apps on the box — same noclobber + TTL + holder-JSON
// protocol as the app lock, at _host/lock. No epoch and no fence: proxy
// converge is one short idempotent critical section, not a resumable
// multi-phase deploy. No deadlock with app locks is possible: every acquirer
// holds either the host lock alone (proxy apply) or its OWN app lock first
// (bootstrap, destroy) — two apps never contend on an app lock, so no cycle.
func (e *Engine) acquireHostLock(ctx context.Context, force bool) error {
	e.hostLockVal = ""
	e.hostLockToken = ""
	hp := proxy.HostPaths()
	token := fmt.Sprintf("%x", time.Now().UnixNano())
	meta := lockMeta{
		Owner: journal.DefaultOperator(), DeployID: e.Cfg.App,
		TTLSeconds: int(e.lockTTL().Seconds()), AcquiredAt: time.Now().UTC().Format(time.RFC3339), Token: token,
	}
	b, _ := json.Marshal(meta)
	create := "mkdir -p " + q(hp.Base) + " && set -C; echo " + q(string(b)) + " > " + q(hp.Lock) + " 2>/dev/null"

	for attempt := 0; attempt < 4; attempt++ {
		res, err := e.T.Run(ctx, create)
		if err != nil {
			return err
		}
		if res.ExitCode == 0 {
			e.hostLockVal = string(b)
			e.hostLockToken = token
			return nil
		}
		hres, err := e.T.Run(ctx, "cat "+q(hp.Lock)+" 2>/dev/null || true")
		if err != nil {
			return err
		}
		observed := strings.TrimSpace(hres.Stdout)
		if observed == "" {
			continue
		}
		var holder lockMeta
		_ = json.Unmarshal([]byte(observed), &holder)
		ares, err := e.T.Run(ctx, lockAgeCmd(q(hp.Lock)))
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
		removeObserved := `if [ "$(cat ` + q(hp.Lock) + ` 2>/dev/null)" = ` + q(observed) + ` ]; then rm -f ` + q(hp.Lock) + `; else exit 75; fi`
		if res, err := e.T.Run(ctx, removeObserved); err != nil {
			return fmt.Errorf("break host lock: %w", err)
		} else if res.ExitCode == 75 {
			continue
		} else if res.ExitCode != 0 {
			return fmt.Errorf("break host lock: %v %s", err, res.Stderr)
		}
	}
	return fmt.Errorf("could not acquire host lock")
}

func (e *Engine) releaseHostLock(ctx context.Context) {
	if e.hostLockVal == "" {
		return
	}
	expected := e.hostLockVal
	path := proxy.HostPaths().Lock
	cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := e.T.Run(cleanupContext, `if [ "$(cat `+q(path)+` 2>/dev/null)" = `+q(expected)+` ]; then rm -f `+q(path)+`; fi`)
	if err != nil || res.ExitCode != 0 {
		e.warnf("release host lock failed: %v %s", err, strings.TrimSpace(res.Stderr))
		return
	}
	e.hostLockVal = ""
	e.hostLockToken = ""
}

// hostMutate fences host-shared writes with the exact host-lock token. When an
// app fence is also active (bootstrap), mutate adds that guard as well.
func (e *Engine) hostMutate(ctx context.Context, cmd string) (res transport.Result, err error) {
	if e.hostLockVal == "" {
		return res, fmt.Errorf("host mutation attempted without owning the host lock")
	}
	guarded := `if [ "$(cat ` + q(proxy.HostPaths().Lock) + ` 2>/dev/null)" = ` + q(e.hostLockVal) + ` ]; then ` + cmd + `; else echo ob-host-fenced >&2; exit 96; fi`
	res, err = e.mutate(ctx, guarded)
	if err != nil {
		return res, err
	}
	if res.ExitCode == 96 && strings.Contains(res.Stderr, "ob-host-fenced") {
		return res, ErrFenced
	}
	return res, nil
}
