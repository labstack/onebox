package onebox

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/transport"
)

func TestReadMemoryIsDeterministicResolvedAndRedactionSafe(t *testing.T) {
	configPath := writeServiceProject(t)
	connected := false
	svc := New(Options{
		ConfigPath: configPath,
		Connect: func(context.Context, string) (transport.Transport, error) {
			connected = true
			return serviceFake(), nil
		},
	})

	first, err := svc.ReadMemory(context.Background(), ReadMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.ReadMemory(context.Background(), ReadMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if connected {
		t.Fatal("reading operational memory must not connect to production")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("unchanged operational memory is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if first.SchemaVersion != MemorySchemaVersion || first.Application != "demo" || first.Environment != "production" || !strings.HasPrefix(first.RevisionDigest, "sha256:") {
		t.Fatalf("unexpected memory identity: %#v", first)
	}
	if first.MigrationPolicy != "manual" || !first.Policy.RequireApproval || !first.Policy.AllowAgentProposals {
		t.Fatalf("resolved policy missing: %#v", first)
	}
	if !first.Observability.LogsDeclared || !first.Observability.LogsEnabled || !first.Observability.MetricsDeclared || !first.Observability.MetricsEnabled || !first.Observability.AlertsDeclared {
		t.Fatalf("observability declarations missing: %#v", first.Observability)
	}
	if len(first.Components) != 2 || first.Components[0].Name != "database" || first.Components[0].Role != "data_service" || first.Components[0].Type != "postgres" || first.Components[0].Service != "postgres" || first.Components[0].PersistenceMode != "durable" || !first.Components[0].BackupDeclared || !first.Components[0].RestoreDrillDeclared {
		t.Fatalf("database memory is incomplete: %#v", first.Components)
	}
	if first.Components[1].Name != "web" || first.Components[1].Role != "workload" || first.Components[1].DeploymentStrategy != "rolling" || first.Components[1].Replicas != 1 || !first.Components[1].ReadinessDeclared {
		t.Fatalf("workload memory is incomplete: %#v", first.Components[1])
	}
	if len(first.Provenance) != 2 || first.Provenance[0].Source != "ob.yml" || first.Provenance[1].Source != "docker-compose.yaml" {
		t.Fatalf("unexpected provenance: %#v", first.Provenance)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), testSecret) || strings.Contains(string(encoded), "RUNTIME_SECRET") {
		t.Fatalf("operational memory exposed secret material: %s", encoded)
	}
}

func TestReadMemoryRevisionChangesWithSourceRevision(t *testing.T) {
	configPath := writeServiceProject(t)
	svc := New(Options{ConfigPath: configPath})
	before, err := svc.ReadMemory(context.Background(), ReadMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(source), "migration_policy: manual", "migration_policy: expand-only", 1)
	if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := svc.ReadMemory(context.Background(), ReadMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if before.RevisionDigest == after.RevisionDigest || after.MigrationPolicy != "expand-only" {
		t.Fatalf("source revision did not change memory: before=%#v after=%#v", before, after)
	}
}

func TestProposeMemoryChangeIsImmutableBoundAndReadOnly(t *testing.T) {
	configPath := writeServiceProject(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(Options{
		ConfigPath: configPath,
		Now:        func() time.Time { return time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC) },
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{9}, 16)),
	})
	memory, err := svc.ReadMemory(context.Background(), ReadMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	replicas := 3
	migrationPolicy := "expand-only"
	request := ProposeMemoryChangeRequest{
		ExpectedRevision: memory.RevisionDigest,
		Rationale:        "Scale the web workload before the next release.",
		ComponentPatches: []ComponentMemoryPatch{{Component: "web", Replicas: &replicas}},
		PolicyPatch:      &MemoryPolicyPatch{MigrationPolicy: &migrationPolicy},
	}
	proposal, err := svc.ProposeMemoryChange(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("memory proposal modified configuration")
	}
	if proposal.SchemaVersion != MemoryProposalSchemaVersion || proposal.BaseRevision != memory.RevisionDigest || proposal.ID != "memory-change-"+strings.Repeat("09", 16) || !strings.HasPrefix(proposal.Digest, "sha256:") {
		t.Fatalf("proposal identity is incomplete: %#v", proposal)
	}
	if err := proposal.VerifyDigest(); err != nil {
		t.Fatalf("proposal digest does not verify: %v", err)
	}
	if proposal.CreatedAt != "2026-07-12T20:00:00.000Z" || proposal.ExpiresAt != "2026-07-12T20:15:00.000Z" || len(proposal.Provenance) != 3 {
		t.Fatalf("proposal attribution/lifetime is incomplete: %#v", proposal)
	}
	if len(proposal.Changes.Components) != 1 || *proposal.Changes.Components[0].Replicas != 3 || proposal.Changes.Policy == nil || *proposal.Changes.Policy.MigrationPolicy != "expand-only" {
		t.Fatalf("proposal changes missing: %#v", proposal.Changes)
	}

	// The proposal owns its copied values; mutating request storage cannot alter
	// the already-digested proposal.
	replicas = 99
	migrationPolicy = "manual"
	if *proposal.Changes.Components[0].Replicas != 3 || *proposal.Changes.Policy.MigrationPolicy != "expand-only" {
		t.Fatalf("proposal aliases mutable request values: %#v", proposal.Changes)
	}
	tampered := proposal
	tampered.Rationale = "A different operational claim."
	if err := tampered.VerifyDigest(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered proposal was not rejected: %v", err)
	}
}

func TestProposeMemoryChangeRejectsUnsafeOrEmptyRequests(t *testing.T) {
	svc := New(Options{ConfigPath: writeServiceProject(t)})
	memory, err := svc.ReadMemory(context.Background(), ReadMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	replicas := 2
	currentReplicas := 1
	tests := []struct {
		name    string
		request ProposeMemoryChangeRequest
		want    string
	}{
		{name: "revision mismatch", request: ProposeMemoryChangeRequest{ExpectedRevision: "sha256:stale", Rationale: "Scale safely", ComponentPatches: []ComponentMemoryPatch{{Component: "web", Replicas: &replicas}}}, want: "revision mismatch"},
		{name: "empty rationale", request: ProposeMemoryChangeRequest{ExpectedRevision: memory.RevisionDigest, ComponentPatches: []ComponentMemoryPatch{{Component: "web", Replicas: &replicas}}}, want: "rationale"},
		{name: "empty change", request: ProposeMemoryChangeRequest{ExpectedRevision: memory.RevisionDigest, Rationale: "Document the current setup"}, want: "effective"},
		{name: "unknown component", request: ProposeMemoryChangeRequest{ExpectedRevision: memory.RevisionDigest, Rationale: "Scale a workload", ComponentPatches: []ComponentMemoryPatch{{Component: "missing", Replicas: &replicas}}}, want: "unknown component"},
		{name: "no-op component patch", request: ProposeMemoryChangeRequest{ExpectedRevision: memory.RevisionDigest, Rationale: "Keep the existing replica count", ComponentPatches: []ComponentMemoryPatch{{Component: "web", Replicas: &currentReplicas}}}, want: "effective suggestion"},
		{name: "secret-like rationale", request: ProposeMemoryChangeRequest{ExpectedRevision: memory.RevisionDigest, Rationale: "Use token=ghp_abcdefghijklmnop", ComponentPatches: []ComponentMemoryPatch{{Component: "web", Replicas: &replicas}}}, want: "secret value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.ProposeMemoryChange(context.Background(), tt.request)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestProposeMemoryChangeSortsComponentPatches(t *testing.T) {
	svc := New(Options{
		ConfigPath: writeServiceProject(t),
		Now:        func() time.Time { return time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC) },
		Entropy:    bytes.NewReader(bytes.Repeat([]byte{4}, 16)),
	})
	memory, err := svc.ReadMemory(context.Background(), ReadMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	databaseMode := "external"
	webReplicas := 2
	proposal, err := svc.ProposeMemoryChange(context.Background(), ProposeMemoryChangeRequest{
		ExpectedRevision: memory.RevisionDigest,
		Rationale:        "Prepare for an external database and increased application traffic.",
		ComponentPatches: []ComponentMemoryPatch{
			{Component: "web", Replicas: &webReplicas},
			{Component: "database", PersistenceMode: &databaseMode},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Changes.Components[0].Component != "database" || proposal.Changes.Components[1].Component != "web" {
		t.Fatalf("component changes are not deterministic: %#v", proposal.Changes.Components)
	}
}
