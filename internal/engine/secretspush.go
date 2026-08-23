package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

type SecretDeclarationDriftError struct {
	CurrentRelease string
}

func (err *SecretDeclarationDriftError) Error() string {
	return fmt.Sprintf("secret declaration graph differs from current release %s — run `ob deploy` before `ob secrets push`", err.CurrentRelease)
}

func (err *SecretDeclarationDriftError) Code() string { return "secret_declaration_not_deployed" }

type SecretRecoveryIncompleteError struct {
	ReleaseID string
	Cause     error
}

func (err *SecretRecoveryIncompleteError) Error() string {
	return fmt.Sprintf("secret generation recovery for release %s is incomplete; retry `ob secrets push`: %v", err.ReleaseID, err.Cause)
}

func (err *SecretRecoveryIncompleteError) Unwrap() error { return err.Cause }
func (err *SecretRecoveryIncompleteError) Code() string  { return "secret_recovery_incomplete" }

// SecretRotationRolledBackError reports that an interrupted rotation was
// restored to its prior generation and the requested payload was not applied.
type SecretRotationRolledBackError struct{ ReleaseID string }

func (err *SecretRotationRolledBackError) Error() string {
	return fmt.Sprintf("the incomplete secret rotation for release %s was rolled back safely; rerun `ob secrets push` to apply the requested payload", err.ReleaseID)
}

func (err *SecretRotationRolledBackError) Code() string { return "secret_rotation_rolled_back" }

// SecretCleanupPendingError reports a rotation that committed and verified the
// new generation but could not finish removing the retired one or clearing the
// checkpoint. The rotation itself succeeded, so rolling live workloads back to
// the old generation would undo applied secrets to fix a housekeeping failure —
// and if the old generation directory is already gone, that rollback can never
// succeed and the checkpoint wedges permanently. The checkpoint is left in
// place instead. The checkpoint is cleared before the sweep, so a rerun
// normally finishes through the orphan sweep at the start of the next push;
// only a failure to clear it leaves a committed checkpoint to resume.
type SecretCleanupPendingError struct {
	ReleaseID string
	Cause     error
}

func (err *SecretCleanupPendingError) Error() string {
	return fmt.Sprintf("secret rotation for release %s is applied but its cleanup is incomplete; rerun `ob secrets push` to finish it: %v", err.ReleaseID, err.Cause)
}

func (err *SecretCleanupPendingError) Unwrap() error { return err.Cause }
func (err *SecretCleanupPendingError) Code() string  { return "secret_cleanup_pending" }

type SecretGenerationNotDeployedError struct{ CurrentRelease string }

func (err *SecretGenerationNotDeployedError) Error() string {
	return fmt.Sprintf("current release %s has no opaque secret generation — run `ob deploy` before `ob secrets push`", err.CurrentRelease)
}

func (err *SecretGenerationNotDeployedError) Code() string { return "secret_generation_not_deployed" }

type SecretPayload struct {
	Path  string
	Bytes []byte
}

type SecretPushResult struct {
	ReleaseID  string
	Generation string
	NoOp       bool
}

