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
	"github.com/labstack/onebox/internal/proxy"
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
	staging, cleanupStaging, err := stageExecution(ctx, lp, s.environment, releaseID, pins)
	if err != nil {
		return DeployPlan{}, err
	}
	defer cleanupStaging()

	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return DeployPlan{}, fmt.Errorf("read staged compose: %w", err)
	}
	renderedRedacted, err := compose.RedactEnvYAML(rendered)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("redact staged compose: %w", err)
	}
	liveRedacted, liveComposeDigest, err := readLiveComposeState(ctx, e, hostState.CurrentRelease)
	if err != nil {
		return DeployPlan{}, err
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
	payloadDigest, err := engine.LocalPayloadDigestContext(ctx, staging)
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

	configDigest := engine.HashBytes(lp.configBytes)
	commands := e.Describe(release.PathsFor(e.Names()).Releases + "/" + releaseID + "/compose.yaml")
	artifact := engine.Artifact{
		ID: releaseID, App: lp.resolved.Name, Env: s.environment, CreatedAt: now,
		GitSHA: gitSHA, ConfigHash: configDigest, HostState: hostState,
		PinnedImages: pins, RenderedCompose: string(renderedRedacted),
		Commands: commands,
	}
	stateDigest, err := artifactDigest(artifact)
	if err != nil {
		return DeployPlan{}, err
	}
	steps, err := DeploymentGraph(lp.resolved, releaseID)
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
			Application: lp.resolved.Name, Environment: s.environment, Target: target,
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
	if !approvalRequired {
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
// The injection steps this used to perform — env files, the proxy network,
// secrets env — are gone. Generation emits all three from the declaration, so
// there is nothing left to patch into a document afterwards, and no second
// place where the runtime can differ from what `ob preview` showed.
func stageExecution(ctx context.Context, lp *loadedProject, environment, releaseID string, images app.Images) (string, func(), error) {
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
	if source := sopsSource(lp.resolved); source != "" {
		envBytes, err := secrets.RenderContext(ctx, filepath.Dir(lp.configPath), source)
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(filepath.Join(staging, secrets.EnvFileName), envBytes, 0o600); err != nil {
			return fail(err)
		}
	}
	rendered, err := lp.resolved.Render(environment, releaseID, images)
	if err != nil {
		return fail(err)
	}
	rewrites, err := compose.StagePayloadContext(ctx, lp.compose, staging)
	if err != nil {
		return fail(err)
	}
	body := compose.RewriteSources(rendered.Bytes, rewrites)
	if err := release.Stage(staging, body, lp.configBytes); err != nil {
		return fail(err)
	}
	return staging, cleanup, nil
}

func managedProxyNetwork(cfg *app.Resolved) string {
	if cfg.Proxy.Network != "" {
		return cfg.Proxy.Network
	}
	return proxy.DefaultNetwork
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
