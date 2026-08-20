package onebox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

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
	projection, err := resolved.EffectiveBackupProjection(service)
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

	image, err := e.ResolveProtectedImage(ctx, service)
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
	// Rebound every time, including when the service is already enabled.
	//
	// `ob backup enable` is the one command that binds a service to a
	// repository, and re-running it after editing the policy or the target is
	// how an operator moves it. Skipping the transition when already enabled
	// discarded the freshly pinned image and the new projection, so the service
	// went on archiving to the original repository with no command able to
	// change it — while the edited project sat there looking applied.
	next, err := rebindBackup(current, projection, image, operationID)
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
		return fmt.Errorf("service %s could not restart under backup: %w", service, err)
	}
	return e.BackupService(ctx, service)
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
func rebindBackup(current BackupLifecycleState, projection app.BackupEffectiveProjection, image, operationID string) (BackupLifecycleState, error) {
	source := current
	if current.State == BackupEnabled {
		source.State = BackupDisabled
		source.Phase = BackupPhaseIdle
	}
	return EnableBackup(source, projection, image, operationID, true, current.Epoch+1)
}
