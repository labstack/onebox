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
	current, err := release.Current(ctx, e.T, e.names())
	result.ReleaseID = current
	if err != nil {
		return result, err
	}
	if current == "" {
		return result, errors.New("nothing deployed yet — secrets ship with `ob deploy`")
	}
	if err := e.requireCurrentSecretDeclarationGraph(ctx, current); err != nil {
		return result, err
	}
	graph := e.Spec.SecretDeclarationGraph()
	paths, err := validateSecretPayloads(graph, payloads)
	if err != nil {
		return result, err
	}
	workloads := affectedLiveSecretWorkloads(e.Spec, graph)

	epoch, err := e.AcquireLock(ctx, current, e.Opts.ForceLock)
	if err != nil {
		return result, err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, current, epoch); err != nil {
		return result, err
	}
	jw := &journal.Writer{T: e.T, Names: e.names(), DeployID: current, Epoch: epoch, Operator: journal.DefaultOperator(), Runner: &e.Opts.Runner}
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
		if err != nil {
			finish.Status = "fail"
			finish.Detail = "generation transition failed"
		}
		if journalErr := jw.Append(ctx, finish); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal secrets push finish: %w", journalErr))
		}
	}()

	checkpoint, checkpointErr := release.ReadSecretCheckpoint(ctx, e.T, e.names())
	if checkpointErr == nil {
		if checkpoint.ReleaseID != current || !reflect.DeepEqual(checkpoint.AffectedWorkloads, workloads) || !reflect.DeepEqual(checkpoint.PayloadPaths, paths) {
			return result, &SecretRecoveryIncompleteError{ReleaseID: current, Cause: errors.New("durable secret checkpoint does not match the current release declaration graph")}
		}
		result.Generation = checkpoint.NewGeneration
		if err := startJournal("resume old_generation=" + checkpoint.OldGeneration + " new_generation=" + checkpoint.NewGeneration); err != nil {
			return result, err
		}
		if checkpoint.Phase == release.SecretRecovering {
			err = e.recoverSecretGeneration(ctx, &checkpoint)
			if err == nil {
				result.Generation = checkpoint.OldGeneration
			}
		} else {
			err = e.advanceSecretGeneration(ctx, &checkpoint)
			if err != nil {
				transitionErr := err
				if recoveryErr := e.recoverSecretGeneration(ctx, &checkpoint); recoveryErr != nil {
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

	currentCompose, err := e.readReleaseCompose(ctx, current)
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
	newGeneration, err := e.freshSecretGeneration(oldGeneration)
	if err != nil {
		return result, err
	}
	result.Generation = newGeneration

	staging, cleanup, err := stageSecretGeneration(payloads, currentCompose, graph, newGeneration)
	if err != nil {
		return result, err
	}
	defer cleanup()
	remoteUpload := e.base() + "/.secret-upload-" + newGeneration
	if err := e.T.Upload(ctx, staging, remoteUpload); err != nil {
		return result, err
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := e.mutateChecked(cleanupContext, "clean secret upload", "rm -rf "+q(remoteUpload)); cleanupErr != nil {
			e.warnf("clean secret upload: %v", cleanupErr)
		}
	}()

	match, err := e.secretPayloadsMatch(ctx, current, oldGeneration, remoteUpload, paths)
	if err != nil {
		return result, err
	}
	if match {
		result.Generation = oldGeneration
		result.NoOp = true
		e.logf("secrets unchanged — nothing to do")
		return result, nil
	}
	if err := startJournal("old_generation=" + oldGeneration + " new_generation=" + newGeneration); err != nil {
		return result, err
	}
	if err := e.installSecretGenerationCandidates(ctx, current, oldGeneration, newGeneration, remoteUpload); err != nil {
		return result, err
	}
	checkpoint, err = release.NewSecretCheckpoint(current, oldGeneration, newGeneration, workloads, paths, e.Opts.Now())
	if err != nil {
		return result, err
	}
	if err := e.writeSecretCheckpoint(ctx, checkpoint); err != nil {
		return result, err
	}

	if err = e.advanceSecretGeneration(ctx, &checkpoint); err == nil {
		return result, nil
	}
	transitionErr := err
	if recoveryErr := e.recoverSecretGeneration(ctx, &checkpoint); recoveryErr != nil {
		err = &SecretRecoveryIncompleteError{ReleaseID: current, Cause: errors.Join(transitionErr, recoveryErr)}
		return result, err
	}
	result.Generation = oldGeneration
	err = transitionErr
	return result, err
}

func (e *Engine) requireCurrentSecretDeclarationGraph(ctx context.Context, current string) error {
	releaseEngine, err := e.engineFromReleaseSnapshot(ctx, current)
	if err != nil {
		return fmt.Errorf("read current secret declaration graph: %w", err)
	}
	if !reflect.DeepEqual(e.Spec.SecretDeclarationGraph(), releaseEngine.Spec.SecretDeclarationGraph()) {
		return &SecretDeclarationDriftError{CurrentRelease: current}
	}
	return nil
}

func validateSecretPayloads(graph []app.SecretDeclaration, payloads []SecretPayload) ([]string, error) {
	expected := map[string]bool{}
	for _, declaration := range graph {
		expected[declaration.OutputPath] = true
	}
	provided := map[string]bool{}
	for _, payload := range payloads {
		if payload.Path == "" || filepath.IsAbs(payload.Path) || strings.Contains(payload.Path, "..") || !expected[payload.Path] {
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
	for attempt := 0; attempt < 3; attempt++ {
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
	oldDir := secretGenerationDir(releaseDir, oldGeneration)
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

func secretGenerationDir(releaseDir, generation string) string {
	return releaseDir + "/" + app.SecretGenerationDirectory + "/" + generation
}

func (e *Engine) installSecretGenerationCandidates(ctx context.Context, releaseID, oldGeneration, newGeneration, upload string) error {
	releaseDir := release.PathsFor(e.names()).Releases + "/" + releaseID
	newDir := secretGenerationDir(releaseDir, newGeneration)
	commands := []string{
		"mkdir -p " + q(newDir),
		"cp -R " + q(upload+"/payload/.") + " " + q(newDir+"/"),
		"cp " + q(upload+"/compose.new.yaml") + " " + q(newDir+"/compose.yaml"),
		"find " + q(newDir) + " -type f -exec chmod 600 {} +",
	}
	oldDir := secretGenerationDir(releaseDir, oldGeneration)
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
		return e.clearSecretCheckpoint(ctx)
	}
	if err := checkpoint.SetPhase(release.SecretReplacing, e.Opts.Now()); err != nil {
		return err
	}
	if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
		return err
	}
	for _, workload := range checkpoint.AffectedWorkloads {
		if err := e.forceSecretGeneration(ctx, *checkpoint, workload, checkpoint.NewGeneration); err != nil {
			return fmt.Errorf("replace %s on secret generation %s: %w", workload, checkpoint.NewGeneration, err)
		}
		if err := checkpoint.MarkReplaced(workload, e.Opts.Now()); err != nil {
			return err
		}
		if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
			return err
		}
	}
	if err := checkpoint.SetPhase(release.SecretVerifying, e.Opts.Now()); err != nil {
		return err
	}
	if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
		return err
	}
	if err := e.verifySecretGeneration(ctx, *checkpoint, checkpoint.NewGeneration); err != nil {
		return err
	}
	if err := checkpoint.SetPhase(release.SecretCommitting, e.Opts.Now()); err != nil {
		return err
	}
	if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
		return err
	}
	if err := e.commitSecretCompose(ctx, *checkpoint, checkpoint.NewGeneration); err != nil {
		return err
	}
	if err := checkpoint.SetPhase(release.SecretCommitted, e.Opts.Now()); err != nil {
		return err
	}
	if err := e.writeSecretCheckpoint(ctx, *checkpoint); err != nil {
		return err
	}
	if err := e.verifySecretGeneration(ctx, *checkpoint, checkpoint.NewGeneration); err != nil {
		return err
	}
	return e.clearSecretCheckpoint(ctx)
}

func (e *Engine) recoverSecretGeneration(ctx context.Context, checkpoint *release.SecretCheckpoint) error {
	if phaseErr := checkpoint.SetPhase(release.SecretRecovering, e.Opts.Now()); phaseErr != nil {
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
	return e.clearSecretCheckpoint(ctx)
}

func (e *Engine) forceSecretGeneration(ctx context.Context, checkpoint release.SecretCheckpoint, workload, generation string) error {
	before, err := e.containerIDs(ctx, workload)
	if err != nil {
		return err
	}
	releaseDir := release.PathsFor(e.names()).Releases + "/" + checkpoint.ReleaseID
	composePath := secretGenerationDir(releaseDir, generation) + "/compose.yaml"
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
	generationDir := secretGenerationDir(releaseDir, generation)
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
	source := secretGenerationDir(releaseDir, generation) + "/compose.yaml"
	temporary := releaseDir + "/compose.yaml.secret-tmp"
	command := "cp " + q(source) + " " + q(temporary) + " && chmod 600 " + q(temporary) + " && mv -f " + q(temporary) + " " + q(releaseDir+"/compose.yaml")
	return e.mutateChecked(ctx, "commit secret generation runtime", command)
}
