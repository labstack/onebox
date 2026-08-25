package onebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/pmezard/go-difflib/difflib"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/secrets"
)

const executablePlanTTL = 15 * time.Minute

type PlanDeployRequest struct{}

// PlanDeploy materializes the local, executable deployment plan used by the
// CLI. Unlike the read-only MCP proposal, this local operation may decrypt
// SOPS while staging; decrypted bytes are removed before the method returns
// and never appear in the returned plan.
func (s *Service) PlanDeploy(ctx context.Context, _ PlanDeployRequest) (DeployPlan, error) {
	now := s.now().UTC()
	lp, err := s.loadProject(ctx, false)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("load project: %w", err)
	}
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return DeployPlan{}, err
	}
	environmentConfig, err := lp.resolved.Environment(s.environment)
	if err != nil {
		return DeployPlan{}, err
	}
	if err := enforceRunnerPolicy(environmentConfig.Policy, s.runner, ExecutableDeployPlanSchemaVersion); err != nil {
		return DeployPlan{}, err
	}
	if err := lp.resolved.Spec.RunPreflight(filepath.Dir(lp.configPath)); err != nil {
		return DeployPlan{}, err
	}
	e, cleanup, target, err := s.engine(ctx, lp, s.environment)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()

	hostState, err := e.Refresh(ctx)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("refresh: %w", err)
	}
	pins, err := e.PinImages(ctx)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("pin images: %w", err)
	}
	gitSHA := gitShortSHA(ctx, filepath.Dir(lp.configPath))
	releaseID := s.newOperationID(now, gitSHA, KindDeploy)
	liveRedacted, liveComposeDigest, err := readLiveComposeState(ctx, e, hostState.CurrentRelease)
	if err != nil {
		return DeployPlan{}, err
	}
	secretGeneration := ""
	activeSecretGeneration := ""
	secretGraph := lp.resolved.SecretDeclarationGraph()
	if len(secretGraph) > 0 {
		if hostState.CurrentRelease != "" {
			activeSecretGeneration, err = e.DeployedSecretGeneration(ctx, hostState.CurrentRelease, []byte(liveRedacted))
			if err != nil {
				return DeployPlan{}, fmt.Errorf("read live secret generation: %w", err)
			}
			if activeSecretGeneration != "" && !release.IsSecretGeneration(activeSecretGeneration) {
				return DeployPlan{}, fmt.Errorf("live secret generation %q is invalid", activeSecretGeneration)
			}
		}
		secretGeneration = activeSecretGeneration
		if secretGeneration == "" {
			secretGeneration, err = s.newSecretGeneration()
			if err != nil {
				return DeployPlan{}, fmt.Errorf("create initial secret generation: %w", err)
			}
		}
	}
	staging, cleanupStaging, err := stageExecution(ctx, lp, s.environment, releaseID, secretGeneration, pins)
	if err != nil {
		return DeployPlan{}, err
	}
	defer func() { cleanupStaging() }()

	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return DeployPlan{}, fmt.Errorf("read staged compose: %w", err)
	}
	renderedRedacted, err := compose.RedactEnvYAML(rendered)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("redact staged compose: %w", err)
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(liveRedacted), B: difflib.SplitLines(string(renderedRedacted)),
		FromFile: "live (" + noneIfEmpty(hostState.CurrentRelease) + ")",
		ToFile:   "planned (" + releaseID + ")", Context: 3,
	})
	if err != nil {
		return DeployPlan{}, fmt.Errorf("build compose diff: %w", err)
	}
	if engine.OnlyReleaseLabelsChanged(liveRedacted, string(renderedRedacted)) {
		diff = ""
	}
	payloadDigest, err := engine.LocalPayloadDigestContext(ctx, lp.resolved, staging)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("hash planned payload: %w", err)
	}
	noOp := false
	livePayloadDigest := ""
	if hostState.CurrentRelease != "" {
		livePayloadDigest, err = e.RemotePayloadDigest(ctx, hostState.CurrentRelease)
		if err != nil {
			return DeployPlan{}, fmt.Errorf("hash live payload: %w", err)
		}
		if livePayloadDigest == "" {
			return DeployPlan{}, errors.New("hash live payload: empty digest")
		}
		if diff == "" || engine.OnlyReleaseLabelsChanged(liveRedacted, string(renderedRedacted)) {
			noOp = livePayloadDigest == payloadDigest
		}
	}
	// Reusing the active generation is what allows a truly unchanged plan to be
	// a no-op. Once any bound input differs, the release gets a fresh opaque
	// generation and is restaged so new secret bytes can never masquerade under
	// the old identity.
	if !noOp && activeSecretGeneration != "" {
		cleanupStaging()
		secretGeneration, err = s.newSecretGeneration()
		if err != nil {
			return DeployPlan{}, fmt.Errorf("create replacement secret generation: %w", err)
		}
		newStaging, newCleanup, stageErr := stageExecution(ctx, lp, s.environment, releaseID, secretGeneration, pins)
		if stageErr != nil {
			return DeployPlan{}, stageErr
		}
		staging, cleanupStaging = newStaging, newCleanup
		rendered, err = os.ReadFile(filepath.Join(staging, "compose.yaml"))
		if err != nil {
			return DeployPlan{}, fmt.Errorf("read restaged compose: %w", err)
		}
		renderedRedacted, err = compose.RedactEnvYAML(rendered)
		if err != nil {
			return DeployPlan{}, fmt.Errorf("redact restaged compose: %w", err)
		}
		diff, err = difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: difflib.SplitLines(liveRedacted), B: difflib.SplitLines(string(renderedRedacted)),
			FromFile: "live (" + noneIfEmpty(hostState.CurrentRelease) + ")",
			ToFile:   "planned (" + releaseID + ")", Context: 3,
		})
		if err != nil {
			return DeployPlan{}, fmt.Errorf("build restaged compose diff: %w", err)
		}
		payloadDigest, err = engine.LocalPayloadDigestContext(ctx, lp.resolved, staging)
		if err != nil {
			return DeployPlan{}, fmt.Errorf("hash restaged payload: %w", err)
		}
	}

	configDigest := engine.HashBytes(lp.configBytes)
	steps, err := DeploymentGraph(lp.resolved, releaseID)
	if err != nil {
		return DeployPlan{}, err
	}
	steps, err = planWorkloadActions(lp.resolved, steps, string(renderedRedacted), hostState, noOp)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("plan workload actions: %w", err)
	}
	workloadPlans := engineWorkloadPlans(steps)
	commands := e.DescribeWorkloadPlans(
		release.PathsFor(e.Names()).Releases+"/"+releaseID+"/compose.yaml",
		workloadPlans,
	)
	artifact := engine.Artifact{
		ID: releaseID, App: lp.resolved.Name, Env: s.environment, CreatedAt: now,
		GitSHA: gitSHA, ConfigHash: configDigest, HostState: hostState,
		PinnedImages: pins, BuildImages: buildImagesFor(lp, s.images),
		SecretGeneration: secretGeneration,
		RenderedCompose:  string(renderedRedacted),
		Commands:         commands,
	}
	stateDigest, err := artifactDigest(artifact)
	if err != nil {
		return DeployPlan{}, err
	}
	migrationBackup, err := migrationBackupRequirement(lp.resolved, environmentConfig.Policy, steps)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("build migration backup requirement: %w", err)
	}
	risk, reversibility, approval := classifyDeploymentForPolicy(
		steps,
		hostState.CurrentRelease,
		environmentConfig.Policy.RequireApproval,
	)
	operation := OperationPlan{
		SchemaVersion: OperationPlanSchemaVersion,
		ID:            releaseID,
		Kind:          KindDeploy,
		ReleaseID:     releaseID,
		CreatedAt:     now.Format(time.RFC3339Nano),
		ExpiresAt:     now.Add(executablePlanTTL).Format(time.RFC3339Nano),
		Risk:          risk,
		Reversibility: reversibility,
		Approval:      approval,
		Binding: OperationBinding{
			Application: lp.resolved.Name, Environment: s.environment, Server: target,
			ConfigDigest: configDigest, ComposeDigest: engine.HashBytes(lp.composeBytes),
			StateDigest: stateDigest, PayloadDigest: payloadDigest,
			LiveComposeDigest: liveComposeDigest, LivePayloadDigest: livePayloadDigest,
		},
		Steps: steps,
	}
	if err := operation.Seal(); err != nil {
		return DeployPlan{}, fmt.Errorf("seal deployment operation: %w", err)
	}
	plan := DeployPlan{
		SchemaVersion: ExecutableDeployPlanSchemaVersion,
		Runner:        s.runner,
		Operation:     operation, Artifact: artifact, Diff: diff, NoOp: noOp,
		MigrationBackup: migrationBackup,
	}
	if err := plan.Seal(); err != nil {
		return DeployPlan{}, fmt.Errorf("seal executable deployment plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return DeployPlan{}, fmt.Errorf("validate deployment plan: %w", err)
	}
	return plan, nil
}

