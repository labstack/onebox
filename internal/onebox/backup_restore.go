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

	if _, err := e.AcquireBackupLock(ctx, service, operationID, 0); err != nil {
		return err
	}
	defer e.ReleaseBackupLock(service)
	stopBackupHeartbeat, err := e.StartBackupHeartbeat(ctx, service)
	if err != nil {
		return err
	}
	defer stopBackupHeartbeat()

	// Read under the backup lock, so the answer cannot change while this
	// operation is deciding on it.
	//
	// A disablement that was requested and did not finish leaves a service that
	// is still archiving under a record nobody has reconciled. Recovering into
	// that is work against a service somebody has just asked to stop
	// protecting: a drill materialises a whole cluster, and a cutover replaces
	// live data. The refusal is stated as a typed code so the operator is told
	// which state they are in rather than watching a restore they did not
	// expect to be allowed.
	current, err := currentBackupLifecycleState(ctx, e, e.Spec.Spec.Name, e.Opts.Environment, service)
	if err != nil {
		return err
	}
	if current.State == BackupDisablePending {
		failure, ferr := NewLifecycleFailure("backup_disable_pending")
		if ferr != nil {
			return ferr
		}
		return failure
	}

	outcome, err := e.RecoverService(ctx, service, target, promote)
	if err != nil {
		return err
	}
	e.ReportRecovery(outcome)
	return nil
}
