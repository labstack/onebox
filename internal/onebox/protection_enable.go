package onebox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
)

// executeProtectionEnable turns a declared policy into an established one.
//
// The order is not a preference. PostgreSQL cannot archive to a stanza that
// does not exist, and pgBackRest cannot create a stanza against a server that
// is not already archiving — so the sequence is forced: check the credentials,
// pin the image, record the state that makes rendering produce a protected
// server, restart under it, then initialise the repository.
//
// It is not finished until a base backup exists. A stanza with no backup is a
// repository that can recover nothing, and a command that returned success
// there would be telling the operator their database is protected at the exact
// moment it is not — which is the failure this whole product is arranged to
// refuse.
func executeProtectionEnable(ctx context.Context, e *engine.Engine, resolved *app.Resolved, environment, service, operationID string) error {
	if service == "" {
		return fmt.Errorf("protection enable requires a service name")
	}
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
			"service %s runs the %s driver; executable protection exists for postgres only today", service, driver)
	}
	if declared.Protection == nil {
		return fmt.Errorf(
			"service %s declares no protection policy; add services.%s.protection to the project first", service, service)
	}
	projection, err := resolved.EffectiveProtectionProjection(service)
	if err != nil {
		return err
	}
	if _, ok := app.LifecycleCredentialSlots(driver, resolved.DeclaredVersion(service)); !ok {
		return fmt.Errorf("service %s runs a %s version with no qualified protection contract", service, driver)
	}

	// Before anything restarts. A missing entry discovered after the server is
	// already running with archive_mode on is a database whose WAL cannot
	// drain, which is a much worse place to learn it.
	if err := e.VerifyProtectionCredentials(ctx, service, declared.Protection.Target,
		app.PgBackRestCredentialEntries(projection.Target)); err != nil {
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
	current, err := currentProtectionLifecycleState(ctx, e, resolved.Spec.Name, environment, service)
	if err != nil {
		return err
	}
	next, err := EnableProtection(current, projection, image, operationID, true, current.Epoch+1)
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
	// The project was loaded before this service was protected, so without
	// rebinding the very next render would produce the unprotected server this
	// run started from and quietly undo what was just recorded.
	runtime := next.RuntimeState()
	runtime.DigestAvailable = true
	if err := e.RebindServiceRuntimeStates(map[string]app.ServiceRuntimeState{service: runtime}); err != nil {
		return err
	}
	if err := e.ApplyServices(ctx); err != nil {
		return fmt.Errorf("service %s could not restart under protection: %w", service, err)
	}
	if err := e.CreateProtectionStanza(ctx, service); err != nil {
		return err
	}
	return e.BackupService(ctx, service, "full")
}

// encodeProtectionLifecycleState renders a validated record as the single JSON
// line the observation probe expects: it reads the marker from the first line
// and the record from the rest, and refuses more than one JSON value.
func encodeProtectionLifecycleState(state ProtectionLifecycleState) ([]byte, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// currentProtectionLifecycleState returns the record the target holds, or a
// fresh never-enabled one when protection has never been established here.
//
// The starting epoch is 1 rather than 0 because the schema treats a
// non-positive epoch as unsealed state: an epoch of 0 is how an uninitialised
// record is told apart from a real one, so it cannot also be a real one.
func currentProtectionLifecycleState(ctx context.Context, e *engine.Engine, application, environment, service string) (ProtectionLifecycleState, error) {
	encoded, err := e.ReadProtectionLifecycleState(ctx, service)
	if err != nil {
		return ProtectionLifecycleState{}, err
	}
	if len(encoded) == 0 {
		return NewProtectionLifecycleState(application, environment, service, 1)
	}
	state, err := DecodeProtectionLifecycleState(encoded)
	if err != nil {
		return ProtectionLifecycleState{}, fmt.Errorf("service %s lifecycle state: %w", service, err)
	}
	if state.Application != application || state.Environment != environment || state.Service != service {
		return ProtectionLifecycleState{}, fmt.Errorf("service %s lifecycle state belongs to a different protected identity", service)
	}
	return state, nil
}
