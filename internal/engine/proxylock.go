package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

// The HOST lock serializes mutations of host-scoped state (owner and proxy)
// across runner attempts — the same remoteLock protocol as the app lock, at
// _host/lock. No epoch and no fence: proxy converge is one short idempotent
// critical section, not a resumable multi-phase deploy, so only an expired
// holder or --break-lock is reclaimable. The one-application-per-host contract
// means the sole app lock cannot participate in a cross-application cycle.
// claimHostOwner acquires only the host lock; bootstrap and destroy acquire the
// app lock before the host lock, and proxy apply acquires only the host lock.
func (e *Engine) acquireHostLock(ctx context.Context, force bool) error {
	e.hostLockVal = ""
	e.hostLockToken = ""
	hp := proxy.HostPaths(e.names())
	ttl := int(e.lockTTL().Seconds())
	token := fmt.Sprintf("%x", time.Now().UnixNano())
	b, err := json.Marshal(lockMeta{
		Owner: journal.DefaultOperator(), DeployID: e.Spec.Name,
		TTLSeconds: ttl, AcquiredAt: time.Now().UTC().Format(time.RFC3339), Token: token,
	})
	if err != nil {
		return err
	}
	lock := remoteLock[lockMeta]{
		label: "host lock", path: hp.Lock, dir: hp.Base, attempts: 4, force: force,
		breakHint: "confirm no other runner is mutating host-scoped state, then retry with --break-lock",
		next:      func(context.Context) (string, error) { return string(b), nil },
		decide: func(holder lockHolder[lockMeta]) (lockVerdict, error) {
			h := holder.Meta
			switch {
			case holder.Age > ttl:
				e.logf("host lock: holder %s (app %s) expired %ds ago — taking over", h.Owner, h.DeployID, holder.Age-ttl)
			case force:
				e.logf("host lock: FORCE-breaking %s (app %s, age %ds)", h.Owner, h.DeployID, holder.Age)
			default:
				return lockRefuse, fmt.Errorf("host lock held by %s (app %s, age %ds, ttl %ds) — another runner is mutating host-scoped state; wait, or use --break-lock after inspecting it",
					h.Owner, h.DeployID, holder.Age, ttl)
			}
			return lockReclaim, nil
		},
	}
	value, err := lock.acquire(ctx, e)
	if err != nil {
		return err
	}
	e.hostLockVal = value
	e.hostLockToken = token
	return nil
}

func (e *Engine) releaseHostLock(_ context.Context) {
	if e.hostLockVal != "" && e.releaseRemoteLock(proxy.HostPaths(e.names()).Lock, e.hostLockVal, "host lock") {
		e.hostLockVal = ""
		e.hostLockToken = ""
	}
}

// hostMutate fences host-shared writes with the exact host-lock token. When an
// app fence is also active (bootstrap), mutate adds that guard as well.
func (e *Engine) hostMutate(ctx context.Context, cmd string) (res transport.Result, err error) {
	if e.hostLockVal == "" {
		return res, fmt.Errorf("host mutation attempted without owning the host lock")
	}
	res, err = e.mutate(ctx, guardedCmd(fileHolds(proxy.HostPaths(e.names()).Lock, e.hostLockVal), cmd, "onebox-host-fenced", lockLostExitCode))
	if err == nil && refusedBy(res, lockLostExitCode, "onebox-host-fenced") {
		err = ErrFenced
	}
	return res, err
}
