package onebox

import (
	"context"
	"fmt"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
)

// executeProtectionDisable stops archiving and returns the service to an
// ordinary unprotected one.
//
// What it does not do is touch the repository. The backups already taken are
// the reason anyone turned protection on, and an operator disabling archiving
// today may still need to recover from last week — so the history stays, and
// `ob backup status` keeps reporting it.
func executeProtectionDisable(ctx context.Context, e *engine.Engine, resolved *app.Resolved, environment, service, operationID string) error {
	if service == "" {
		return fmt.Errorf("protection disable requires a service name")
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

	current, err := currentProtectionLifecycleState(ctx, e, resolved.Spec.Name, environment, service)
	if err != nil {
		return err
	}
	if current.State != ProtectionEnabled && current.State != ProtectionDisablePending {
		return fmt.Errorf("service %s is not protected, so there is nothing to disable", service)
	}
	next, err := DisableProtection(current, operationID, current.Epoch+1)
	if err != nil {
		return err
	}
	body, err := encodeProtectionLifecycleState(next)
	if err != nil {
		return err
	}
	if err := e.WriteProtectionLifecycleState(ctx, service, body); err != nil {
		return err
	}
	// Rebound before applying, so the render that follows produces the ordinary
	// server rather than the protected one this run started from.
	if err := e.RebindServiceRuntimeStates(map[string]app.ServiceRuntimeState{
		service: next.RuntimeState(),
	}); err != nil {
		return err
	}
	// Restarts the service without archive_mode and removes its timers, because
	// SyncProtectionSchedules removes what is no longer protected.
	if err := e.ApplyServices(ctx); err != nil {
		return fmt.Errorf("service %s could not restart without protection: %w", service, err)
	}
	// The destination keys have no further use here, and a credential that is
	// not needed is one that should not be lying around. The repository is
	// untouched; re-enabling stages them again from the encrypted file.
	if err := e.RemoveProtectionCredentials(ctx, service, current.LastEffective); err != nil {
		return err
	}
	e.ReportDisabled(service)
	return nil
}