// SecretsPushBatch rotates one complete declaration graph. It never exposes a
// content hash: comparison happens with cmp on the host after an opaque upload,
// and journals contain generation identifiers only.
func (e *Engine) SecretsPushBatch(ctx context.Context, payloads []SecretPayload) (result SecretPushResult, err error) {
	if err := e.RequireHostOwner(ctx); err != nil {
		return result, err
	}
	current, err := release.Current(ctx, e.T, e.names())
	result.ReleaseID = current
	if err != nil {
		return result, err
	}
	if current == "" {
		return result, errors.New("nothing deployed yet — secrets ship with `ob deploy`")
	}
	runtimeEngine, err := e.currentSecretEngine(ctx, current)
	if err != nil {
		return result, err
	}
	graph := runtimeEngine.Spec.SecretDeclarationGraph()
	paths, err := validateSecretPayloads(graph, payloads)
	if err != nil {
		return result, err
	}
	workloads := affectedLiveSecretWorkloads(runtimeEngine.Spec, graph)

	epoch, err := runtimeEngine.AcquireLock(ctx, current, runtimeEngine.Opts.ForceLock)
	if err != nil {
		return result, err
	}
	defer runtimeEngine.ReleaseLock(ctx)
	if err := runtimeEngine.WriteFence(ctx, current, epoch); err != nil {
		return result, err
	}
	if err := runtimeEngine.cleanupSecretUploads(ctx); err != nil {
		return result, err
	}
	jw := &journal.Writer{T: runtimeEngine.T, Names: runtimeEngine.names(), DeployID: current, Epoch: epoch, Operator: journal.DefaultOperator(), Runner: &runtimeEngine.Opts.Runner}
	journalStarted := false
	startJournal := func(detail string) error {
		if err := jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "start", Detail: detail}); err != nil {
			return fmt.Errorf("journal secrets push start: %w", err)
		}
		journalStarted = true
		return nil
	}
	defer func() {
		if !journalStarted {
			return
		}
		finish := journal.Record{Phase: "secrets-push", Event: "finish", Status: "ok", Detail: "generation=" + result.Generation}
		var cleanupPending *SecretCleanupPendingError
		switch {
		case errors.As(err, &cleanupPending):
			// The transition committed and verified; only the sweep is
			// outstanding. Recording this as a failure would make the durable
			// evidence assert the opposite of what happened.
			finish.Detail = "generation=" + result.Generation + " cleanup=pending"
		case err != nil:
			finish.Status = "fail"
			finish.Detail = "generation transition failed"
		}
		if journalErr := jw.Append(ctx, finish); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal secrets push finish: %w", journalErr))
		}
	}()

	checkpoint, checkpointErr := release.ReadSecretCheckpoint(ctx, runtimeEngine.T, runtimeEngine.names())
	if checkpointErr == nil {
		if checkpoint.ReleaseID != current || !slices.Equal(checkpoint.AffectedWorkloads, workloads) || !slices.Equal(checkpoint.PayloadPaths, paths) {
			return result, &SecretRecoveryIncompleteError{ReleaseID: current, Cause: errors.New("durable secret checkpoint does not match the current release declaration graph")}
		}
		result.Generation = checkpoint.NewGeneration
		if err := startJournal("resume old_generation=" + checkpoint.OldGeneration + " new_generation=" + checkpoint.NewGeneration); err != nil {
			return result, err
		}
		if checkpoint.Phase == release.SecretRecovering {
			err = runtimeEngine.recoverSecretGeneration(ctx, &checkpoint)
			if err == nil {
				result.Generation = checkpoint.OldGeneration
				err = &SecretRotationRolledBackError{ReleaseID: current}
			}
		} else {
			err = runtimeEngine.advanceSecretGeneration(ctx, &checkpoint)
			var cleanupPending *SecretCleanupPendingError
			switch {
			case err == nil:
			case errors.As(err, &cleanupPending):
				// Applied and verified; only housekeeping is outstanding. Keep the
				// new generation as the result and let the next push finish the sweep.
			default:
				transitionErr := err
				if recoveryErr := runtimeEngine.recoverSecretGeneration(ctx, &checkpoint); recoveryErr != nil {
					err = &SecretRecoveryIncompleteError{ReleaseID: current, Cause: errors.Join(transitionErr, recoveryErr)}
					return result, err
				}
				result.Generation = checkpoint.OldGeneration
				err = transitionErr
			}
		}
		if err != nil {
			return result, err
		}
		return result, nil
	}
	if !errors.Is(checkpointErr, release.ErrSecretCheckpointMissing) {
		return result, &SecretRecoveryIncompleteError{ReleaseID: current, Cause: checkpointErr}
	}

	currentCompose, err := runtimeEngine.readReleaseCompose(ctx, current)
	if err != nil {
		return result, err
	}
	allAffected := affectedSecretWorkloads(graph)
	oldGeneration, err := app.SecretGenerationFromCompose(currentCompose, allAffected)
	if err != nil {
		return result, fmt.Errorf("read current secret generation: %w", err)
	}
	if oldGeneration == "" {
		return result, &SecretGenerationNotDeployedError{CurrentRelease: current}
	}
	if !release.IsSecretGeneration(oldGeneration) {
		return result, fmt.Errorf("current runtime secret generation %q is invalid", oldGeneration)
	}
	// A crash after candidate installation but before checkpoint publication can
	// leave plaintext outside recovery's durable graph. With no checkpoint, the
	// committed runtime's generation is the only one that may survive.
	if err := runtimeEngine.cleanupOrphanSecretGenerations(ctx, current, oldGeneration); err != nil {
		return result, err
	}
	newGeneration, err := runtimeEngine.freshSecretGeneration(oldGeneration)
	if err != nil {
		return result, err
	}
	result.Generation = newGeneration

	staging, cleanup, err := stageSecretGeneration(payloads, currentCompose, graph, newGeneration)
	if err != nil {
		return result, err
	}
	defer cleanup()
	remoteUpload := runtimeEngine.base() + "/.secret-upload-" + newGeneration
	if err := runtimeEngine.T.Upload(ctx, staging, remoteUpload); err != nil {
		return result, err
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := runtimeEngine.mutateChecked(cleanupContext, "clean secret upload", "rm -rf "+q(remoteUpload)); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean secret upload: %w", cleanupErr))
		}
	}()

	match, err := runtimeEngine.secretPayloadsMatch(ctx, current, oldGeneration, remoteUpload, paths)
	if err != nil {
		return result, err
	}
	if match {
		result.Generation = oldGeneration
		result.NoOp = true
		runtimeEngine.logf("secrets unchanged — nothing to do")
		return result, nil
	}
	if err := startJournal("old_generation=" + oldGeneration + " new_generation=" + newGeneration); err != nil {
		return result, err
	}
	if err := runtimeEngine.installSecretGenerationCandidates(ctx, current, oldGeneration, newGeneration, remoteUpload); err != nil {
		return result, err
	}
	checkpoint, err = release.NewSecretCheckpoint(current, oldGeneration, newGeneration, workloads, paths, runtimeEngine.Opts.Now())
	if err != nil {
		return result, err
	}
	if err := runtimeEngine.writeSecretCheckpoint(ctx, checkpoint); err != nil {
		cleanupErr := runtimeEngine.removeSecretGeneration(ctx, current, newGeneration)
		return result, errors.Join(err, cleanupErr)
	}

	if err = runtimeEngine.advanceSecretGeneration(ctx, &checkpoint); err == nil {
		return result, nil
	}
	// A cleanup failure after a verified commit is not a failed rotation. The
	// requested payload is live, so recovering would undo it; the operator is told
	// to rerun, which finishes the sweep.
	var cleanupPending *SecretCleanupPendingError
	if errors.As(err, &cleanupPending) {
		return result, err
	}
	transitionErr := err
	if recoveryErr := runtimeEngine.recoverSecretGeneration(ctx, &checkpoint); recoveryErr != nil {
		err = &SecretRecoveryIncompleteError{ReleaseID: current, Cause: errors.Join(transitionErr, recoveryErr)}
		return result, err
	}
	result.Generation = oldGeneration
	err = transitionErr
	return result, err
}

