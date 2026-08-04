package onebox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/release"
)

const ProposalSchemaVersion = "onebox.run/deployment-proposal/v1alpha1"

const MCPFidelityContract = "This read-only proposal records Onebox-generated deployment choreography and exact content identities. Every Compose scalar value and every operator-authored hook body is hidden from model output. Data-effect labels, protection, and observability are operator declarations; this proposal does not prove those controls are active. Lifecycle hooks remain unplannable and require local review. The MCP tool cannot execute this proposal."

func (s *Service) Propose(ctx context.Context, request ProposeRequest) (DeploymentProposal, error) {
	if request.Kind == "" {
		request.Kind = KindDeploy
	}
	if request.Kind != KindDeploy {
		return DeploymentProposal{}, fmt.Errorf("proposals for operation kind %q are not supported", request.Kind)
	}
	return s.ProposeDeploy(ctx, ProposeDeployRequest{})
}

func (s *Service) ProposeDeploy(ctx context.Context, _ ProposeDeployRequest) (DeploymentProposal, error) {
	now := s.now().UTC()
	lp, err := s.loadProject(ctx, false)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("load project: %w", err)
	}
	environment := s.environment
	if err := ensureEnvironment(lp.resolved, environment); err != nil {
		return DeploymentProposal{}, err
	}
	environmentConfig, err := lp.resolved.Environment(environment)
	if err != nil {
		return DeploymentProposal{}, err
	}
	policy := describePolicy(environmentConfig.Policy)
	if !policy.AllowAgentProposals {
		return DeploymentProposal{}, fmt.Errorf("environment %q policy does not allow agent deployment proposals", environment)
	}
	if err := enforceRunnerPolicy(environmentConfig.Policy, s.runner, ExecutableDeployPlanSchemaVersion); err != nil {
		return DeploymentProposal{}, err
	}
	if err := lp.resolved.Spec.RunPreflight(filepath.Dir(lp.configPath)); err != nil {
		return DeploymentProposal{}, err
	}
	e, cleanup, target, err := s.engine(ctx, lp, environment)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()

	hs, err := e.Refresh(ctx)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("refresh: %w", err)
	}
	if hs.CurrentRelease != "" && !safeStatusValue.MatchString(hs.CurrentRelease) {
		return DeploymentProposal{}, fmt.Errorf("target current release identifier is invalid; inspect it locally")
	}
	status, err := e.StatusSnapshot(ctx)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("observe deployment preconditions: %w", err)
	}
	status = sanitizeStatus(status)
	preconditions, err := proposalPreconditions(status, hs.CurrentRelease)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("encode status precondition: %w", err)
	}
	pins, err := e.PinImages(ctx)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("pin images: %w", err)
	}
	gitSHA := gitShortSHA(ctx, filepath.Dir(lp.configPath))
	releaseID := release.NewID(now, gitSHA)
	entropy := make([]byte, 48)
	if err := s.readEntropy(entropy); err != nil {
		return DeploymentProposal{}, fmt.Errorf("create proposal identity: %w", err)
	}
	proposalID := releaseID + "-" + hex.EncodeToString(entropy[:12])
	maskKey := entropy[16:]
	secretSourceCommitment := ""
	payloadMaterialized := true
	if lp.resolved.Secrets != nil {
		secretSource, sourceErr := encryptedSecretSource(lp)
		err = sourceErr
		if err != nil {
			return DeploymentProposal{}, err
		}
		secretSourceCommitment = opaqueCommitment(maskKey, secretSource)
		payloadMaterialized = false
		blockProposal(&preconditions, "SOPS values are not decrypted by the read-only proposal tool")
	}
	staging, cleanupStaging, err := stageProposal(ctx, lp, environment, releaseID, pins)
	if err != nil {
		return DeploymentProposal{}, err
	}
	defer cleanupStaging()
	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return DeploymentProposal{}, err
	}
	renderedComposeCommitment := opaqueCommitment(maskKey, rendered)
	localPayloadDigest, err := engine.LocalPayloadDigestContext(ctx, staging)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("hash planned payload: %w", err)
	}
	payloadCommitment := opaqueCommitment(maskKey, []byte(localPayloadDigest))
	renderedRedacted, err := compose.RedactEnvYAML(rendered)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("redact planned compose: %w", err)
	}
	renderedMasked, err := compose.MaskValuesYAML(rendered, maskKey)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("mask planned compose: %w", err)
	}

	liveRedacted := ""
	liveMasked := ""
	liveKnown := hs.CurrentRelease == ""
	warnings := []string{}
	if hs.CurrentRelease != "" {
		remoteCompose := release.PathsFor(lp.resolved.Name).Releases + "/" + hs.CurrentRelease + "/compose.yaml"
		res, runErr := e.T.Run(ctx, "cat "+quote(remoteCompose)+" 2>/dev/null")
		if runErr != nil {
			return DeploymentProposal{}, fmt.Errorf("read live compose: %w", runErr)
		}
		if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
			warnings = append(warnings, "live Compose is unavailable; diff and no-op detection were omitted")
		} else {
			redacted, redactErr := compose.RedactEnvYAML([]byte(res.Stdout))
			masked, maskErr := compose.MaskValuesYAML([]byte(res.Stdout), maskKey)
			if redactErr != nil || maskErr != nil {
				warnings = append(warnings, "live Compose could not be safely redacted; diff omitted")
			} else {
				liveRedacted = string(redacted)
				liveMasked = string(masked)
				liveKnown = true
			}
		}
	}
	diff := ""
	if liveKnown {
		diff, err = difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: difflib.SplitLines(liveMasked), B: difflib.SplitLines(string(renderedMasked)),
			FromFile: "live (" + noneIfEmpty(hs.CurrentRelease) + ")", ToFile: "planned (" + releaseID + ")", Context: 3,
		})
		if err != nil {
			return DeploymentProposal{}, err
		}
	}

	composeComparison := ComparisonFirstDeploy
	payloadComparison := ComparisonNotEvaluated
	livePayloadCommitment := ""
	if hs.CurrentRelease != "" {
		composeComparison = ComparisonUnavailable
		if liveKnown {
			composeComparison = ComparisonDifferent
			if diff == "" || engine.OnlyReleaseLabelsChanged(liveRedacted, string(renderedRedacted)) {
				composeComparison = ComparisonIdentical
			}
			if engine.OnlyReleaseLabelsChanged(liveRedacted, string(renderedRedacted)) {
				diff = ""
			}
		} else {
			blockProposal(&preconditions, "live Compose could not be observed")
		}

		remoteDigest, remoteErr := "", error(nil)
		if payloadMaterialized {
			remoteDigest, remoteErr = e.RemotePayloadDigest(ctx, hs.CurrentRelease)
		}
		if !payloadMaterialized {
			payloadComparison = ComparisonNotEvaluated
			warnings = append(warnings, "SOPS values were not decrypted; full payload comparison is intentionally omitted")
		} else if remoteErr != nil || remoteDigest == "" {
			payloadComparison = ComparisonUnavailable
			warnings = append(warnings, "live payload could not be compared; no-op detection is unavailable")
			blockProposal(&preconditions, "live payload could not be observed")
		} else {
			livePayloadCommitment = opaqueCommitment(maskKey, []byte(remoteDigest))
			payloadComparison = ComparisonDifferent
			if localPayloadDigest == remoteDigest {
				payloadComparison = ComparisonIdentical
			}
		}
	}
	noOp := composeComparison == ComparisonIdentical && payloadComparison == ComparisonIdentical

	images := make([]ImagePin, 0, len(pins))
	for service, image := range pins {
		digest := safeImageDigest(image)
		images = append(images, ImagePin{Service: service, Digest: digest, Pinned: digest != ""})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Service < images[j].Service })
	for _, image := range images {
		if !image.Pinned {
			warnings = append(warnings, fmt.Sprintf("%s remains tag-bound; exact image reference is hidden", image.Service))
			blockProposal(&preconditions, fmt.Sprintf("image %s is not pinned to an immutable digest", image.Service))
		}
	}
	if hasOperatorLifecycleHooks(lp.resolved) {
		blockProposal(&preconditions, "operator-authored deploy hooks require local review")
	}
	_, migrationJobs, unknownJobs := jobDataEffects(lp.resolved)
	if len(unknownJobs) > 0 {
		blockProposal(&preconditions, "jobs with unknown data effects require local review: "+strings.Join(unknownJobs, ", "))
	}
	if len(migrationJobs) > 0 {
		if lp.resolved.Deployment.MigrationPolicy != "expand-only" {
			blockProposal(&preconditions, "migration jobs require manual review: "+strings.Join(migrationJobs, ", "))
		} else {
			warnings = append(warnings, "expand-only migration safety is an operator declaration, not a verified guarantee")
		}
	}

	remoteCompose := release.PathsFor(lp.resolved.Name).Releases + "/" + releaseID + "/compose.yaml"
	fullCommands := e.Describe(remoteCompose)
	commands, redactedHooks := safeCommandSummary(fullCommands, lp.resolved)
	hostImageIDs := make(map[string]string, len(hs.ImageIDs))
	for service, imageID := range hs.ImageIDs {
		if safeImageID.MatchString(imageID) {
			hostImageIDs[service] = imageID
		} else {
			hostImageIDs[service] = "<invalid-image-id>"
			blockProposal(&preconditions, fmt.Sprintf("running image identity for %s is invalid", service))
		}
	}
	hostState := ProposalHostState{Host: hs.Host, CurrentRelease: hs.CurrentRelease, ImageIDs: hostImageIDs}
	configHash := engine.HashBytes(lp.configBytes)
	composeHash := engine.HashBytes(lp.composeBytes)
	stateBytes, err := json.Marshal(struct {
		Environment string                       `json:"environment"`
		Policy      EnvironmentPolicyDescription `json:"policy"`
		ConfigHash  string                       `json:"config_hash"`
		ComposeHash string                       `json:"compose_hash"`
		HostState   ProposalHostState            `json:"host_state"`
	}{Environment: environment, Policy: policy, ConfigHash: configHash, ComposeHash: composeHash, HostState: hostState})
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("encode state digest: %w", err)
	}
	stateDigest := engine.HashBytes(stateBytes)
	operationGraph, err := DeploymentGraph(lp.resolved, releaseID)
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("build operation graph: %w", err)
	}
	createdAt := now.Format(timeFormat)
	expiresAt := now.Add(15 * time.Minute).Format(timeFormat)
	proposalBytes, err := json.Marshal(struct {
		ID                        string                       `json:"id"`
		ReleaseID                 string                       `json:"release_id"`
		Application               string                       `json:"application"`
		Environment               string                       `json:"environment"`
		Policy                    EnvironmentPolicyDescription `json:"policy"`
		Target                    string                       `json:"target"`
		CreatedAt                 string                       `json:"created_at"`
		ExpiresAt                 string                       `json:"expires_at"`
		GitSHA                    string                       `json:"git_sha"`
		ConfigHash                string                       `json:"config_hash"`
		ComposeHash               string                       `json:"compose_hash"`
		StateDigest               string                       `json:"state_digest"`
		RenderedComposeCommitment string                       `json:"rendered_compose_commitment"`
		PayloadCommitment         string                       `json:"payload_commitment"`
		LivePayloadCommitment     string                       `json:"live_payload_commitment"`
		SecretSourceCommitment    string                       `json:"secret_source_commitment"`
		PayloadMaterialized       bool                         `json:"payload_materialized"`
		HostState                 ProposalHostState            `json:"host_state"`
		Preconditions             ProposalPreconditions        `json:"preconditions"`
		OperationGraph            []OperationStep              `json:"operation_graph"`
		Images                    []ImagePin                   `json:"images"`
		Commands                  []string                     `json:"commands"`
		ComposeComparison         ComparisonStatus             `json:"compose_comparison"`
		PayloadComparison         ComparisonStatus             `json:"payload_comparison"`
		NoOp                      bool                         `json:"no_op"`
	}{
		ID:                        proposalID,
		ReleaseID:                 releaseID,
		Application:               lp.resolved.Name,
		Environment:               environment,
		Policy:                    policy,
		Target:                    target,
		CreatedAt:                 createdAt,
		ExpiresAt:                 expiresAt,
		GitSHA:                    gitSHA,
		ConfigHash:                configHash,
		ComposeHash:               composeHash,
		StateDigest:               stateDigest,
		RenderedComposeCommitment: renderedComposeCommitment,
		PayloadCommitment:         payloadCommitment,
		LivePayloadCommitment:     livePayloadCommitment,
		SecretSourceCommitment:    secretSourceCommitment,
		PayloadMaterialized:       payloadMaterialized,
		HostState:                 hostState,
		Preconditions:             preconditions,
		OperationGraph:            operationGraph,
		Images:                    images,
		Commands:                  fullCommands,
		ComposeComparison:         composeComparison,
		PayloadComparison:         payloadComparison,
		NoOp:                      noOp,
	})
	if err != nil {
		return DeploymentProposal{}, fmt.Errorf("encode proposal digest: %w", err)
	}
	proposalDigest := engine.HashBytes(proposalBytes)

	return DeploymentProposal{
		SchemaVersion:             ProposalSchemaVersion,
		ID:                        proposalID,
		ReleaseID:                 releaseID,
		Application:               lp.resolved.Name,
		Environment:               environment,
		Policy:                    policy,
		Target:                    target,
		CreatedAt:                 createdAt,
		ExpiresAt:                 expiresAt,
		GitSHA:                    gitSHA,
		ConfigHash:                configHash,
		ComposeHash:               composeHash,
		StateDigest:               stateDigest,
		ProposalDigest:            proposalDigest,
		RenderedComposeCommitment: renderedComposeCommitment,
		PayloadCommitment:         payloadCommitment,
		LivePayloadCommitment:     livePayloadCommitment,
		SecretSourceCommitment:    secretSourceCommitment,
		PayloadMaterialized:       payloadMaterialized,
		HostState:                 hostState,
		Preconditions:             preconditions,
		OperationGraph:            operationGraph,
		Images:                    images,
		RenderedCompose:           string(renderedMasked),
		Diff:                      diff,
		ComposeComparison:         composeComparison,
		PayloadComparison:         payloadComparison,
		NoOp:                      noOp,
		CommandSummary:            commands,
		HookBodiesRedacted:        redactedHooks,
		FidelityContract:          MCPFidelityContract,
		Risk:                      riskSummary(lp.resolved, hs.CurrentRelease),
		Verification:              verificationSummary(lp.resolved),
		Warnings:                  warnings,
	}, nil
}

