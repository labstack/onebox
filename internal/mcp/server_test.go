package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/onebox"
)

type recordingService struct {
	observeRequests             []onebox.ObserveRequest
	proposeRequests             []onebox.ProposeRequest
	readMemoryRequests          []onebox.ReadMemoryRequest
	proposeMemoryChangeRequests []onebox.ProposeMemoryChangeRequest
	observation                 onebox.Observation
	proposal                    onebox.DeploymentProposal
	memory                      onebox.OperationalMemory
	memoryProposal              onebox.MemoryChangeProposal
	observeErr                  error
	proposeErr                  error
	readMemoryErr               error
	proposeMemoryChangeErr      error
}

func (s *recordingService) Observe(_ context.Context, req onebox.ObserveRequest) (onebox.Observation, error) {
	s.observeRequests = append(s.observeRequests, req)
	return s.observation, s.observeErr
}

func (s *recordingService) Propose(_ context.Context, req onebox.ProposeRequest) (onebox.DeploymentProposal, error) {
	s.proposeRequests = append(s.proposeRequests, req)
	return s.proposal, s.proposeErr
}

func (s *recordingService) ReadMemory(_ context.Context, req onebox.ReadMemoryRequest) (onebox.OperationalMemory, error) {
	s.readMemoryRequests = append(s.readMemoryRequests, req)
	return s.memory, s.readMemoryErr
}

func (s *recordingService) ProposeMemoryChange(_ context.Context, req onebox.ProposeMemoryChangeRequest) (onebox.MemoryChangeProposal, error) {
	s.proposeMemoryChangeRequests = append(s.proposeMemoryChangeRequests, req)
	return s.memoryProposal, s.proposeMemoryChangeErr
}

func TestToolErrorsDoNotExposeServiceDiagnostics(t *testing.T) {
	const secret = "host-output-must-not-reach-model"
	service := &recordingService{
		observeErr: errors.New(secret), proposeErr: errors.New(secret),
		readMemoryErr: errors.New(secret), proposeMemoryChangeErr: errors.New(secret),
	}
	client := connectTestClient(t, service)

	toolArguments := map[string]map[string]any{
		"onebox_observe":               {},
		"onebox_propose_deploy":        {},
		"onebox_read_memory":           {},
		"onebox_propose_memory_change": {"expected_revision": "revision", "rationale": "rationale"},
	}
	for name, arguments := range toolArguments {
		result, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: arguments})
		if err != nil {
			t.Fatalf("CallTool(%s): %v", name, err)
		}
		if !result.IsError {
			t.Fatalf("CallTool(%s) should return a sanitized tool error", name)
		}
		encoded, err := json.Marshal(result.Content)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), secret) || !strings.Contains(string(encoded), name+": operation could not complete") {
			t.Fatalf("CallTool(%s) error boundary failed: %s", name, encoded)
		}
	}
}

func TestNewListsReadOnlyTools(t *testing.T) {
	service := &recordingService{}
	client := connectTestClient(t, service)

	result, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got, want := len(result.Tools), 4; got != want {
		t.Fatalf("tool count = %d, want %d", got, want)
	}

	tools := make(map[string]*mcpsdk.Tool, len(result.Tools))
	for _, tool := range result.Tools {
		tools[tool.Name] = tool
	}
	externalReads := map[string]bool{
		"onebox_observe":               true,
		"onebox_propose_deploy":        true,
		"onebox_read_memory":           false,
		"onebox_propose_memory_change": false,
	}
	for name, wantOpenWorld := range externalReads {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("tool %q is not advertised", name)
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q does not advertise readOnlyHint=true", name)
		}
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Errorf("tool %q does not advertise destructiveHint=false", name)
		}
		if tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint != wantOpenWorld {
			t.Errorf("tool %q openWorldHint = %v, want %v", name, tool.Annotations.OpenWorldHint, wantOpenWorld)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(schema), "environment") {
			t.Errorf("tool %q input can widen the launch environment: %s", name, schema)
		}
	}
}