func (e *Engine) currentSecretEngine(ctx context.Context, current string) (*Engine, error) {
	releaseEngine, err := e.engineFromReleaseSnapshot(ctx, current)
	if err != nil {
		return nil, fmt.Errorf("read current secret declaration graph: %w", err)
	}
	if !reflect.DeepEqual(e.Spec.SecretDeclarationGraph(), releaseEngine.Spec.SecretDeclarationGraph()) {
		return nil, &SecretDeclarationDriftError{CurrentRelease: current}
	}
	return releaseEngine, nil
}

// DeployedSecretGeneration reads the active generation against the immutable
// declaration graph that produced the deployed release. A new project may add
// or remove secret-consuming workloads; using its graph to inspect the old
// runtime mistakes that intentional transition for partial live state.
func (e *Engine) DeployedSecretGeneration(ctx context.Context, releaseID string, composeBytes []byte) (string, error) {
	releaseEngine, err := e.engineFromReleaseSnapshotFor(ctx, releaseID, "deploy planning")
	if err != nil {
		return "", err
	}
	graph := releaseEngine.Spec.SecretDeclarationGraph()
	workloads := affectedLiveSecretWorkloads(releaseEngine.Spec, graph)
	return app.SecretGenerationFromCompose(composeBytes, workloads)
}

func (e *Engine) cleanupSecretUploads(ctx context.Context) error {
	return e.mutateChecked(ctx, "clean abandoned secret uploads", "rm -rf "+q(e.base()+"/.secret-upload-")+"*")
}

