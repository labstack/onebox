package onebox

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
)

// executeRecovery drives a restore or a drill, which are the same operation
// with different endings.
//
// The locking is the same as enablement's and for the same reason: a restore
// replaces a database's data, so it must not interleave with a deploy, another
// recovery, or a scheduled backup.
func executeRecovery(ctx context.Context, e *engine.Engine, service, target, generation string, promote bool, operationID string) error {
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
	selected := current
	if generation != "" {
		selectedGeneration := generation
		if generation == "legacy" {
			selectedGeneration = ""
		} else if !recoveryGeneration.MatchString(generation) {
			return fmt.Errorf("repository generation %q is not a PostgreSQL system identifier or legacy", generation)
		}
		selected.DatabaseSystemIdentifier = selectedGeneration
		selected.BackupRepositoryGeneration = selectedGeneration
		if err := selected.Seal(); err != nil {
			return err
		}
		runtime := selected.RuntimeState()
		runtime.DigestAvailable = true
		if err := e.RebindServiceRuntimeStates(map[string]app.ServiceRuntimeState{service: runtime}); err != nil {
			return err
		}
	}

	outcome, err := e.RecoverService(ctx, service, target, promote)
	if err != nil {
		if generation != "" {
			if outcome.RetainStaging && outcome.DatabaseSystemIdentifier != "" {
				selected.DatabaseSystemIdentifier = outcome.DatabaseSystemIdentifier
				if generation != "legacy" {
					selected.BackupRepositoryGeneration = outcome.DatabaseSystemIdentifier
				}
				if persistErr := writeRecoveryBinding(context.WithoutCancel(ctx), e, service, selected); persistErr != nil {
					return errors.Join(err, fmt.Errorf("cannot record the binding required by the partially promoted recovered service: %w", persistErr))
				}
			} else {
				runtime := current.RuntimeState()
				runtime.DigestAvailable = true
				if rebindErr := e.RebindServiceRuntimeStates(map[string]app.ServiceRuntimeState{service: runtime}); rebindErr == nil {
					_ = e.StageServiceCompose(context.WithoutCancel(ctx), service)
				}
			}
		}
		return err
	}
	if promote && generation != "" {
		selected.DatabaseSystemIdentifier = outcome.DatabaseSystemIdentifier
		if generation != "legacy" {
			selected.BackupRepositoryGeneration = outcome.DatabaseSystemIdentifier
		}
		if err := writeRecoveryBinding(ctx, e, service, selected); err != nil {
			return fmt.Errorf("recovered service is running but its selected repository binding could not be recorded: %w", err)
		}
	}
	e.ReportRecovery(outcome)
	return nil
}

var recoveryGeneration = regexp.MustCompile(`^[0-9]{1,20}$`)

func writeRecoveryBinding(ctx context.Context, e *engine.Engine, service string, state BackupLifecycleState) error {
	if err := state.Seal(); err != nil {
		return err
	}
	body, err := encodeBackupLifecycleState(state)
	if err != nil {
		return err
	}
	return e.WriteBackupLifecycleState(ctx, service, body)
}
