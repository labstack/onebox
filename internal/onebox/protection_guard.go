package onebox

import (
	"context"

	"github.com/labstack/onebox/internal/engine"
)

// underProtectionLocks runs one repository operation with the same guards every
// other mutation takes: host ownership, the application lock and fence, and the
// per-service protection lock.
//
// Backup, prune and verify used to run with none of them. Prune *deletes* base
// backups, so it could expire generations while a restore was reading them or
// while a deploy was replacing the container underneath. The flock inside the
// wal-g invocation serialises against the systemd timers and nothing else — it
// asserts no host ownership and stops no concurrent deploy.
func underProtectionLocks(
	ctx context.Context,
	e *engine.Engine,
	service, operationID string,
	run func(context.Context) error,
) error {
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return err
	}
	stopAppHeartbeat := e.StartHeartbeat(ctx)
	defer stopAppHeartbeat()

	if _, err := e.AcquireProtectionLock(ctx, service, operationID, 0); err != nil {
		return err
	}
	defer e.ReleaseProtectionLock(service)
	stopProtectionHeartbeat, err := e.StartProtectionHeartbeat(ctx, service)
	if err != nil {
		return err
	}
	defer stopProtectionHeartbeat()

	return run(ctx)
}