func validateSecretPayloads(graph []app.SecretDeclaration, payloads []SecretPayload) ([]string, error) {
	expected := map[string]bool{}
	for _, declaration := range graph {
		if !canonicalRelativeSecretPath(declaration.OutputPath) {
			return nil, fmt.Errorf("deployed secret path %q is unsafe", declaration.OutputPath)
		}
		expected[declaration.OutputPath] = true
	}
	provided := map[string]bool{}
	for _, payload := range payloads {
		if !canonicalRelativeSecretPath(payload.Path) || !expected[payload.Path] {
			return nil, fmt.Errorf("secret payload path %q is outside the deployed declaration graph", payload.Path)
		}
		if provided[payload.Path] {
			return nil, fmt.Errorf("secret payload path %q is duplicated", payload.Path)
		}
		provided[payload.Path] = true
	}
	if len(provided) != len(expected) {
		return nil, fmt.Errorf("secret push must provide the complete deployed declaration graph")
	}
	paths := make([]string, 0, len(expected))
	for value := range expected {
		paths = append(paths, value)
	}
	sort.Strings(paths)
	return paths, nil
}

func canonicalRelativeSecretPath(value string) bool {
	return value != "" && value != "." && !path.IsAbs(value) && path.Clean(value) == value &&
		value != ".." && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\\x00")
}

func affectedSecretWorkloads(graph []app.SecretDeclaration) []string {
	set := map[string]bool{}
	for _, declaration := range graph {
		for _, workload := range declaration.AffectedWorkloads {
			set[workload] = true
		}
	}
	return sortedSet(set)
}

func affectedLiveSecretWorkloads(spec *app.Resolved, graph []app.SecretDeclaration) []string {
	set := map[string]bool{}
	for _, workload := range affectedSecretWorkloads(graph) {
		if spec.Workloads[workload].Role != app.RoleJob {
			set[workload] = true
		}
	}
	return sortedSet(set)
}

func sortedSet(set map[string]bool) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func randomSecretGeneration() (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "sg-" + hex.EncodeToString(bytes), nil
}

func (e *Engine) freshSecretGeneration(exclude string) (string, error) {
	for range 3 {
		generation, err := e.Opts.SecretGeneration()
		if err != nil {
			return "", fmt.Errorf("create opaque secret generation: %w", err)
		}
		if release.IsSecretGeneration(generation) && generation != exclude {
			return generation, nil
		}
	}
	return "", errors.New("secret generation source returned invalid or repeated identifiers")
}

func stageSecretGeneration(payloads []SecretPayload, currentCompose []byte, graph []app.SecretDeclaration, newGeneration string) (string, func(), error) {
	directory, err := os.MkdirTemp("", "ob-secret-generation")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	fail := func(err error) (string, func(), error) { cleanup(); return "", nil, err }
	for _, payload := range payloads {
		file := filepath.Join(directory, "payload", filepath.FromSlash(payload.Path))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(file, payload.Bytes, 0o600); err != nil {
			return fail(err)
		}
	}
	newCompose, err := app.ApplySecretGeneration(currentCompose, graph, newGeneration)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "compose.new.yaml"), newCompose, 0o600); err != nil {
		return fail(err)
	}
	return directory, cleanup, nil
}