// stageProposal builds the payload a proposal describes without performing it.
// It differs from execution staging in exactly one way: a read-only proposal
// never decrypts SOPS into a temporary directory. It binds the encrypted
// source's hash and reports the missing secrets as a readiness blocker, so
// proposing a deploy cannot be a way to get the secrets out.
func stageProposal(ctx context.Context, lp *loadedProject, environment, id string, images app.Images) (string, func(), error) {
	staging, err := os.MkdirTemp("", "ob-"+lp.resolved.Name)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(staging) }
	fail := func(err error) (string, func(), error) { cleanup(); return "", nil, err }

	rendered, err := lp.resolved.Render(environment, id, images)
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

func encryptedSecretSource(lp *loadedProject) ([]byte, error) {
	path := sopsSource(lp.resolved)
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(lp.configPath), path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encrypted SOPS source: %w", err)
	}
	return contents, nil
}

func opaqueCommitment(key, content []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(content)
	return "opaque:hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
}

func safeImageDigest(reference string) string {
	const marker = "@sha256:"
	index := strings.LastIndex(reference, marker)
	if index < 0 {
		return ""
	}
	digest := reference[index+1:]
	if !safeImageID.MatchString(digest) {
		return ""
	}
	return digest
}

func gitShortSHA(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--short=7", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func safeCommandSummary(commands []string, cfg *app.Resolved) ([]string, bool) {
	sensitivePrefixes := map[string]string{}
	for _, job := range cfg.JobOrder() {
		if hook, ok := cfg.Hooks[job]; ok && hook.Run != "" {
			sensitivePrefixes["job "+job+" ("] = "job " + job + " (operator-authored command hidden)"
		}
	}
	for _, name := range []string{"pre_release", "post_release", "post_deploy"} {
		if hook, ok := cfg.Hooks[name]; ok && hook.Run != "" {
			sensitivePrefixes["hook "+name+" ("] = "hook " + name + " (operator-authored command hidden)"
		}
	}
	out := make([]string, len(commands))
	redacted := false
	for i, command := range commands {
		out[i] = command
		for prefix, fallback := range sensitivePrefixes {
			if !strings.HasPrefix(command, prefix) {
				continue
			}
			if split := strings.Index(command, "): "); split >= 0 {
				out[i] = command[:split+3] + "<operator-authored command hidden from model output>"
			} else {
				out[i] = fallback
			}
			redacted = true
			break
		}
	}
	return out, redacted
}