func classifyDeployment(steps []OperationStep, currentRelease string) (RiskClass, ReversibilityClass, ApprovalClass) {
	for _, step := range steps {
		if step.DataEffect == DataEffectDestructive {
			return RiskCritical, ReversibilityIrreversible, ApprovalStrong
		}
	}
	for _, step := range steps {
		if step.DataEffect == DataEffectMigration || step.DataEffect == DataEffectUnknown {
			return RiskHigh, ReversibilityConditional, ApprovalStrong
		}
	}
	if currentRelease == "" {
		return RiskModerate, ReversibilityConditional, ApprovalOneTime
	}
	return RiskModerate, ReversibilityReversible, ApprovalOneTime
}

func classifyDeploymentForPolicy(steps []OperationStep, currentRelease string, approvalRequired bool) (RiskClass, ReversibilityClass, ApprovalClass) {
	risk, reversibility, approval := classifyDeployment(steps, currentRelease)
	if !approvalRequired && risk != RiskCritical {
		approval = ApprovalNone
	}
	return risk, reversibility, approval
}

func readLiveComposeState(ctx context.Context, e *engine.Engine, currentRelease string) (string, string, error) {
	if currentRelease == "" {
		return "", "", nil
	}
	path := release.PathsFor(e.Names()).Releases + "/" + currentRelease + "/compose.yaml"
	res, err := e.T.Run(ctx, "cat "+quote(path)+" 2>/dev/null")
	if err != nil {
		return "", "", fmt.Errorf("read live compose: %w", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return "", "", fmt.Errorf("read live compose: release %q is unavailable", currentRelease)
	}
	redacted, err := compose.RedactEnvYAML([]byte(res.Stdout))
	if err != nil {
		return "", "", fmt.Errorf("redact live compose: %w", err)
	}
	return string(redacted), engine.HashBytes([]byte(res.Stdout)), nil
}

// stageExecution builds the release payload: the generated runtime, the
// project snapshot that lets a rollback replay the choreography that shipped,
// any decrypted secrets, and the files a workload's env_files and bind mounts
// reference.
//
// Nothing is injected here — not env files, not the proxy network, not secrets
// env. Generation emits all three from the declaration, so there is nothing to
// patch into a document afterwards, and no second place where the runtime can
// differ from what `ob preview` showed.
func stageExecution(ctx context.Context, lp *loadedProject, environment, releaseID, secretGeneration string, images app.Images) (string, func(), error) {
	staging, err := os.MkdirTemp("", "ob-"+lp.resolved.Name)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(staging) }
	fail := func(err error) (string, func(), error) {
		cleanup()
		return "", nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	// Mark generated secret files before staging ordinary project payload. Root
	// bind mounts copy the whole project, so the generated files are written
	// afterwards and cannot be replaced by stale files from the source tree.
	entries := encryptedEntries(lp.resolved)
	externalProjections := externalConnectionProjections(lp.resolved)
	if (len(entries) > 0 || len(externalProjections) > 0) && secretGeneration == "" {
		return fail(errors.New("secret generation is required for an encrypted runtime"))
	}
	projected := map[string]bool{}
	for _, entry := range entries {
		projected[entry.StagedPath()] = true
	}
	for _, projection := range externalProjections {
		projected[projection.Path] = true
	}
	rendered, err := lp.resolved.Render(environment, releaseID, images)
	if err != nil {
		return fail(err)
	}
	rewrites, err := compose.StagePayloadContext(ctx, lp.compose, staging, projected)
	if err != nil {
		return fail(err)
	}
	// A root bind may have copied a repository-local directory using Onebox's
	// reserved generation name. Generated state owns this subtree completely;
	// discard it before writing the bound generation so stale secret files never
	// ride into a release as unreferenced payload.
	if err := os.RemoveAll(filepath.Join(staging, app.SecretGenerationDirectory)); err != nil {
		return fail(err)
	}
	for generatedPath := range projected {
		if err := os.Remove(filepath.Join(staging, filepath.FromSlash(generatedPath))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
	}
	// Every encrypted entry is decrypted into its own file, at the name the
	// generated document references. One shared file would make a later entry
	// win outright instead of key by key, which is not what a list means.
	for _, entry := range entries {
		envBytes, err := secrets.RenderContext(ctx, filepath.Dir(lp.configPath), entry.File)
		if err != nil {
			return fail(err)
		}
		secretPath := filepath.Join(staging, filepath.FromSlash(app.SecretGenerationPath(secretGeneration, entry.StagedPath())))
		if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
			return fail(err)
		}
		// WriteFile preserves an existing file's mode. A root bind may have
		// copied a stale placeholder here, so remove that staged copy first and
		// create the decrypted file with the required private permissions.
		if err := os.Remove(secretPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		if err := os.WriteFile(secretPath, envBytes, 0o600); err != nil {
			return fail(err)
		}
	}
	decryptedSources := map[string][]byte{}
	for _, projection := range externalProjections {
		cacheKey := projection.Source.Provider + "\x00" + projection.Source.File
		source, ok := decryptedSources[cacheKey]
		if !ok {
			source, err = secrets.RenderContext(ctx, filepath.Dir(lp.configPath), projection.Source.File)
			if err != nil {
				return fail(err)
			}
			decryptedSources[cacheKey] = source
		}
		envBytes, err := secrets.ProjectEnvironment(source, projection.Entries)
		if err != nil {
			return fail(fmt.Errorf("project external connection %s: %w", projection.Path, err))
		}
		secretPath := filepath.Join(staging, filepath.FromSlash(app.SecretGenerationPath(secretGeneration, projection.Path)))
		if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
			return fail(err)
		}
		if err := os.Remove(secretPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		if err := os.WriteFile(secretPath, envBytes, 0o600); err != nil {
			return fail(err)
		}
	}
	body := compose.RewriteSources(rendered.Bytes, rewrites)
	if len(lp.resolved.SecretDeclarationGraph()) > 0 {
		body, err = app.ApplySecretGeneration(body, lp.resolved.SecretDeclarationGraph(), secretGeneration)
		if err != nil {
			return fail(err)
		}
	}
	if err := release.Stage(staging, body, lp.configBytes); err != nil {
		return fail(err)
	}
	if secretGeneration != "" {
		generationCompose := filepath.Join(staging, filepath.FromSlash(app.SecretGenerationPath(secretGeneration, "compose.yaml")))
		if err := os.WriteFile(generationCompose, body, 0o600); err != nil {
			return fail(err)
		}
	}
	return staging, cleanup, nil
}

func externalConnectionProjections(r *app.Resolved) []app.ExternalConnectionProjection {
	var out []app.ExternalConnectionProjection
	for _, workloadName := range sortedNames(r.Workloads) {
		out = append(out, r.Spec.ExternalConnectionProjections(workloadName, r.Workloads[workloadName])...)
	}
	return out
}

// applyPinnedImages records the plan's resolved digests on the parsed runtime
// so host comparisons see what the plan pinned.
//
// The pins also go into generation, which is what actually decides the image a
// release runs. Patching only the parsed copy would leave the staged runtime
// on mutable tags while every report claimed a digest.
func applyPinnedImages(project *ctypes.Project, pins map[string]string) {
	for service, image := range pins {
		if image == "" {
			continue
		}
		if component, ok := project.Services[service]; ok {
			component.Image = image
			project.Services[service] = component
		}
	}
}

// buildImagesFor records image inputs the authored project cannot reproduce on
// its own: native build-sourced workloads and adopted Compose services that
// carry a build source. These inputs are replayed before the execution binding
// is checked; digest pins are applied only after that exact authored runtime is
// re-derived.
func buildImagesFor(lp *loadedProject, images app.Images) map[string]string {
	if len(images) == 0 {
		return nil
	}
	out := map[string]string{}
	for name, ref := range images {
		w, ok := lp.resolved.Spec.Workloads[name]
		if !ok {
			continue
		}
		composeBuild := false
		if w.Compose != "" {
			if service, present := lp.compose.Services[name]; present {
				composeBuild = service.Build != nil
			}
		}
		if w.Build != nil || composeBuild {
			out[name] = ref
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