func TestToolsDelegateTypedRequestsAndReturnStructuredOutput(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 12, 9, 30, 0, 0, time.UTC)
	service := &recordingService{
		observation: onebox.Observation{
			SchemaVersion: "onebox.run/observation/v1alpha1",
			Application:   "storefront",
			Environment:   "production",
			Policy: onebox.EnvironmentPolicyDescription{
				RequireApproval:     true,
				AllowAgentProposals: true,
			},
			Observability: onebox.ObservabilityDescription{
				LogsDeclared: true, MetricsDeclared: true, AlertsDeclared: true, Managed: false,
			},
			Target:      "deploy@example.com",
			CapturedAt:  capturedAt.Format(time.RFC3339),
			ConfigHash:  "config-sha256",
			ComposeHash: "compose-sha256",
			StateDigest: "observation-sha256",
			Complete:    true,
			Provenance:  []onebox.Provenance{},
			Services: []onebox.ServiceDescription{
				{
					Name: "web", Service: "server", Type: "application", Strategy: "rolling", Replicas: 2,
					ImageDeclared: true,
				},
				{
					Name: "database", Service: "postgres", Type: "postgres", PersistenceMode: "durable",
					ProtectionDeclared: true, ProtectionManaged: false, ImageDeclared: true,
				},
			},
			Status: engine.StatusSnapshot{
				App:         "storefront",
				Host:        "example.com",
				CapturedAt:  capturedAt,
				Roles:       []engine.StatusRole{},
				Accessories: []engine.StatusAccessory{},
				Complete:    true,
				Warnings:    []engine.StatusWarning{},
			},
			Warnings: []engine.StatusWarning{},
		},
		proposal: onebox.DeploymentProposal{
			SchemaVersion: "onebox.run/deployment-proposal/v1alpha1",
			ID:            "20260712T093000Z-deadbee",
			Application:   "storefront",
			Environment:   "staging",
			Policy: onebox.EnvironmentPolicyDescription{
				RequireApproval:     true,
				AllowAgentProposals: true,
			},
			Target:           "deploy@example.com",
			CreatedAt:        capturedAt.Format(time.RFC3339),
			ExpiresAt:        capturedAt.Add(15 * time.Minute).Format(time.RFC3339),
			ConfigHash:       "config-sha256",
			ComposeHash:      "compose-sha256",
			StateDigest:      "proposal-sha256",
			HostState:        onebox.ProposalHostState{Host: "example.com", ImageIDs: map[string]string{}},
			Images:           []onebox.ImagePin{},
			RenderedCompose:  "services: {}\n",
			CommandSummary:   []string{"upload release payload"},
			FidelityContract: "test fidelity contract",
			Risk: onebox.RiskSummary{
				ExpectedInterruption: "none expected",
				ApplicationRollback:  "previous release available",
				DataEffects:          "none declared",
			},
			Verification: []string{},
			Warnings:     []string{},
		},
		memory: onebox.OperationalMemory{
			SchemaVersion:   onebox.MemorySchemaVersion,
			Application:     "storefront",
			Environment:     "production",
			RevisionDigest:  "memory-sha256",
			MigrationPolicy: "expand-only",
			Components: []onebox.MemoryComponent{
				{Name: "database", Role: "data_service", Type: "postgres", Service: "postgres", PersistenceMode: "durable", BackupDeclared: true},
			},
			Provenance: []onebox.Provenance{{Kind: "config", Source: "ob.yml"}},
		},
		memoryProposal: onebox.MemoryChangeProposal{
			SchemaVersion: onebox.MemoryProposalSchemaVersion,
			ID:            "memory-change-deadbeef",
			Application:   "storefront",
			Environment:   "production",
			BaseRevision:  "memory-sha256",
			CreatedAt:     capturedAt.Format(time.RFC3339),
			ExpiresAt:     capturedAt.Add(15 * time.Minute).Format(time.RFC3339),
			Rationale:     "record backup ownership",
			Digest:        "memory-proposal-sha256",
		},
	}
	client := connectTestClient(t, service)

	observeResult, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "onebox_observe",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool(onebox_observe): %v", err)
	}
	if observeResult.IsError {
		t.Fatalf("CallTool(onebox_observe) returned tool error: %#v", observeResult.Content)
	}
	if got, want := service.observeRequests, []onebox.ObserveRequest{{}}; !equalJSON(got, want) {
		t.Fatalf("Observe requests = %#v, want %#v", got, want)
	}
	observation := decodeStructured[onebox.Observation](t, observeResult.StructuredContent)
	if got, want := observation.StateDigest, service.observation.StateDigest; got != want {
		t.Errorf("observation state digest = %q, want %q", got, want)
	}
	if got, want := observation.Status.CapturedAt, capturedAt; !got.Equal(want) {
		t.Errorf("observation status captured_at = %v, want %v", got, want)
	}
	if !observation.Policy.RequireApproval || !observation.Policy.AllowAgentProposals {
		t.Errorf("observation policy was lost across MCP: %#v", observation.Policy)
	}
	if !observation.Observability.LogsDeclared || !observation.Observability.MetricsDeclared || !observation.Observability.AlertsDeclared || observation.Observability.Managed {
		t.Errorf("observation observability metadata was lost across MCP: %#v", observation.Observability)
	}
	if len(observation.Services) != 2 || observation.Services[0].Type != "application" || observation.Services[1].PersistenceMode != "durable" || !observation.Services[1].ProtectionDeclared || observation.Services[1].ProtectionManaged {
		t.Errorf("stable component metadata was lost across MCP: %#v", observation.Services)
	}

	proposeResult, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "onebox_propose_deploy",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool(onebox_propose_deploy): %v", err)
	}
	if proposeResult.IsError {
		t.Fatalf("CallTool(onebox_propose_deploy) returned tool error: %#v", proposeResult.Content)
	}
	if got, want := service.proposeRequests, []onebox.ProposeRequest{{Kind: onebox.KindDeploy}}; !equalJSON(got, want) {
		t.Fatalf("Propose requests = %#v, want %#v", got, want)
	}
	proposal := decodeStructured[onebox.DeploymentProposal](t, proposeResult.StructuredContent)
	if got, want := proposal.ID, service.proposal.ID; got != want {
		t.Errorf("proposal ID = %q, want %q", got, want)
	}
	if got, want := proposal.StateDigest, service.proposal.StateDigest; got != want {
		t.Errorf("proposal state digest = %q, want %q", got, want)
	}
	if !proposal.Policy.RequireApproval || !proposal.Policy.AllowAgentProposals {
		t.Errorf("proposal policy was lost across MCP: %#v", proposal.Policy)
	}

	readMemoryResult, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "onebox_read_memory",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool(onebox_read_memory): %v", err)
	}
	if readMemoryResult.IsError {
		t.Fatalf("CallTool(onebox_read_memory) returned tool error: %#v", readMemoryResult.Content)
	}
	if got, want := service.readMemoryRequests, []onebox.ReadMemoryRequest{{}}; !equalJSON(got, want) {
		t.Fatalf("ReadMemory requests = %#v, want %#v", got, want)
	}
	memory := decodeStructured[onebox.OperationalMemory](t, readMemoryResult.StructuredContent)
	if got, want := memory.RevisionDigest, service.memory.RevisionDigest; got != want {
		t.Errorf("memory revision digest = %q, want %q", got, want)
	}
	if len(memory.Components) != 1 || memory.Components[0].PersistenceMode != "durable" || !memory.Components[0].BackupDeclared {
		t.Errorf("operational memory metadata was lost across MCP: %#v", memory.Components)
	}

	memoryChangeRequest := onebox.ProposeMemoryChangeRequest{
		ExpectedRevision: service.memory.RevisionDigest,
		Rationale:        "record backup ownership",
		ComponentPatches: []onebox.ComponentMemoryPatch{{Component: "database", BackupDeclared: boolPointer(true)}},
	}
	proposeMemoryResult, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "onebox_propose_memory_change",
		Arguments: memoryChangeRequest,
	})
	if err != nil {
		t.Fatalf("CallTool(onebox_propose_memory_change): %v", err)
	}
	if proposeMemoryResult.IsError {
		t.Fatalf("CallTool(onebox_propose_memory_change) returned tool error: %#v", proposeMemoryResult.Content)
	}
	if got, want := service.proposeMemoryChangeRequests, []onebox.ProposeMemoryChangeRequest{memoryChangeRequest}; !equalJSON(got, want) {
		t.Fatalf("ProposeMemoryChange requests = %#v, want %#v", got, want)
	}
	memoryProposal := decodeStructured[onebox.MemoryChangeProposal](t, proposeMemoryResult.StructuredContent)
	if got, want := memoryProposal.Digest, service.memoryProposal.Digest; got != want {
		t.Errorf("memory proposal digest = %q, want %q", got, want)
	}
}

func connectTestClient(t *testing.T, service Service) *mcpsdk.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	serverSession, err := New(service, "test").Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect MCP server: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "onebox-test", Version: "test"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("connect MCP client: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return clientSession
}

func decodeStructured[T any](t *testing.T, value any) T {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal structured output: %v", err)
	}
	var decoded T
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode structured output: %v", err)
	}
	return decoded
}

func equalJSON(a, b any) bool {
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && string(aJSON) == string(bJSON)
}