func (e *Engine) readReleaseCompose(ctx context.Context, releaseID string) ([]byte, error) {
	composePath := release.PathsFor(e.names()).Releases + "/" + releaseID + "/compose.yaml"
	result, err := e.T.Run(ctx, "cat "+q(composePath))
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) == "" {
		return nil, fmt.Errorf("read current release runtime failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return []byte(result.Stdout), nil
}

func (e *Engine) secretPayloadsMatch(ctx context.Context, releaseID, oldGeneration, upload string, paths []string) (bool, error) {
	releaseDir := release.PathsFor(e.names()).Releases + "/" + releaseID
	oldDir, err := secretGenerationDir(releaseDir, oldGeneration)
	if err != nil {
		return false, err
	}
	commands := make([]string, 0, len(paths))
	for _, payloadPath := range paths {
		commands = append(commands, "cmp -s "+q(path.Join(upload, "payload", payloadPath))+" "+q(path.Join(oldDir, payloadPath)))
	}
	result, err := e.T.Run(ctx, strings.Join(commands, " && "))
	if err != nil {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("compare secret generation failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
}

func secretGenerationDir(releaseDir, generation string) (string, error) {
	if !release.IsSecretGeneration(generation) {
		return "", fmt.Errorf("secret generation %q is invalid", generation)
	}
	return releaseDir + "/" + app.SecretGenerationDirectory + "/" + generation, nil
}

func (e *Engine) installSecretGenerationCandidates(ctx context.Context, releaseID, oldGeneration, newGeneration, upload string) error {
	releaseDir := release.PathsFor(e.names()).Releases + "/" + releaseID
	newDir, err := secretGenerationDir(releaseDir, newGeneration)
	if err != nil {
		return err
	}
	commands := []string{
		"mkdir -p " + q(newDir),
		"cp -R " + q(upload+"/payload/.") + " " + q(newDir+"/"),
		"cp " + q(upload+"/compose.new.yaml") + " " + q(newDir+"/compose.yaml"),
		"find " + q(newDir) + " -type f -exec chmod 600 {} +",
	}
	oldDir, err := secretGenerationDir(releaseDir, oldGeneration)
	if err != nil {
		return err
	}
	commands = append(commands, "test -d "+q(oldDir), "cp "+q(releaseDir+"/compose.yaml")+" "+q(oldDir+"/compose.yaml"))
	commands = append(commands, "find "+q(oldDir)+" -type f -exec chmod 600 {} +")
	if err := e.mutateChecked(ctx, "prepare secret generations", strings.Join(commands, " && ")); err != nil {
		return err
	}
	return nil
}

func (e *Engine) writeSecretCheckpoint(ctx context.Context, checkpoint release.SecretCheckpoint) error {
	command, input, err := release.SecretCheckpointWrite(e.names(), checkpoint)
	if err != nil {
		return err
	}
	result, err := e.mutateInput(ctx, command, input)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write secret checkpoint failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (e *Engine) clearSecretCheckpoint(ctx context.Context) error {
	return e.mutateChecked(ctx, "clear secret checkpoint", "rm -f "+q(release.SecretCheckpointPath(e.names())))
}

func (e *Engine) advanceSecretGeneration(ctx context.Context, checkpoint *release.SecretCheckpoint) error {
	if checkpoint.Phase == release.SecretCommitted {
		if err := e.verifySecretGeneration(ctx, *checkpoint, checkpoint.NewGeneration); err != nil {
			return err
		}
		return e.finishSecretCleanup(ctx, checkpoint)
	}
	if checkpoint.Phase == release.SecretPrepared || checkpoint.Phase == release.SecretReplacing {
		if err := checkpoint.SetPhase(release.SecretReplacing, secretCheckpointTime(*checkpoint, e.Opts.Now())); err != nil {
			return err
		}
		if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
			return err
		}
		for _, workload := range checkpoint.AffectedWorkloads {
			if slices.Contains(checkpoint.ReplacedWorkloads, workload) {
				continue
			}
			if err := e.forceSecretGeneration(ctx, *checkpoint, workload, checkpoint.NewGeneration); err != nil {
				return fmt.Errorf("replace %s on secret generation %s: %w", workload, checkpoint.NewGeneration, err)
			}
			if err := checkpoint.MarkReplaced(workload, secretCheckpointTime(*checkpoint, e.Opts.Now())); err != nil {
				return err
			}
			if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
				return err
			}
		}
		if err := checkpoint.SetPhase(release.SecretVerifying, secretCheckpointTime(*checkpoint, e.Opts.Now())); err != nil {
			return err
		}
		if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
			return err
		}
	}
	if checkpoint.Phase == release.SecretVerifying {
		if err := e.verifySecretGeneration(ctx, *checkpoint, checkpoint.NewGeneration); err != nil {
			return err
		}
		if err := checkpoint.SetPhase(release.SecretCommitting, secretCheckpointTime(*checkpoint, e.Opts.Now())); err != nil {
			return err
		}
		if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
			return err
		}
	}
	if checkpoint.Phase == release.SecretCommitting {
		if err := e.commitSecretCompose(ctx, *checkpoint, checkpoint.NewGeneration); err != nil {
			return err
		}
		if err := checkpoint.SetPhase(release.SecretCommitted, secretCheckpointTime(*checkpoint, e.Opts.Now())); err != nil {
			return err
		}
		if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
			return err
		}
	}
	if checkpoint.Phase != release.SecretCommitted {
		return fmt.Errorf("secret checkpoint phase %q cannot advance", checkpoint.Phase)
	}
	if err := e.verifySecretGeneration(ctx, *checkpoint, checkpoint.NewGeneration); err != nil {
		return err
	}
	return e.finishSecretCleanup(ctx, checkpoint)
}

