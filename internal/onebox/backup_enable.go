package onebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/secrets"
)

// executeBackupEnable turns a declared policy into an established one.
//
// The order is forced: check the credentials, pin the image, record the state
// that makes rendering produce a protected server, restart under it — which is
// also what places the verified wal-g binary and turns archive_mode on — and
// only then take the base backup the recovery window is measured from.
//
// Re-running it on an already-enabled service re-converges rather than
// refusing: the runtime is re-staged, the server re-applied, and another base
// backup taken. That is what an operator does after a partial failure, and
// making it an error would only teach them to delete state by hand.
//
// It is not finished until a base backup exists. WAL archiving with no base
// backup recovers nothing — there is no starting point to replay onto — so a
// command that returned success there would be telling the operator their
// database is protected at the exact moment it is not, which is the failure
// this whole product is arranged to refuse.
func executeBackupEnable(ctx context.Context, e *engine.Engine, resolved *app.Resolved, configPath, environment, service, operationID string) error {
	if service == "" {
		return fmt.Errorf("backup enable requires a service name")
	}
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	// The application lock, taken here for the same reason every other mutation
	// takes one: this restarts a database. ApplyServices does not take it — the
	// engine's own ServiceApply does, and enablement drives ApplyServices
	// directly — so without this a deploy and an enablement could interleave on
	// the same service.
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

	declared, ok := resolved.Services[service]
	if !ok {
		return fmt.Errorf("service %s is not declared in this project", service)
	}
	driver := declared.Driver
	if driver == "" {
		driver = service
	}
	if driver != "postgres" {
		return fmt.Errorf(
			"service %s runs the %s driver; executable backup exists for postgres only today", service, driver)
	}
	if declared.Backup == nil {
		return fmt.Errorf(
			"service %s declares no backup policy; add services.%s.backup to the project first", service, service)
	}
	// The declared projection, because this command is where the project's
	// intent takes effect. Everything else — rendering, recovery, status,
	// retention — resolves the recorded one, so a target edited after
	// enablement cannot silently redirect a restore at a repository the history
	// is not in.
	projection, err := resolved.DeclaredBackupProjection(service)
	if err != nil {
		return err
	}
	if _, ok := app.LifecycleCredentialSlots(driver, resolved.DeclaredVersion(service)); !ok {
		return fmt.Errorf("service %s runs a %s version with no qualified backup contract", service, driver)
	}

	// The credential file is installed here, not assumed to be present.
	//
	// It was previously the operator's job, and the error message told them to
	// "stage it through the trusted secret flow" — a flow that does not reach
	// backup credentials, so the instruction pointed at nothing. Onebox
	// already knows which encrypted file the target names and already has the
	// machinery to place a mode-0600 file under the service lock, so it does.
	//
	// Decrypted and checked on this machine before any of it crosses to the
	// target: a missing entry discovered after the server has restarted with
	// archive_mode on is a database whose WAL cannot drain, which is a far
	// worse place to learn it.
	plaintext, err := secrets.RenderContext(ctx, filepath.Dir(configPath), projection.Target.Credentials.File)
	if err != nil {
		return fmt.Errorf("decrypt the backup credentials for service %s: %w", service, err)
	}
	if err := app.ValidateWalgCredentials(plaintext, projection.Target); err != nil {
		return err
	}
	// The install is a backup mutation, so it needs the service lock as
	// well as the application lock. Taking it here also closes a gap that had
	// nothing to do with credentials: enablement restarts a database and took
	// no per-service lock at all, so two of them could interleave.
	if _, err := e.AcquireBackupLock(ctx, service, operationID, 0); err != nil {
		return err
	}
	defer e.ReleaseBackupLock(service)
	stopHeartbeat, err := e.StartBackupHeartbeat(ctx, service)
	if err != nil {
		return err
	}
	defer stopHeartbeat()
	if _, err := e.InstallBackupCredentialFile(ctx, service, declared.Backup.Target,
		app.WalgCredentialEntries(projection.Target), plaintext); err != nil {
		return err
	}

	// Read before resolving: a service that is already bound keeps the exact
	// bytes it was bound with, and this is where that record comes from.
	recorded, err := currentBackupLifecycleState(ctx, e, resolved.Spec.Name, environment, service)
	if err != nil {
		return err
	}
	image, err := e.ResolveProtectedImage(ctx, service, recorded.ServiceImage, recorded.ServiceImageReference)
	if err != nil {
		return err
	}
	// The authored reference, not the runtime one: the runtime selection is the
	// pinned digest while the service is protected and the tag once it is not,
	// so recording it would never match on the next enable.
	declaredImage, err := resolved.DeclaredServiceImage(service)
	if err != nil {
		return err
	}

	// Re-enabling after a disablement continues the existing record rather than
	// starting a new one. The epoch is a fence: reusing or lowering it would let
	// an operation launched against the old state still be accepted, which is
	// precisely what the fence exists to prevent.
	// The host must be able to run the schedules this policy declares, and that
	// is checked before anything durable happens. Finding out at the schedule
	// sync would mean finding out after the service had been recorded as
	// protected and restarted archiving, leaving an enablement half-applied.
	if err := e.RequireBackupScheduling(ctx, []string{service}); err != nil {
		return err
	}

	// Staged before anything durable claims the service is protected. A failure
	// here — an unreachable release, a checksum that does not match, a host
	// architecture with no verified build — must leave the service exactly as
	// it was, not recorded as enabled with no binary to archive with. The
	// earlier ordering did the opposite, and a single failed enablement left a
	// state that refused every retry.
	if err := e.StageBackupRuntime(ctx, service, app.RenderWalgWrapper(projection.Target)); err != nil {
		return fmt.Errorf("service %s: cannot place its backup runtime: %w", service, err)
	}

	current, err := currentBackupLifecycleState(ctx, e, resolved.Spec.Name, environment, service)
	if err != nil {
		return err
	}
	systemIdentifier, err := e.PostgresSystemIdentifier(ctx, service)
	if err != nil {
		return err
	}
	repositoryGeneration := backupRepositoryGeneration(current, projection, resolved.Spec.Name, service, systemIdentifier)
	// Rebound every time, including when the service is already enabled.
	//
	// `ob backup enable` is the one command that binds a service to a
	// repository, and re-running it after editing the policy or the target is
	// how an operator moves it. Skipping the transition when already enabled
	// discarded the freshly pinned image and the new projection, so the service
	// went on archiving to the original repository with no command able to
	// change it — while the edited project sat there looking applied.
	next, err := rebindBackup(current, projection, image, declaredImage, operationID, systemIdentifier, repositoryGeneration)
	if err != nil {
		return err
	}
	body, err := encodeBackupLifecycleState(next)
	if err != nil {
		return err
	}
	if err := e.WriteBackupLifecycleState(ctx, service, body); err != nil {
		return err
	}
	// The project was loaded before this service was protected, so without
	// rebinding the very next render would produce the unprotected server this
	// run started from and quietly undo what was just recorded.
	runtime := next.RuntimeState()
	runtime.DigestAvailable = true
	if err := e.RebindServiceRuntimeStates(map[string]app.ServiceRuntimeState{service: runtime}); err != nil {
		return err
	}
	// ApplyServices stages the verified wal-g binary and the generated wrapper
	// before starting anything that mounts them, then restarts the server with
	// archive_mode on.
	if err := e.ApplyServices(ctx); err != nil {
		failure := fmt.Errorf("service %s could not restart under backup: %w", service, err)
		if rollbackErr := rollbackFailedEnablement(ctx, e, service, current, next, operationID); rollbackErr != nil {
			return errors.Join(failure, rollbackErr)
		}
		return failure
	}
	// Everything PostgreSQL is holding goes to the repository before the base
	// backup writes into the same WAL namespace. Without this, `backup-push`
	// can land a segment the archiver has not shipped yet, the archiver's own
	// copy is then refused as "already archived, contents differ", and because
	// PostgreSQL archives in order the chain stops there permanently — while
	// this command reports success.
	if err := e.QuiesceArchiver(ctx, service); err != nil {
		if rollbackErr := rollbackFailedEnablement(ctx, e, service, current, next, operationID); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return err
	}

	// The base backup is the last step, and until it exists the service is
	// archiving with nothing to replay onto.
	//
	// A failure here used to be returned as-is, which left `archive_mode=on`
	// and an archive_command that could not reach the repository — so
	// PostgreSQL retained every WAL segment it could not ship, indefinitely,
	// established by a command that reported failure. On a busy database that
	// is an unbounded disk commitment nobody agreed to.
	if err := e.BackupService(ctx, service); err != nil {
		if rollbackErr := rollbackFailedEnablement(ctx, e, service, current, next, operationID); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return err
	}

	// A target that moved is reported and its old credential retired only after
	// the new repository has accepted a complete base backup. Until this point a
	// failure must be able to restore the exact previous binding, including the
	// credential file that opens it.
	nextRepository := app.WalgPrefix(projection.Target, resolved.Spec.Name, service, next.BackupRepositoryGeneration)
	if previous := previousBackupRepository(current, resolved.Spec.Name, service); previous != "" && previous != nextRepository {
		e.ReportTargetMoved(service, previous, nextRepository)
		if retiresCredentialFile(current.LastEffective, projection) {
			if err := e.RemoveBackupCredentials(ctx, service, current.LastEffective); err != nil {
				return err
			}
		}
	}
	return nil
}

