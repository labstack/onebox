package onebox

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/engine"
)

const memoryProposalLifetime = 15 * time.Minute

var (
	// The [\w-]* after each keyword catches compound names like
	// aws_secret_access_key= or access-token: that a bare keyword+separator
	// would miss. A separator is still required, so prose ("the secret sauce",
	// "rotate the API key next week") does not trip it.
	secretAssignment = regexp.MustCompile(`(?i)(?:password|passwd|secret|token|api[ _-]?key|access[ _-]?key|private[ _-]?key|credential)[\w-]*\s*[:=]\s*\S+`)
	secretBearer     = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]+`)
	secretKnownToken = regexp.MustCompile(`(?i)(?:\bsk-[a-z0-9_-]{12,}|\bghp_[a-z0-9]{12,}|\bgithub_pat_[a-z0-9_]{12,}|\bxox[baprs]-[a-z0-9-]{12,}|\bAKIA[A-Z0-9]{16}|\bAIza[a-z0-9_-]{35}|\beyJ[a-z0-9_-]{10,}\.[a-z0-9_-]{10,}\.[a-z0-9_-]{5,})`)
	// Match every PEM private-key header variant (RSA, EC, OPENSSH, PGP, …),
	// not just the bare "PRIVATE KEY" form.
	secretPEM         = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY`)
	secretURLUserinfo = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s:@/]+:[^\s:@/]+@`)
)

func (s *Service) ReadMemory(ctx context.Context, _ ReadMemoryRequest) (OperationalMemory, error) {
	_, memory, err := s.loadMemory(ctx)
	return memory, err
}

func (s *Service) loadMemory(ctx context.Context) (*loadedProject, OperationalMemory, error) {
	if err := ctx.Err(); err != nil {
		return nil, OperationalMemory{}, err
	}
	lp, err := s.loadProject(ctx, true)
	if err != nil {
		return nil, OperationalMemory{}, fmt.Errorf("load project: %w", err)
	}
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return nil, OperationalMemory{}, err
	}
	environment, _ := lp.resolved.Environment(s.environment)

	components := make([]MemoryComponent, 0, len(lp.resolved.Workloads))
	for name, component := range lp.resolved.Workloads {
		item := MemoryComponent{
			Name: name, Role: memoryRole(component.Role), Type: component.Role, Service: name,
			DataEffect: component.DataEffect, ReadinessDeclared: component.Health != nil,
		}
		if true {
			item.DeploymentStrategy = component.Mode()
			item.Replicas = component.Count()
			if item.Replicas < 1 {
				item.Replicas = 1
			}
		}
		if component.Persistence != nil {
			item.PersistenceMode = component.Persistence.Mode
		}
		if component.Protection != nil {
			item.BackupDeclared = component.Protection.Backup != nil
			item.RestoreDrillDeclared = component.Protection.RestoreDrill != nil
		}
		components = append(components, item)
	}
	sort.Slice(components, func(i, j int) bool { return components[i].Name < components[j].Name })

	observability := MemoryObservability{
		LogsDeclared:    lp.resolved.Observability.Logs != nil,
		MetricsDeclared: lp.resolved.Observability.Metrics != nil,
		AlertsDeclared:  lp.resolved.Observability.Alerts != nil,
	}
	if lp.resolved.Observability.Logs != nil {
		observability.LogsEnabled = lp.resolved.Observability.Logs.Enabled
	}
	if lp.resolved.Observability.Metrics != nil {
		observability.MetricsEnabled = lp.resolved.Observability.Metrics.Enabled
	}
	provenance := []Provenance{
		{Kind: "config", Source: filepath.Base(lp.configPath)},
		{Kind: "compose", Source: filepath.Base(lp.configPath)},
	}
	memory := OperationalMemory{
		SchemaVersion: MemorySchemaVersion, Application: lp.resolved.Name, Environment: s.environment,
		MigrationPolicy: lp.resolved.Deployment.MigrationPolicy,
		Policy:          describePolicy(environment.Policy), Observability: observability,
		Components: components, Provenance: provenance,
	}
	revisionBytes, err := json.Marshal(struct {
		SchemaVersion   string                       `json:"schema_version"`
		Application     string                       `json:"application"`
		Environment     string                       `json:"environment"`
		ConfigHash      string                       `json:"config_hash"`
		ComposeHash     string                       `json:"compose_hash"`
		MigrationPolicy string                       `json:"migration_policy"`
		Policy          EnvironmentPolicyDescription `json:"policy"`
		Observability   MemoryObservability          `json:"observability"`
		Components      []MemoryComponent            `json:"components"`
		Provenance      []Provenance                 `json:"provenance"`
	}{
		SchemaVersion: memory.SchemaVersion, Application: memory.Application, Environment: memory.Environment,
		ConfigHash: engine.HashBytes(lp.configBytes), ComposeHash: engine.HashBytes(lp.composeBytes),
		MigrationPolicy: memory.MigrationPolicy, Policy: memory.Policy, Observability: memory.Observability,
		Components: memory.Components, Provenance: memory.Provenance,
	})
	if err != nil {
		return nil, OperationalMemory{}, fmt.Errorf("encode memory revision: %w", err)
	}
	memory.RevisionDigest = engine.HashBytes(revisionBytes)
	return lp, memory, nil
}

func memoryRole(componentType string) string {
	switch componentType {
	case "application", "worker":
		return "workload"
	case "job":
		return "job"
	case "postgres", "mysql", "redis":
		return "data_service"
	default:
		return "service"
	}
}

func (s *Service) ProposeMemoryChange(ctx context.Context, request ProposeMemoryChangeRequest) (MemoryChangeProposal, error) {
	lp, memory, err := s.loadMemory(ctx)
	if err != nil {
		return MemoryChangeProposal{}, err
	}
	if request.ExpectedRevision == "" || request.ExpectedRevision != memory.RevisionDigest {
		return MemoryChangeProposal{}, fmt.Errorf("operational memory revision mismatch; read memory again before proposing a change")
	}
	rationale := strings.TrimSpace(request.Rationale)
	if rationale == "" {
		return MemoryChangeProposal{}, fmt.Errorf("memory change rationale must not be empty")
	}
	if containsSecretLikeValue(rationale) {
		return MemoryChangeProposal{}, fmt.Errorf("memory change rationale appears to contain a secret value")
	}

	changes, err := validateAndCopyMemoryChanges(lp, memory, request)
	if err != nil {
		return MemoryChangeProposal{}, err
	}
	now := s.now().UTC()
	entropy := make([]byte, 16)
	if err := s.readEntropy(entropy); err != nil {
		return MemoryChangeProposal{}, fmt.Errorf("create memory proposal identity: %w", err)
	}
	proposal := MemoryChangeProposal{
		SchemaVersion: MemoryProposalSchemaVersion,
		ID:            "memory-change-" + hex.EncodeToString(entropy),
		Application:   memory.Application, Environment: memory.Environment, BaseRevision: memory.RevisionDigest,
		CreatedAt: now.Format(timeFormat), ExpiresAt: now.Add(memoryProposalLifetime).Format(timeFormat),
		Rationale: rationale, Changes: changes,
		Provenance: append(append([]Provenance(nil), memory.Provenance...), Provenance{Kind: "base_revision", Source: memory.RevisionDigest}),
	}
	if err := proposal.Seal(); err != nil {
		return MemoryChangeProposal{}, err
	}
	return proposal, nil
}

func (p MemoryChangeProposal) CanonicalJSON() ([]byte, error) {
	return json.Marshal(struct {
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
	}{
		SchemaVersion: p.SchemaVersion, ID: p.ID,
		Application: p.Application, Environment: p.Environment,
		BaseRevision: p.BaseRevision, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
		Rationale: p.Rationale, Changes: p.Changes, Provenance: p.Provenance,
	})
}

func (p MemoryChangeProposal) ComputeDigest() (string, error) {
	canonical, err := p.CanonicalJSON()
	if err != nil {
		return "", fmt.Errorf("encode memory proposal digest: %w", err)
	}
	return engine.HashBytes(canonical), nil
}

func (p *MemoryChangeProposal) Seal() error {
	if p == nil {
		return fmt.Errorf("memory change proposal is nil")
	}
	if p.SchemaVersion != MemoryProposalSchemaVersion || p.ID == "" || p.BaseRevision == "" ||
		p.Application == "" || p.Environment == "" || p.CreatedAt == "" || p.ExpiresAt == "" {
		return fmt.Errorf("memory change proposal identity is incomplete")
	}
	digest, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	p.Digest = digest
	return nil
}

func (p MemoryChangeProposal) VerifyDigest() error {
	if p.Digest == "" {
		return fmt.Errorf("memory change proposal digest is required")
	}
	expected, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if p.Digest != expected {
		return fmt.Errorf("memory change proposal digest mismatch: got %q, expected %q", p.Digest, expected)
	}
	return nil
}

func validateAndCopyMemoryChanges(lp *loadedProject, memory OperationalMemory, request ProposeMemoryChangeRequest) (MemoryChangeSet, error) {
	known := make(map[string]MemoryComponent, len(memory.Components))
	for _, component := range memory.Components {
		known[component.Name] = component
	}
	seen := make(map[string]bool, len(request.ComponentPatches))
	patches := make([]ComponentMemoryPatch, 0, len(request.ComponentPatches))
	effectiveChanges := 0
	for _, input := range request.ComponentPatches {
		current, ok := known[input.Component]
		if !ok {
			return MemoryChangeSet{}, fmt.Errorf("memory change references unknown component %q", input.Component)
		}
		if seen[input.Component] {
			return MemoryChangeSet{}, fmt.Errorf("memory change contains duplicate patch for component %q", input.Component)
		}
		seen[input.Component] = true
		patch, changed, err := validateAndCopyComponentPatch(lp, current, input)
		if err != nil {
			return MemoryChangeSet{}, fmt.Errorf("component %q: %w", input.Component, err)
		}
		if changed == 0 {
			return MemoryChangeSet{}, fmt.Errorf("component %q patch must contain an effective suggestion", input.Component)
		}
		effectiveChanges += changed
		patches = append(patches, patch)
	}
	sort.Slice(patches, func(i, j int) bool { return patches[i].Component < patches[j].Component })

	policy, changed, err := validateAndCopyPolicyPatch(memory, request.PolicyPatch)
	if err != nil {
		return MemoryChangeSet{}, err
	}
	if request.PolicyPatch != nil && changed == 0 {
		return MemoryChangeSet{}, fmt.Errorf("memory policy patch must contain an effective suggestion")
	}
	effectiveChanges += changed
	if effectiveChanges == 0 {
		return MemoryChangeSet{}, fmt.Errorf("memory change must contain at least one effective component or policy suggestion")
	}
	return MemoryChangeSet{Components: patches, Policy: policy}, nil
}

func validateAndCopyComponentPatch(lp *loadedProject, current MemoryComponent, input ComponentMemoryPatch) (ComponentMemoryPatch, int, error) {
	out := ComponentMemoryPatch{Component: input.Component}
	changed := 0
	if input.Type != nil {
		if !oneOf(*input.Type, "application", "worker", "job", "postgres", "mysql", "redis", "service") {
			return out, 0, fmt.Errorf("type must be application|worker|job|postgres|mysql|redis|service")
		}
		out.Type = copyPointer(input.Type)
		changed += boolInt(*input.Type != current.Type)
	}
	if input.Service != nil {
		if containsSecretLikeValue(*input.Service) {
			return out, 0, fmt.Errorf("service appears to contain a secret value")
		}
		if _, ok := lp.compose.Services[*input.Service]; !ok {
			return out, 0, fmt.Errorf("service %q is not declared by Compose", *input.Service)
		}
		out.Service = copyPointer(input.Service)
		changed += boolInt(*input.Service != current.Service)
	}
	if input.PersistenceMode != nil {
		if !oneOf(*input.PersistenceMode, "durable", "ephemeral", "external") {
			return out, 0, fmt.Errorf("persistence_mode must be durable|ephemeral|external")
		}
		out.PersistenceMode = copyPointer(input.PersistenceMode)
		changed += boolInt(*input.PersistenceMode != current.PersistenceMode)
	}
	if input.DataEffect != nil {
		if !oneOf(*input.DataEffect, "none", "migration", "unknown") {
			return out, 0, fmt.Errorf("data_effect must be none|migration|unknown")
		}
		out.DataEffect = copyPointer(input.DataEffect)
		changed += boolInt(*input.DataEffect != current.DataEffect)
	}
	if input.DeploymentStrategy != nil {
		if !oneOf(*input.DeploymentStrategy, "rolling", "recreate") {
			return out, 0, fmt.Errorf("deployment_strategy must be rolling|recreate")
		}
		out.DeploymentStrategy = copyPointer(input.DeploymentStrategy)
		changed += boolInt(*input.DeploymentStrategy != current.DeploymentStrategy)
	}
	if input.Replicas != nil {
		if *input.Replicas < 1 {
			return out, 0, fmt.Errorf("replicas must be positive")
		}
		out.Replicas = copyPointer(input.Replicas)
		changed += boolInt(*input.Replicas != current.Replicas)
	}
	if input.ReadinessDeclared != nil {
		out.ReadinessDeclared = copyPointer(input.ReadinessDeclared)
		changed += boolInt(*input.ReadinessDeclared != current.ReadinessDeclared)
	}
	if input.BackupDeclared != nil {
		out.BackupDeclared = copyPointer(input.BackupDeclared)
		changed += boolInt(*input.BackupDeclared != current.BackupDeclared)
	}
	if input.RestoreDrillDeclared != nil {
		out.RestoreDrillDeclared = copyPointer(input.RestoreDrillDeclared)
		changed += boolInt(*input.RestoreDrillDeclared != current.RestoreDrillDeclared)
	}
	return out, changed, nil
}

func validateAndCopyPolicyPatch(memory OperationalMemory, input *MemoryPolicyPatch) (*MemoryPolicyPatch, int, error) {
	if input == nil {
		return nil, 0, nil
	}
	out := &MemoryPolicyPatch{}
	changed := 0
	if input.MigrationPolicy != nil {
		if !oneOf(*input.MigrationPolicy, "manual", "expand-only") {
			return nil, 0, fmt.Errorf("memory policy migration_policy must be manual|expand-only")
		}
		out.MigrationPolicy = copyPointer(input.MigrationPolicy)
		changed += boolInt(*input.MigrationPolicy != memory.MigrationPolicy)
	}
	if input.RequireApproval != nil {
		out.RequireApproval = copyPointer(input.RequireApproval)
		changed += boolInt(*input.RequireApproval != memory.Policy.RequireApproval)
	}
	if input.AllowAgentProposals != nil {
		out.AllowAgentProposals = copyPointer(input.AllowAgentProposals)
		changed += boolInt(*input.AllowAgentProposals != memory.Policy.AllowAgentProposals)
	}
	if input.LogsDeclared != nil {
		out.LogsDeclared = copyPointer(input.LogsDeclared)
		changed += boolInt(*input.LogsDeclared != memory.Observability.LogsDeclared)
	}
	if input.MetricsDeclared != nil {
		out.MetricsDeclared = copyPointer(input.MetricsDeclared)
		changed += boolInt(*input.MetricsDeclared != memory.Observability.MetricsDeclared)
	}
	if input.AlertsDeclared != nil {
		out.AlertsDeclared = copyPointer(input.AlertsDeclared)
		changed += boolInt(*input.AlertsDeclared != memory.Observability.AlertsDeclared)
	}
	return out, changed, nil
}

func containsSecretLikeValue(value string) bool {
	return secretPEM.MatchString(value) ||
		secretAssignment.MatchString(value) || secretBearer.MatchString(value) ||
		secretKnownToken.MatchString(value) || secretURLUserinfo.MatchString(value)
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func copyPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
