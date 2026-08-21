package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// engineFromReleaseSnapshot returns a shallow engine copy whose deployment
// choreography comes from the immutable snapshot staged with releaseID.
// Recovery must never substitute the working-tree config: it may describe a
// different set of roles, ordering, strategies, hooks, or verification checks.
func (e *Engine) engineFromReleaseSnapshot(ctx context.Context, releaseID string) (*Engine, error) {
	return e.engineFromReleaseSnapshotFor(ctx, releaseID, "recovery")
}

// engineFromReleaseSnapshotFor lets non-recovery lifecycle operations replay
// the same immutable declaration without reporting themselves as recovery.
// operation is always an internal literal used only in refusal text.
func (e *Engine) engineFromReleaseSnapshotFor(ctx context.Context, releaseID, operation string) (*Engine, error) {
	names := e.names()
	environment := e.Opts.Environment
	if environment == "" {
		environment = e.Spec.Env
	}
	path := release.PathsFor(names).Releases + "/" + releaseID + "/ob.snapshot.yml"
	res, err := e.T.Run(ctx, "cat "+q(path))
	if err != nil {
		return nil, fmt.Errorf("read release %s snapshot: %w", releaseID, err)
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail != "" {
			detail = ": " + detail
		}
		return nil, fmt.Errorf("%s refused: release %s snapshot unavailable (exit %d)%s", operation, releaseID, res.ExitCode, detail)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return nil, fmt.Errorf("%s refused: release %s snapshot is empty", operation, releaseID)
	}

	snapshot, err := app.LoadBytes([]byte(res.Stdout), path)
	if err != nil {
		return nil, fmt.Errorf("%s refused: release %s snapshot unusable: %w", operation, releaseID, err)
	}
	resolved, err := snapshot.Resolve(environment)
	if err != nil {
		return nil, fmt.Errorf("%s refused: release %s snapshot unusable: %w", operation, releaseID, err)
	}
	if got := resolved.NamesFor(environment); got != names {
		return nil, fmt.Errorf("%s refused: release %s snapshot resolves to app %q at %q, expected app %q at %q",
			operation, releaseID, got.App, got.BasePath, names.App, names.BasePath)
	}

	replay := *e
	replay.Spec = resolved
	return &replay, nil
}

type recoveryRequest struct {
	InterruptedID string
	PreviousID    string
	TerminalState release.State
	GateCovered   bool
	BreakGlass    bool
	Phase         string
	Journal       *journal.Writer
}

// RecoveryIncompleteError is returned only after recovery authority was
// accepted but a cleanup, restore, verification, or finalization postcondition
// failed. The checkpoint remains durable and retryable.
type RecoveryIncompleteError struct {
	Phase string
	Err   error
}

func (err *RecoveryIncompleteError) Error() string {
	return fmt.Sprintf("recovery incomplete during %s: %v", err.Phase, err.Err)
}

func (err *RecoveryIncompleteError) Unwrap() error { return err.Err }
func (err *RecoveryIncompleteError) Code() string  { return "recovery_incomplete" }

// MigrationGateClosedError is raised by every command the gate stops — resume
// and abort alike — so the code is deliberately command-neutral. Refused names
// the command that was refused: those two are what an operator is choosing
// between, so the sentence has to say which one it is talking about, and the
// registry message is worded to fit either.
type MigrationGateClosedError struct {
	InterruptedID string
	Refused       string
}

func (err *MigrationGateClosedError) Error() string {
	refused := err.Refused
	if refused == "" {
		refused = "recovery"
	}
	return fmt.Sprintf("%s refused — HALT-AND-PAGE: deploy %s ran a job or lifecycle hook with rollback-unknown data effects not covered by a safe result or migration_policy. Fix-forward + `ob resume`, or `ob abort --break-migration-gate` if you know the data is compatible", refused, err.InterruptedID)
}

func (err *MigrationGateClosedError) Code() string { return "migration_gate_closed" }