// rollbackFailedEnablement restores the state in which this enablement found
// the service. An enabled or disable-pending service was already archiving, so
// its exact repository binding is reinstated. A previously unprotected service
// instead walks the fail-closed disable transition used by first enablement.
func rollbackFailedEnablement(ctx context.Context, e *engine.Engine, service string, current, attempted BackupLifecycleState, operationID string) error {
	if current.State != BackupEnabled && current.State != BackupDisablePending {
		return revertFailedEnablement(ctx, e, service, attempted, operationID)
	}
	body, err := encodeBackupLifecycleState(current)
	if err != nil {
		return err
	}
	if err := e.WriteBackupLifecycleState(ctx, service, body); err != nil {
		return err
	}
	runtime := current.RuntimeState()
	runtime.DigestAvailable = true
	if err := e.RebindServiceRuntimeStates(map[string]app.ServiceRuntimeState{service: runtime}); err != nil {
		return err
	}
	if err := e.ApplyServices(ctx); err != nil {
		return fmt.Errorf("service %s could not restore its previous backup binding: %w", service, err)
	}
	return nil
}

// revertFailedEnablement takes a service back to unprotected after enablement
// turned archiving on but could not complete a base backup.
//
// It walks the same two transitions `ob backup disable` walks, for the same
// reason: pending is written first, so a run interrupted during the restart
// leaves a record saying the decision was made and the work is unfinished,
// rather than one claiming the service stopped archiving while it still is.
//
// The credential file installed by this run is deliberately left in place. The
// operator's next move is to fix the cause and re-run enable, which reinstalls
// it anyway, and removing it makes the failure harder to diagnose than the
// unused keys are worth.
func revertFailedEnablement(ctx context.Context, e *engine.Engine, service string, enabled BackupLifecycleState, operationID string) error {
	pending, next, err := disablementAfterFailedEnablement(enabled, operationID, time.Now())
	if err != nil {
		return err
	}
	body, err := encodeBackupLifecycleState(pending)
	if err != nil {
		return err
	}
	if err := e.WriteBackupLifecycleState(ctx, service, body); err != nil {
		return err
	}
	if err := e.RebindServiceRuntimeStates(map[string]app.ServiceRuntimeState{
		service: next.RuntimeState(),
	}); err != nil {
		return err
	}
	// Restarts the server without archive_mode and removes the timers this
	// enablement installed, because SyncBackupSchedules removes what is no
	// longer protected.
	if err := e.ApplyServices(ctx); err != nil {
		return fmt.Errorf("service %s could not restart without backup: %w", service, err)
	}
	if body, err = encodeBackupLifecycleState(next); err != nil {
		return err
	}
	return e.WriteBackupLifecycleState(ctx, service, body)
}