// finishSecretCleanup runs the two steps that follow a verified commit. Both are
// idempotent — `rm -rf` of an already-removed generation and clearing an
// already-cleared checkpoint both succeed — so either step may be repeated.
func (e *Engine) finishSecretCleanup(ctx context.Context, checkpoint *release.SecretCheckpoint) error {
	// The checkpoint is cleared before the retired generation is swept, not
	// after. The other order leaves a window where the old generation is gone
	// while the checkpoint still names it: a retry then re-enters the committed
	// phase, and any failure there is not a cleanup error, so it falls through
	// to recovery and tries to roll back onto a directory that no longer
	// exists — wedging the checkpoint permanently.
	//
	// Clearing first cannot strand the generation: cleanupOrphanSecretGenerations
	// sweeps any generation the committed runtime does not reference at the
	// start of the next push, which is exactly the crash window this opens.
	if err := e.clearSecretCheckpoint(ctx); err != nil {
		return &SecretCleanupPendingError{ReleaseID: checkpoint.ReleaseID, Cause: err}
	}
	if err := e.removeSecretGeneration(ctx, checkpoint.ReleaseID, checkpoint.OldGeneration); err != nil {
		return &SecretCleanupPendingError{ReleaseID: checkpoint.ReleaseID, Cause: err}
	}
	return nil
}

func (e *Engine) recoverSecretGeneration(ctx context.Context, checkpoint *release.SecretCheckpoint) error {
	if phaseErr := checkpoint.SetPhase(release.SecretRecovering, secretCheckpointTime(*checkpoint, e.Opts.Now())); phaseErr != nil {
		return phaseErr
	}
	if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
		return err
	}
	for _, workload := range checkpoint.AffectedWorkloads {
		if err := e.forceSecretGeneration(ctx, *checkpoint, workload, checkpoint.OldGeneration); err != nil {
			return fmt.Errorf("restore %s to old secret generation: %w", workload, err)
		}
	}
	if err := e.verifySecretGeneration(ctx, *checkpoint, checkpoint.OldGeneration); err != nil {
		return err
	}
	if err := e.commitSecretCompose(ctx, *checkpoint, checkpoint.OldGeneration); err != nil {
		return err
	}
	if err := e.verifySecretGeneration(ctx, *checkpoint, checkpoint.OldGeneration); err != nil {
		return err
	}
	if err := e.removeSecretGeneration(ctx, checkpoint.ReleaseID, checkpoint.NewGeneration); err != nil {
		return err
	}
	return e.clearSecretCheckpoint(ctx)
}

func secretCheckpointTime(checkpoint release.SecretCheckpoint, now time.Time) time.Time {
	previous, err := time.Parse(time.RFC3339Nano, checkpoint.UpdatedAt)
	if err == nil && now.UTC().Before(previous) {
		return previous
	}
	return now.UTC()
}

func (e *Engine) removeSecretGeneration(ctx context.Context, releaseID, generation string) error {
	if !release.IsID(releaseID) || !release.IsSecretGeneration(generation) {
		return errors.New("refusing to remove an invalid secret generation path")
	}
	releaseDir := release.PathsFor(e.names()).Releases + "/" + releaseID
	generationDir, err := secretGenerationDir(releaseDir, generation)
	if err != nil {
		return err
	}
	return e.mutateChecked(ctx, "remove retired secret generation", "rm -rf "+q(generationDir))
}