// GuidanceCommand overrides the registry default per refused command. The
// structured envelope never renders Error(), so without this an operator who
// ran `ob abort` is handed `ob resume` — the opposite action — and an agent
// that executes resolving commands fixes forward instead of rolling back.
func (err *MigrationGateClosedError) GuidanceCommand() string {
	if err.Refused == "abort" {
		// Deliberately NOT `ob abort --break-migration-gate`. That string
		// classifies as a resolving command, which an agent may run — and
		// running it rolls back a release with rollback-unknown data effects,
		// which is the decision this gate exists to put in front of a human.
		// The break-glass flag is named in Error(), for a person to read.
		return "ob audit --output json"
	}
	return "ob resume --output ndjson"
}

func (e *Engine) recoverInterrupted(ctx context.Context, request recoveryRequest) (err error) {
	if !request.GateCovered && !request.BreakGlass {
		return &MigrationGateClosedError{InterruptedID: request.InterruptedID, Refused: "recovery"}
	}
	if request.Journal == nil || request.InterruptedID == "" ||
		(request.TerminalState != release.StateFailed && request.TerminalState != release.StateAborted) {
		return fmt.Errorf("recovery request is invalid")
	}
	var previous *Engine
	if request.PreviousID != "" {
		if err := e.requireRecoverableApplicationManifest(ctx, request.PreviousID); err != nil {
			return err
		}
		previous, err = e.engineFromReleaseSnapshot(ctx, request.PreviousID)
		if err != nil {
			return err
		}
		previous.fenceVal = e.fenceVal
	}
	checkpoint, checkpointErr := release.ReadActivationCheckpoint(ctx, e.T, e.names())
	checkpointPresent := checkpointErr == nil
	if checkpointErr != nil && !errors.Is(checkpointErr, release.ErrActivationCheckpointMissing) {
		return fmt.Errorf("recovery checkpoint: %w", checkpointErr)
	}
	if checkpointPresent && checkpoint.ReleaseID != request.InterruptedID {
		return fmt.Errorf("recovery checkpoint belongs to %s, not interrupted release %s", checkpoint.ReleaseID, request.InterruptedID)
	}

	if err := request.Journal.Append(ctx, journal.Record{Phase: request.Phase, Event: "intent", Detail: "to=" + request.PreviousID}); err != nil {
		return fmt.Errorf("journal %s intent: %w", request.Phase, err)
	}
	defer func() {
		event := "result"
		if request.Phase == "abort" {
			event = "abort"
		}
		result := journal.Record{Phase: request.Phase, Event: event, Status: "ok"}
		if err != nil {
			result.Status = "fail"
			result.Detail = err.Error()
		}
		if journalErr := request.Journal.Append(ctx, result); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal %s result: %w", request.Phase, journalErr))
		}
	}()

	if err := e.removeExactReleaseContainers(ctx, request.InterruptedID); err != nil {
		return &RecoveryIncompleteError{Phase: "cleanup", Err: err}
	}
	if previous != nil {
		if err := e.restoreReleaseRoles(ctx, previous, request.PreviousID); err != nil {
			return &RecoveryIncompleteError{Phase: "restore", Err: err}
		}
		if err := previous.Verify(ctx); err != nil {
			return &RecoveryIncompleteError{Phase: "verify", Err: err}
		}
	}
	if err := e.assertNoReleaseContainers(ctx, request.InterruptedID); err != nil {
		return &RecoveryIncompleteError{Phase: "cleanup-postcondition", Err: err}
	}
	if err := e.finalizeRecoveredRelease(ctx, request.InterruptedID, request.PreviousID, request.TerminalState); err != nil {
		return &RecoveryIncompleteError{Phase: "state-finalization", Err: err}
	}
	if err := e.clearActivationCheckpoint(ctx); err != nil {
		return &RecoveryIncompleteError{Phase: "checkpoint-finalization", Err: err}
	}
	return nil
}

