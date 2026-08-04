package onebox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
)

func (s *Service) Observe(ctx context.Context, _ ObserveRequest) (Observation, error) {
	capturedAt := s.now().UTC()
	lp, err := s.loadProject(ctx, true)
	if err != nil {
		return Observation{}, fmt.Errorf("load project: %w", err)
	}
	environment := s.environment
	if err := ensureEnvironment(lp.resolved, environment); err != nil {
		return Observation{}, err
	}
	environmentConfig, _ := lp.resolved.Environment(environment)
	e, cleanup, target, err := s.engine(ctx, lp, environment)
	if err != nil {
		return Observation{}, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()

	status, err := e.StatusSnapshot(ctx)
	if err != nil {
		return Observation{}, fmt.Errorf("observe target: %w", err)
	}
	status = sanitizeStatus(status)
	services := describeServices(lp)
	warnings := append([]engine.StatusWarning(nil), status.Warnings...)
	statusForDigest := canonicalStatus(status)
	stateInput := struct {
		ConfigHash  string                `json:"config_hash"`
		ComposeHash string                `json:"compose_hash"`
		Services    []ServiceDescription  `json:"services"`
		Status      engine.StatusSnapshot `json:"status"`
	}{
		ConfigHash: engine.HashBytes(lp.configBytes), ComposeHash: engine.HashBytes(lp.composeBytes),
		Services: services, Status: statusForDigest,
	}
	stateBytes, err := json.Marshal(stateInput)
	if err != nil {
		return Observation{}, fmt.Errorf("encode state digest: %w", err)
	}
	return Observation{
		SchemaVersion: SchemaVersion,
		Application:   lp.resolved.Name,
		Environment:   environment,
		Policy:        describePolicy(environmentConfig.Policy),
		Observability: describeObservability(lp.resolved),
		Target:        target,
		CapturedAt:    capturedAt.Format(timeFormat),
		ConfigHash:    engine.HashBytes(lp.configBytes),
		ComposeHash:   engine.HashBytes(lp.composeBytes),
		StateDigest:   engine.HashBytes(stateBytes),
		Complete:      status.Complete,
		Provenance: []Provenance{
			{Kind: "config", Source: filepath.Base(lp.configPath)},
			{Kind: "compose", Source: filepath.Base(lp.configPath)},
			{Kind: "host", Source: target},
		},
		Services: services,
		Status:   status,
		Warnings: warnings,
	}, nil
}

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

func describeServices(lp *loadedProject) []ServiceDescription {
	if len(lp.resolved.Workloads) > 0 {
		names := make([]string, 0, len(lp.resolved.Workloads))
		for name := range lp.resolved.Workloads {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]ServiceDescription, 0, len(names))
		for _, name := range names {
			component := lp.resolved.Workloads[name]
			description := ServiceDescription{
				Name: name, Service: name, Type: component.Role,
				DataEffect: component.DataEffect, ImageDeclared: serviceImage(lp.compose, name) != "",
			}
			if true {
				description.Strategy = component.Mode()
				description.Replicas = component.Count()
				if description.Replicas < 1 {
					description.Replicas = 1
				}
			}
			if component.Persistence != nil {
				description.PersistenceMode = component.Persistence.Mode
			}
			out = append(out, description)
		}
		return out
	}

	kinds := map[string]ServiceDescription{}
	for _, name := range lp.resolved.ReleaseOrder() {
		role := lp.resolved.Workloads[name]
		kinds[name] = ServiceDescription{
			Name: name, Service: name, Type: role.Role, Strategy: role.Mode(),
			Replicas: role.Count(), ImageDeclared: serviceImage(lp.compose, name) != "",
		}
	}
	for _, name := range lp.resolved.ServiceNames() {
		kinds[name] = ServiceDescription{Name: name, Service: name, Type: "service", ImageDeclared: serviceImage(lp.compose, name) != ""}
	}
	for _, name := range lp.resolved.JobOrder() {
		kinds[name] = ServiceDescription{Name: name, Service: name, Type: "job", DataEffect: "unknown", ImageDeclared: serviceImage(lp.compose, name) != ""}
	}
	for name := range lp.compose.Services {
		if _, ok := kinds[name]; !ok {
			kinds[name] = ServiceDescription{Name: name, Service: name, Type: "service", ImageDeclared: serviceImage(lp.compose, name) != ""}
		}
	}
	out := make([]ServiceDescription, 0, len(kinds))
	for _, service := range kinds {
		out = append(out, service)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func describePolicy(policy app.Policy) EnvironmentPolicyDescription {
	return EnvironmentPolicyDescription{
		RequireApproval: policy.RequireApproval, AllowAgentProposals: policy.AllowAgentProposals,
	}
}

func describeObservability(cfg *app.Resolved) ObservabilityDescription {
	return ObservabilityDescription{
		LogsDeclared: cfg.Observability.Logs != nil, MetricsDeclared: cfg.Observability.Metrics != nil,
		AlertsDeclared: cfg.Observability.Alerts != nil, Managed: false,
	}
}