func (e *Engine) cleanupOrphanSecretGenerations(ctx context.Context, releaseID string, keep ...string) error {
	if !release.IsID(releaseID) {
		return errors.New("refusing to sweep secret generations for an invalid release")
	}
	root := release.PathsFor(e.names()).Releases + "/" + releaseID + "/" + app.SecretGenerationDirectory
	command := "if [ -d " + q(root) + " ]; then find " + q(root) + " -mindepth 1 -maxdepth 1 -type d"
	for _, generation := range keep {
		if !release.IsSecretGeneration(generation) {
			return fmt.Errorf("refusing to preserve invalid secret generation %q", generation)
		}
		command += " ! -name " + q(generation)
	}
	command += " -exec rm -rf -- {} +; fi"
	return e.mutateChecked(ctx, "clean orphaned secret generations", command)
}

func (e *Engine) forceSecretGeneration(ctx context.Context, checkpoint release.SecretCheckpoint, workload, generation string) error {
	before, err := e.containerIDs(ctx, workload)
	if err != nil {
		return err
	}
	releaseDir := release.PathsFor(e.names()).Releases + "/" + checkpoint.ReleaseID
	generationDir, err := secretGenerationDir(releaseDir, generation)
	if err != nil {
		return err
	}
	composePath := generationDir + "/compose.yaml"
	if err := e.recreateRoleForRelease(ctx, workload, composePath, checkpoint.ReleaseID); err != nil {
		return err
	}
	after, err := e.containerIDs(ctx, workload)
	if err != nil {
		return err
	}
	if len(after) != e.Spec.Workloads[workload].Count() {
		return fmt.Errorf("workload has %d containers after replacement, want %d", len(after), e.Spec.Workloads[workload].Count())
	}
	old := map[string]bool{}
	for _, identifier := range before {
		old[identifier] = true
	}
	for _, identifier := range after {
		if old[identifier] {
			return fmt.Errorf("container %s identity did not change", identifier)
		}
		if err := e.requireContainerSecretGeneration(ctx, identifier, generation); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) requireContainerSecretGeneration(ctx context.Context, containerID, generation string) error {
	result, err := e.T.Run(ctx, "docker inspect -f '{{ index .Config.Labels \"ob.secret-generation\" }}' "+containerID)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != generation {
		return fmt.Errorf("container %s did not adopt secret generation %s", containerID, generation)
	}
	return nil
}

func (e *Engine) verifySecretGeneration(ctx context.Context, checkpoint release.SecretCheckpoint, generation string) error {
	releaseDir := release.PathsFor(e.names()).Releases + "/" + checkpoint.ReleaseID
	generationDir, err := secretGenerationDir(releaseDir, generation)
	if err != nil {
		return err
	}
	checks := make([]string, 0, len(checkpoint.PayloadPaths)+1)
	checks = append(checks, "test -f "+q(generationDir+"/compose.yaml"))
	for _, payloadPath := range checkpoint.PayloadPaths {
		checks = append(checks, "test -f "+q(path.Join(generationDir, payloadPath)))
	}
	result, err := e.T.Run(ctx, strings.Join(checks, " && "))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("secret generation %s staged files are incomplete", generation)
	}
	for _, workload := range checkpoint.AffectedWorkloads {
		containers, err := e.containerIDs(ctx, workload)
		if err != nil {
			return err
		}
		if len(containers) != e.Spec.Workloads[workload].Count() {
			return fmt.Errorf("workload %s is not fully running", workload)
		}
		for _, container := range containers {
			if err := e.requireContainerSecretGeneration(ctx, container, generation); err != nil {
				return err
			}
		}
	}
	if err := e.Verify(ctx); err != nil {
		return fmt.Errorf("verify secret generation: %w", err)
	}
	return nil
}

func (e *Engine) commitSecretCompose(ctx context.Context, checkpoint release.SecretCheckpoint, generation string) error {
	releaseDir := release.PathsFor(e.names()).Releases + "/" + checkpoint.ReleaseID
	generationDir, err := secretGenerationDir(releaseDir, generation)
	if err != nil {
		return err
	}
	source := generationDir + "/compose.yaml"
	temporary := releaseDir + "/compose.yaml.secret-tmp"
	command := "cp " + q(source) + " " + q(temporary) + " && chmod 600 " + q(temporary) + " && mv -f " + q(temporary) + " " + q(releaseDir+"/compose.yaml")
	return e.mutateChecked(ctx, "commit secret generation runtime", command)
}