func (e *Engine) exactReleaseContainerIDs(ctx context.Context, releaseID string) ([]string, error) {
	result, err := e.T.Run(ctx, "docker ps -aq --filter label=ob.app="+q(e.Spec.Name)+" --filter label=ob.release="+q(releaseID))
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("list interrupted-release containers failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return splitIDs(result.Stdout)
}

func (e *Engine) removeExactReleaseContainers(ctx context.Context, releaseID string) error {
	ids, err := e.exactReleaseContainerIDs(ctx, releaseID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		command := "if docker inspect " + id + " >/dev/null 2>&1; then docker rm -f " + id + "; fi"
		if err := e.mutateChecked(ctx, "remove interrupted container "+id, command); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) assertNoReleaseContainers(ctx context.Context, releaseID string) error {
	ids, err := e.exactReleaseContainerIDs(ctx, releaseID)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		return fmt.Errorf("interrupted release %s still owns containers %v", releaseID, ids)
	}
	return nil
}

func (e *Engine) restoreReleaseRoles(ctx context.Context, previous *Engine, previousID string) error {
	composePath := release.PathsFor(e.names()).Releases + "/" + previousID + "/compose.yaml"
	for _, roleName := range previous.Spec.ReleaseOrder() {
		role := previous.Spec.Workloads[roleName]
		if role.Mode() == "recreate" {
			ids, err := previous.newcomerIDs(ctx, roleName, previousID)
			if err != nil {
				return err
			}
			if len(ids) > 0 {
				continue
			}
		}
		var err error
		if role.Mode() == "rolling" {
			err = previous.RollRole(ctx, roleName, composePath)
		} else {
			err = previous.RecreateRole(ctx, roleName, composePath)
		}
		if err != nil {
			return fmt.Errorf("restore %s: %w", roleName, err)
		}
	}
	return nil
}

func (e *Engine) finalizeRecoveredRelease(ctx context.Context, interruptedID, previousID string, terminalState release.State) error {
	current, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return err
	}
	if previousID == "" {
		if current == interruptedID {
			if err := e.mutateChecked(ctx, "clear interrupted current release", "rm -f "+q(release.PathsFor(e.names()).Current)); err != nil {
				return err
			}
		} else if current != "" {
			return fmt.Errorf("current release %s is neither the interrupted first release nor empty", current)
		}
	} else {
		if current != previousID && current != interruptedID {
			return fmt.Errorf("current release %s does not match interrupted %s or predecessor %s", current, interruptedID, previousID)
		}
		previous, err := release.ReadManifest(ctx, e.T, e.names(), previousID)
		if err != nil {
			return fmt.Errorf("previous release manifest: %w", err)
		}
		if previous.Kind != release.KindApplication || (previous.State != release.StateServing && previous.State != release.StateSuperseded) {
			return fmt.Errorf("previous release %s cannot be restored from state %s/%s", previousID, previous.Kind, previous.State)
		}
		if current == interruptedID {
			if err := e.activate(ctx, previousID); err != nil {
				return err
			}
		}
		if previous.State == release.StateSuperseded {
			if err := previous.Transition(release.StateServing, e.Opts.Now(), previous.Predecessor); err != nil {
				return err
			}
			if err := e.writeReleaseManifest(ctx, previous); err != nil {
				return err
			}
		}
	}

	manifest, err := release.ReadManifest(ctx, e.T, e.names(), interruptedID)
	if err != nil {
		return fmt.Errorf("interrupted release manifest: %w", err)
	}
	switch manifest.State {
	case release.StateStaged, release.StateVerified:
		if err := manifest.Transition(terminalState, e.Opts.Now(), ""); err != nil {
			return err
		}
	case release.StateServing:
		if err := manifest.Transition(release.StateSuperseded, e.Opts.Now(), ""); err != nil {
			return err
		}
		fallthrough
	case release.StateSuperseded:
		outcome := release.OutcomeFailed
		if terminalState == release.StateAborted {
			outcome = release.OutcomeAborted
		}
		if err := manifest.RecordOperationOutcome(outcome, e.Opts.Now()); err != nil {
			return err
		}
	case release.StateFailed, release.StateAborted:
		return nil
	default:
		return fmt.Errorf("interrupted release %s has unrecoverable manifest state %s", interruptedID, manifest.State)
	}
	return e.writeReleaseManifest(ctx, manifest)
}