func blockProposal(preconditions *ProposalPreconditions, blocker string) {
	for _, existing := range preconditions.Blockers {
		if existing == blocker {
			preconditions.Ready = false
			return
		}
	}
	preconditions.Blockers = append(preconditions.Blockers, blocker)
	preconditions.Ready = false
}

func hasOperatorLifecycleHooks(cfg *app.Resolved) bool {
	for _, name := range []string{"pre_release", "post_release", "post_deploy"} {
		if hook, ok := cfg.Hooks[name]; ok && hook.Run != "" {
			return true
		}
	}
	return false
}

func jobDataEffects(cfg *app.Resolved) (none, migrations, unknown []string) {
	if len(cfg.Workloads) == 0 {
		unknown = append(unknown, cfg.JobOrder()...)
		sort.Strings(unknown)
		return none, migrations, unknown
	}
	for name, component := range cfg.Workloads {
		if component.Role != "job" {
			continue
		}
		switch component.DataEffect {
		case "none":
			none = append(none, name)
		case "migration":
			migrations = append(migrations, name)
		default:
			unknown = append(unknown, name)
		}
	}
	sort.Strings(none)
	sort.Strings(migrations)
	sort.Strings(unknown)
	return none, migrations, unknown
}

func riskSummary(cfg *app.Resolved, currentRelease string) RiskSummary {
	var recreate []string
	for name, role := range cfg.Workloads {
		if role.Mode() == "recreate" {
			recreate = append(recreate, name)
		}
	}
	sort.Strings(recreate)
	interruption := "none expected for rolling roles while the host remains healthy"
	if len(recreate) > 0 {
		interruption = "possible interruption for recreate roles: " + strings.Join(recreate, ", ")
	}
	if hasOperatorLifecycleHooks(cfg) {
		interruption += "; operator-authored hooks may add interruption and require local review"
	}
	rollback := "not available for a first deployment"
	if currentRelease != "" {
		rollback = "previous application release remains available; data rollback is separate"
	}
	noneJobs, migrationJobs, unknownJobs := jobDataEffects(cfg)
	dataEffects := "no job or lifecycle hook is declared"
	parts := make([]string, 0, 4)
	if len(noneJobs) > 0 {
		parts = append(parts, "no data effects declared for jobs: "+strings.Join(noneJobs, ", "))
	}
	if len(migrationJobs) > 0 {
		migration := "migration effects declared for jobs: " + strings.Join(migrationJobs, ", ")
		if cfg.Deployment.MigrationPolicy == "expand-only" {
			migration += " (expand-only is an operator declaration, not a verified guarantee)"
		} else {
			migration += " (manual review required)"
		}
		parts = append(parts, migration)
	}
	if len(unknownJobs) > 0 {
		parts = append(parts, "unknown data effects for jobs: "+strings.Join(unknownJobs, ", "))
	}
	if hasOperatorLifecycleHooks(cfg) {
		parts = append(parts, "lifecycle hooks may mutate data and require local review")
	}
	if len(parts) > 0 {
		dataEffects = strings.Join(parts, "; ")
	}
	return RiskSummary{ExpectedInterruption: interruption, ApplicationRollback: rollback, DataEffects: dataEffects}
}

func verificationSummary(cfg *app.Resolved) []string {
	out := make([]string, 0, len(cfg.Verification))
	for _, check := range cfg.Verification {
		switch {
		case check.URL != "":
			mode := "required"
			if check.Advisory {
				mode = "advisory"
			}
			out = append(out, fmt.Sprintf("%s URL check: %s", mode, safeVerificationOrigin(check.URL)))
		case check.HTTP != "":
			out = append(out, fmt.Sprintf("component %s HTTP check (endpoint hidden)", check.Workload))
		case check.Exec != "":
			out = append(out, fmt.Sprintf("component %s exec check (command hidden)", check.Workload))
		}
	}
	return out
}

func safeVerificationOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<configured endpoint hidden>"
	}
	return u.Scheme + "://" + u.Host + "/<path and query hidden>"
}

func noneIfEmpty(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }
