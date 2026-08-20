// Package onebox exposes the typed product service shared by agent-facing
// adapters. It deliberately contains no protocol or presentation code.
package onebox

import "github.com/labstack/onebox/internal/engine"

const SchemaVersion = "onebox.run/observation/v1alpha1"

// ObserveRequest is intentionally empty. A Service is bound to exactly one
// launch-time environment; tool input cannot widen that authority boundary.
type ObserveRequest struct{}

// Provenance identifies one source used to build an observation. Observations
// are timestamped, permission-scoped snapshots rather than timeless truth.
type Provenance struct {
	Kind   string `json:"kind" jsonschema:"Source kind, such as config, compose, or host"`
	Source string `json:"source" jsonschema:"Redaction-safe source identity"`
}

// ServiceDescription is the declared, non-secret shape of one Compose service.
type ServiceDescription struct {
	Name            string `json:"name" jsonschema:"Stable logical component name"`
	Service         string `json:"service" jsonschema:"Compose service implementing the component"`
	Type            string `json:"type" jsonschema:"Component type such as application, worker, job, postgres, redis, or service"`
	Strategy        string `json:"strategy,omitempty" jsonschema:"Deployment strategy for application and worker components"`
	Replicas        int    `json:"replicas,omitempty" jsonschema:"Resolved steady-state replica count"`
	DataEffect      string `json:"data_effect,omitempty" jsonschema:"Declared job data effect"`
	PersistenceMode string `json:"persistence_mode,omitempty" jsonschema:"Declared durable, ephemeral, or external persistence mode"`
	ImageDeclared   bool   `json:"image_declared" jsonschema:"Whether the Compose service declares an image reference; the scalar value is hidden"`
}

type EnvironmentPolicyDescription struct {
	RequireApproval     bool `json:"require_approval" jsonschema:"Declared policy that production mutation requires human approval"`
	AllowAgentProposals bool `json:"allow_agent_proposals" jsonschema:"Whether an agent may construct deployment proposals"`
}

type ObservabilityDescription struct {
	LogsDeclared    bool `json:"logs_declared"`
	MetricsDeclared bool `json:"metrics_declared"`
	AlertsDeclared  bool `json:"alerts_declared"`
	Managed         bool `json:"managed" jsonschema:"Whether Onebox currently runs the declared observability capabilities"`
}

// Observation is the structured read model returned to agents. It omits
// Compose environment blocks, secret payloads, and raw application output.
type Observation struct {
	SchemaVersion string                       `json:"schema_version" jsonschema:"Version of this structured observation"`
	Application   string                       `json:"application" jsonschema:"Resolved Onebox application name"`
	Environment   string                       `json:"environment" jsonschema:"Observed Onebox environment"`
	Policy        EnvironmentPolicyDescription `json:"policy" jsonschema:"Resolved environment policy"`
	Observability ObservabilityDescription     `json:"observability" jsonschema:"Declared versus currently managed observability"`
	Server        string                       `json:"server" jsonschema:"Configured SSH server identity"`
	CapturedAt    string                       `json:"captured_at" jsonschema:"RFC3339 timestamp at which observation began"`
	ConfigHash    string                       `json:"config_hash" jsonschema:"SHA-256 identity of the Onebox configuration bytes"`
	ComposeHash   string                       `json:"compose_hash" jsonschema:"SHA-256 identity of the root Compose file bytes"`
	StateDigest   string                       `json:"state_digest" jsonschema:"Digest of the state suitable for plan preconditions"`
	Complete      bool                         `json:"complete" jsonschema:"Whether every supported observation component completed"`
	Provenance    []Provenance                 `json:"provenance" jsonschema:"Sources used to construct this observation"`
	Services      []ServiceDescription         `json:"services" jsonschema:"Declared services in deterministic name order"`
	Status        engine.StatusSnapshot        `json:"status" jsonschema:"Recorded-versus-actual production status"`
	Warnings      []engine.StatusWarning       `json:"warnings,omitempty" jsonschema:"Redaction-safe partial-observation warnings by component"`
}

// ProposeDeployRequest is intentionally empty. The proposal uses the Service's
// launch-time environment and never performs a production mutation.
type ProposeDeployRequest struct{}

// ProposeRequest is the adapter-neutral proposal request. The current kind is
// deploy; approved execution can be added without changing this service
// boundary.
type ProposeRequest struct {
	Kind OperationKind `json:"kind" jsonschema:"Operation kind to propose; currently deploy"`
}

type ProposalHostState struct {
	Host           string            `json:"host" jsonschema:"Bare target hostname"`
	CurrentRelease string            `json:"current_release,omitempty" jsonschema:"Currently activated release, if any"`
	ImageIDs       map[string]string `json:"image_ids,omitempty" jsonschema:"Observed running image identities by service"`
}

type ImagePin struct {
	Service string `json:"service" jsonschema:"Compose service name"`
	Digest  string `json:"digest,omitempty" jsonschema:"Resolved immutable OCI digest; mutable source reference is hidden"`
	Pinned  bool   `json:"pinned" jsonschema:"Whether the image is bound to an immutable digest"`
}

type RiskSummary struct {
	ExpectedInterruption string `json:"expected_interruption" jsonschema:"Plain-language interruption expectation"`
	ApplicationRollback  string `json:"application_rollback" jsonschema:"Available application rollback path"`
	DataEffects          string `json:"data_effects" jsonschema:"Declared job or migration consequences"`
}

