package onebox

const (
	MemorySchemaVersion         = "onebox.run/operational-memory/v1alpha1"
	MemoryProposalSchemaVersion = "onebox.run/memory-change-proposal/v1alpha1"
)

// OperationalMemory is a deterministic, redaction-safe projection of the
// resolved v1 configuration. It contains declarations, not claims that a
// protection or observability capability is currently managed.
type OperationalMemory struct {
	SchemaVersion   string                       `json:"schema_version"`
	Application     string                       `json:"application"`
	Environment     string                       `json:"environment"`
	RevisionDigest  string                       `json:"revision_digest"`
	MigrationPolicy string                       `json:"migration_policy"`
	Policy          EnvironmentPolicyDescription `json:"policy"`
	Observability   MemoryObservability          `json:"observability"`
	Components      []MemoryComponent            `json:"components"`
	Provenance      []Provenance                 `json:"provenance"`
}

// MemoryComponent records the stable operational classification of one v1
// component. Role is Onebox's runtime role; Type is the author's v1 type; and
// Service is the backing Compose service.
type MemoryComponent struct {
	Name               string `json:"name"`
	Role               string `json:"role"`
	Type               string `json:"type"`
	Service            string `json:"service"`
	PersistenceMode    string `json:"persistence_mode,omitempty"`
	DataEffect         string `json:"data_effect,omitempty"`
	DeploymentStrategy string `json:"deployment_strategy,omitempty"`
	Replicas           int    `json:"replicas,omitempty"`
	ReadinessDeclared  bool   `json:"readiness_declared"`
}

type MemoryObservability struct {
	LogsDeclared    bool `json:"logs_declared"`
	LogsEnabled     bool `json:"logs_enabled"`
	MetricsDeclared bool `json:"metrics_declared"`
	MetricsEnabled  bool `json:"metrics_enabled"`
	AlertsDeclared  bool `json:"alerts_declared"`
}

type ReadMemoryRequest struct{}

// ComponentMemoryPatch is a typed suggestion against one existing logical
// component. Pointer fields distinguish "not suggested" from a zero value.
// Proposals never apply these fields to the source configuration.
type ComponentMemoryPatch struct {
	Component          string  `json:"component"`
	Type               *string `json:"type,omitempty"`
	Service            *string `json:"service,omitempty"`
	PersistenceMode    *string `json:"persistence_mode,omitempty"`
	DataEffect         *string `json:"data_effect,omitempty"`
	DeploymentStrategy *string `json:"deployment_strategy,omitempty"`
	Replicas           *int    `json:"replicas,omitempty"`
	ReadinessDeclared  *bool   `json:"readiness_declared,omitempty"`
}

// MemoryPolicyPatch contains project- and environment-level policy
// suggestions. Declaration booleans deliberately do not accept endpoints,
// credentials, schedules, or other potentially sensitive configuration.
type MemoryPolicyPatch struct {
	MigrationPolicy     *string `json:"migration_policy,omitempty"`
	RequireApproval     *bool   `json:"require_approval,omitempty"`
	AllowAgentProposals *bool   `json:"allow_agent_proposals,omitempty"`
	LogsDeclared        *bool   `json:"logs_declared,omitempty"`
	MetricsDeclared     *bool   `json:"metrics_declared,omitempty"`
	AlertsDeclared      *bool   `json:"alerts_declared,omitempty"`
}

type MemoryChangeSet struct {
	Components []ComponentMemoryPatch `json:"components,omitempty"`
	Policy     *MemoryPolicyPatch     `json:"policy,omitempty"`
}

type ProposeMemoryChangeRequest struct {
	ExpectedRevision string                 `json:"expected_revision"`
	Rationale        string                 `json:"rationale"`
	ComponentPatches []ComponentMemoryPatch `json:"component_patches,omitempty"`
	PolicyPatch      *MemoryPolicyPatch     `json:"policy_patch,omitempty"`
}

// MemoryChangeProposal is an immutable, expiring suggestion bound to the
// exact operational-memory revision from which it was created.
type MemoryChangeProposal struct {
	SchemaVersion string          `json:"schema_version"`
	ID            string          `json:"id"`
	Application   string          `json:"application"`
	Environment   string          `json:"environment"`
	BaseRevision  string          `json:"base_revision"`
	CreatedAt     string          `json:"created_at"`
	ExpiresAt     string          `json:"expires_at"`
	Rationale     string          `json:"rationale"`
	Changes       MemoryChangeSet `json:"changes"`
	Provenance    []Provenance    `json:"provenance"`
	Digest        string          `json:"digest"`
}