// encodeBackupLifecycleState renders a validated record as the single JSON
// line the observation probe expects: it reads the marker from the first line
// and the record from the rest, and refuses more than one JSON value.
func encodeBackupLifecycleState(state BackupLifecycleState) ([]byte, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// currentBackupLifecycleState returns the record the target holds, or a
// fresh never-enabled one when backup has never been established here.
//
// The starting epoch is 1 rather than 0 because the schema treats a
// non-positive epoch as unsealed state: an epoch of 0 is how an uninitialised
// record is told apart from a real one, so it cannot also be a real one.
func currentBackupLifecycleState(ctx context.Context, e *engine.Engine, application, environment, service string) (BackupLifecycleState, error) {
	encoded, err := e.ReadBackupLifecycleState(ctx, service)
	if err != nil {
		return BackupLifecycleState{}, err
	}
	if len(encoded) == 0 {
		return NewBackupLifecycleState(application, environment, service, 1)
	}
	state, err := DecodeBackupLifecycleState(encoded)
	if err != nil {
		return BackupLifecycleState{}, fmt.Errorf("service %s lifecycle state: %w", service, err)
	}
	if state.Application != application || state.Environment != environment || state.Service != service {
		return BackupLifecycleState{}, fmt.Errorf("service %s lifecycle state belongs to a different protected identity", service)
	}
	return state, nil
}

// rebindBackup produces the enabled state for this run, from whichever
// state the service is in. An already-enabled service is taken back to
// never-enabled first, because EnableBackup is the single place that
// decides what an enabled record contains and it refuses to transition from
// enabled — the alternative is a second, divergent copy of that logic.
func rebindBackup(current BackupLifecycleState, projection app.BackupEffectiveProjection, image, imageReference, operationID, systemIdentifier, repositoryGeneration string) (BackupLifecycleState, error) {
	source := current
	if current.State == BackupEnabled {
		source.State = BackupDisabled
		source.Phase = BackupPhaseIdle
		// Resealed, because EnableBackup validates what it is handed and the
		// digest covers the two fields just changed. Without this, re-running
		// enable on an already-enabled service — the documented way to move a
		// service to an edited policy or target — failed every time with
		// "backup lifecycle state digest mismatch", against a record that was
		// perfectly intact on the host.
		if err := source.Seal(); err != nil {
			return BackupLifecycleState{}, err
		}
	}
	next, err := EnableBackup(source, projection, image, imageReference, operationID, true, current.Epoch+1)
	if err != nil {
		return BackupLifecycleState{}, err
	}
	next.DatabaseSystemIdentifier = systemIdentifier
	next.BackupRepositoryGeneration = repositoryGeneration
	if err := next.Seal(); err != nil {
		return BackupLifecycleState{}, err
	}
	return next, nil
}

// backupRepositoryGeneration keeps an established binding only while both the
// database and target repository root are provably unchanged. A legacy record
// has no database identity, so it cannot make that proof: its next explicit
// enable starts a cluster-scoped generation and leaves the old repository
// untouched. Guessing that it is the same database is exactly how a lifecycle
// record surviving volume replacement would recreate the collision this fixes.
func backupRepositoryGeneration(current BackupLifecycleState, projection app.BackupEffectiveProjection, application, service, systemIdentifier string) string {
	if current.LastEffective == nil {
		return systemIdentifier
	}
	previousRoot := app.WalgPrefix(current.LastEffective.Target, application, service, "")
	nextRoot := app.WalgPrefix(projection.Target, application, service, "")
	sameDatabase := current.DatabaseSystemIdentifier != "" && current.DatabaseSystemIdentifier == systemIdentifier
	if previousRoot == nextRoot && sameDatabase {
		return current.BackupRepositoryGeneration
	}
	return systemIdentifier
}

// previousBackupRepository is the repository a service was last archiving to,
// or empty when it has never been enabled.
func previousBackupRepository(state BackupLifecycleState, application, service string) string {
	if state.LastEffective == nil {
		return ""
	}
	return app.WalgPrefix(state.LastEffective.Target, application, service, state.BackupRepositoryGeneration)
}

// retiresCredentialFile reports whether the previous binding left a credential
// file this one will not overwrite.
//
// The file is named for the target, so a move to a differently named target
// strands the old one. Editing the bucket inside a target keeps the name and
// therefore the path, and retiring it there deletes the file the same run just
// installed — which is what happened: the next command that needed the
// repository failed with "--env-file: no such file or directory".
func retiresCredentialFile(previous *app.BackupEffectiveProjection, next app.BackupEffectiveProjection) bool {
	return previous != nil && previous.Policy.Target != next.Policy.Target
}

// disablementAfterFailedEnablement computes the two records the revert writes:
// the pending one that covers the restart, and the disabled one that replaces
// it once the restart has happened.
//
// Separated from the writing so the transition can be judged without a target:
// what matters is that the epoch never repeats or goes backwards — it is the
// fence, and an operation launched against the state this run is leaving must
// not still be accepted against the state it is leaving it in.
func disablementAfterFailedEnablement(enabled BackupLifecycleState, operationID string, now time.Time) (pending, disabled BackupLifecycleState, err error) {
	pending, err = BeginBackupDisable(enabled, operationID, now, enabled.Epoch+1)
	if err != nil {
		return BackupLifecycleState{}, BackupLifecycleState{}, err
	}
	disabled, err = DisableBackup(pending, operationID, pending.Epoch+1)
	if err != nil {
		return BackupLifecycleState{}, BackupLifecycleState{}, err
	}
	return pending, disabled, nil
}