type ComparisonStatus string

const (
	ComparisonIdentical    ComparisonStatus = "identical"
	ComparisonDifferent    ComparisonStatus = "different"
	ComparisonUnavailable  ComparisonStatus = "unavailable"
	ComparisonFirstDeploy  ComparisonStatus = "first_deploy"
	ComparisonNotEvaluated ComparisonStatus = "not_evaluated"
)

type ProposalPreconditions struct {
	Ready          bool     `json:"ready" jsonschema:"Whether the observed target has no known deployment blockers"`
	StatusComplete bool     `json:"status_complete" jsonschema:"Whether all supported status sources were observed"`
	StatusDigest   string   `json:"status_digest" jsonschema:"Timestamp-independent digest of the observed operational status"`
	Blockers       []string `json:"blockers" jsonschema:"Redaction-safe reasons this proposal is not ready to execute"`
}

// DeploymentProposal is a redacted, state-bound preview. Commands originating
// in operator-authored hooks are deliberately hidden from model output because
// arbitrary hook text may contain sensitive literals.
type DeploymentProposal struct {
	SchemaVersion             string                       `json:"schema_version" jsonschema:"Version of this proposal schema"`
	ID                        string                       `json:"id" jsonschema:"Unique proposal identifier"`
	ReleaseID                 string                       `json:"release_id" jsonschema:"Release identifier the proposal would stage"`
	Application               string                       `json:"application" jsonschema:"Resolved Onebox application name"`
	Environment               string                       `json:"environment" jsonschema:"Target Onebox environment"`
	Policy                    EnvironmentPolicyDescription `json:"policy" jsonschema:"Resolved target environment policy"`
	Server                    string                       `json:"server" jsonschema:"Configured SSH server identity"`
	CreatedAt                 string                       `json:"created_at" jsonschema:"RFC3339 proposal creation timestamp"`
	ExpiresAt                 string                       `json:"expires_at" jsonschema:"RFC3339 time after which this proposal should be refreshed"`
	GitSHA                    string                       `json:"git_sha,omitempty" jsonschema:"Local repository commit identity when available"`
	ConfigHash                string                       `json:"config_hash" jsonschema:"SHA-256 identity of configuration bytes"`
	ComposeHash               string                       `json:"compose_hash" jsonschema:"SHA-256 identity of the root Compose file bytes"`
	StateDigest               string                       `json:"state_digest" jsonschema:"Stable precondition digest binding environment, configuration, and observed host state"`
	ProposalDigest            string                       `json:"proposal_digest" jsonschema:"Content digest binding every known execution input and observation in this proposal"`
	RenderedComposeCommitment string                       `json:"rendered_compose_commitment" jsonschema:"Proposal-keyed HMAC commitment to the full, unmasked rendered Compose bytes"`
	PayloadCommitment         string                       `json:"payload_commitment" jsonschema:"Proposal-keyed HMAC commitment to the planned non-Compose staged payload"`
	LivePayloadCommitment     string                       `json:"live_payload_commitment,omitempty" jsonschema:"Proposal-keyed HMAC commitment to the observed current-release payload"`
	SecretSourceCommitment    string                       `json:"secret_source_commitment,omitempty" jsonschema:"Proposal-keyed HMAC commitment to the encrypted SOPS source when configured"`
	PayloadMaterialized       bool                         `json:"payload_materialized" jsonschema:"Whether every runtime payload value was materialized while proposing"`
	HostState                 ProposalHostState            `json:"host_state" jsonschema:"Relevant target state observed while planning"`
	Preconditions             ProposalPreconditions        `json:"preconditions" jsonschema:"Observed readiness and blockers"`
	OperationGraph            []OperationStep              `json:"operation_graph" jsonschema:"Canonical typed deployment choreography; hook bodies are never included"`
	Images                    []ImagePin                   `json:"images" jsonschema:"Planned images in deterministic service order"`
	RenderedCompose           string                       `json:"rendered_compose" jsonschema:"Non-executable Compose structure with every scalar value replaced by a proposal-local opaque marker"`
	Diff                      string                       `json:"diff,omitempty" jsonschema:"Unified structural diff whose scalar values are proposal-local opaque markers"`
	ComposeComparison         ComparisonStatus             `json:"compose_comparison" jsonschema:"identical, different, unavailable, or first_deploy"`
	PayloadComparison         ComparisonStatus             `json:"payload_comparison" jsonschema:"identical, different, unavailable, or not_evaluated"`
	NoOp                      bool                         `json:"no_op" jsonschema:"Whether Compose and staged payload are byte-identical to live"`
	CommandSummary            []string                     `json:"command_summary" jsonschema:"Redaction-safe execution-shape summary; operator hook bodies are hidden"`
	HookBodiesRedacted        bool                         `json:"hook_bodies_redacted" jsonschema:"Whether operator-authored command bodies were hidden"`
	FidelityContract          string                       `json:"fidelity_contract" jsonschema:"What the current planner can and cannot promise"`
	Risk                      RiskSummary                  `json:"risk" jsonschema:"Current risk and recovery summary"`
	Checks                    []string                     `json:"checks" jsonschema:"Redaction-safe summary of the release checks"`
	Warnings                  []string                     `json:"warnings,omitempty" jsonschema:"Planning limitations or unpinned image warnings"`
}
