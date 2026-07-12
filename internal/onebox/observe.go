package onebox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/engine"
)

func (s *Service) Observe(ctx context.Context, _ ObserveRequest) (Observation, error) {
	capturedAt := s.now().UTC()
	lp, err := loadProject(ctx, s.configPath, true)
	if err != nil {
		return Observation{}, fmt.Errorf("load project: %w", err)
	}
	environment := s.environment
	if err := ensureEnvironment(lp.config, environment); err != nil {
		return Observation{}, err
	}
	environmentConfig, _ := lp.config.Environment(environment)
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
		Application:   lp.config.App,
		Environment:   environment,
		Policy:        describePolicy(environmentConfig.Policy),
		Observability: describeObservability(lp.config),
		Target:        target,
		CapturedAt:    capturedAt.Format(timeFormat),
		ConfigHash:    engine.HashBytes(lp.configBytes),
		ComposeHash:   engine.HashBytes(lp.composeBytes),
		StateDigest:   engine.HashBytes(stateBytes),
		Complete:      status.Complete,
		Provenance: []Provenance{
			{Kind: "config", Source: filepath.Base(lp.configPath)},
			{Kind: "compose", Source: filepath.Base(lp.composePath)},
			{Kind: "host", Source: target},
		},
		Services: services,
		Status:   status,
		Warnings: warnings,
	}, nil
}

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

func describeServices(lp *loadedProject) []ServiceDescription {
	if len(lp.config.Components) > 0 {
		names := make([]string, 0, len(lp.config.Components))
		for name := range lp.config.Components {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]ServiceDescription, 0, len(names))
		for _, name := range names {
			component := lp.config.Components[name]
			description := ServiceDescription{
				Name: name, Service: component.Service, Type: component.Type,
				DataEffect: component.DataEffect, ImageDeclared: serviceImage(lp.project, component.Service) != "",
			}
			if component.Deployment != nil {
				description.Strategy = component.Deployment.Strategy
				description.Replicas = component.Deployment.Replicas
				if description.Replicas < 1 {
					description.Replicas = 1
				}
			}
			if component.Persistence != nil {
				description.PersistenceMode = component.Persistence.Mode
			}
			description.ProtectionDeclared = component.Protection != nil && (component.Protection.Backup != nil || component.Protection.RestoreDrill != nil)
			// The schema is stable ahead of the paid protection loop; declarations
			// are visible but never misreported as active management.
			description.ProtectionManaged = false
			out = append(out, description)
		}
		return out
	}

	kinds := map[string]ServiceDescription{}
	for roleName, role := range lp.config.Roles {
		kinds[role.Service] = ServiceDescription{
			Name: roleName, Service: role.Service, Type: "application", Strategy: role.Mode,
			Replicas: resolvedReplicas(role), ImageDeclared: serviceImage(lp.project, role.Service) != "",
		}
	}
	for _, name := range lp.config.Accessories {
		kinds[name] = ServiceDescription{Name: name, Service: name, Type: "service", ImageDeclared: serviceImage(lp.project, name) != ""}
	}
	for _, name := range lp.config.Jobs {
		kinds[name] = ServiceDescription{Name: name, Service: name, Type: "job", DataEffect: "unknown", ImageDeclared: serviceImage(lp.project, name) != ""}
	}
	for name := range lp.project.Services {
		if _, ok := kinds[name]; !ok {
			kinds[name] = ServiceDescription{Name: name, Service: name, Type: "service", ImageDeclared: serviceImage(lp.project, name) != ""}
		}
	}
	out := make([]ServiceDescription, 0, len(kinds))
	for _, service := range kinds {
		out = append(out, service)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func describePolicy(policy config.EnvironmentPolicy) EnvironmentPolicyDescription {
	return EnvironmentPolicyDescription{
		RequireApproval: policy.ApprovalRequired(), AllowAgentProposals: policy.AgentProposalsAllowed(),
	}
}

func describeObservability(cfg *config.Config) ObservabilityDescription {
	return ObservabilityDescription{
		LogsDeclared: cfg.Observability.Logs != nil, MetricsDeclared: cfg.Observability.Metrics != nil,
		AlertsDeclared: cfg.Observability.Alerts != nil, Managed: false,
	}
}
