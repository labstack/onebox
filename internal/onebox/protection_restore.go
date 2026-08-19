package onebox

import (
	"context"
	"fmt"

	"github.com/labstack/onebox/internal/engine"
)

// executeRecovery drives a restore or a drill, which are the same operation
// with different endings.
//
// The locking is the same as enablement's and for the same reason: a restore
// replaces a database's data, so it must not interleave with a deploy, another
// recovery, or a scheduled backup.
func executeRecovery(ctx context.Context, e *engine.Engine, service, target string, promote bool, operationID string) error {
	if service == "" {
		return fmt.Errorf("recovery requires a service name")
	}
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

	outcome, err := e.RecoverService(ctx, service, target, promote)
	if err != nil {
		return err
	}
	e.ReportRecovery(outcome)
	return nil
}
