package onebox

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

// DeploymentGraph exposes the canonical deployment choreography without
// rendering commands. releaseID is required so callers cannot accidentally
// plan an unbound transfer even though it is carried by OperationPlan rather
// than repeated on every step.
func DeploymentGraph(cfg *app.Resolved, releaseID string) ([]OperationStep, error) {
	return deploymentGraph(cfg, releaseID)
}

func deploymentGraph(cfg *app.Resolved, releaseID string) ([]OperationStep, error) {
	if cfg == nil {
		return nil, errors.New("deployment graph config is nil")
	}
	if strings.TrimSpace(releaseID) == "" {
		return nil, errors.New("deployment graph release ID is required")
	}

	steps := make([]OperationStep, 0, 8+len(cfg.Workloads))
	appendStep := func(step OperationStep) {
		if len(steps) > 0 {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps = append(steps, step)
	}

	appendStep(OperationStep{ID: "preflight", Kind: StepPreflight, DataEffect: DataEffectNone})
	appendStep(OperationStep{ID: "transfer", Kind: StepTransfer, DataEffect: DataEffectNone, Mutation: true})

	for _, name := range orderedComponents(cfg, "job") {
		component := cfg.Workloads[name]
		service := name
		step := OperationStep{
			ID:         "job:" + service,
			Kind:       StepJob,
			Component:  name,
			Service:    service,
			DataEffect: DataEffectClass(component.DataEffect),
			Mutation:   true,
		}
		if step.DataEffect == DataEffectMigration {
			step.ResultPolicy = JobResultProviderOrStrongUnknown
		}
		appendStep(step)
	}

	if hasLifecycleHook(cfg, "pre_release") {
		appendStep(OperationStep{
			ID: "hook:pre_release", Kind: StepHook, Component: "pre_release",
			DataEffect: DataEffectUnknown, Mutation: true,
		})
	}

	for _, name := range workloadOrder(cfg) {
		component, ok := cfg.Workloads[name]
		if !ok {
			return nil, fmt.Errorf("deployment order references missing component %q", name)
		}
		strategy := ""
		if true {
			strategy = component.Mode()
		} else if role, exists := cfg.Workloads[name]; exists {
			strategy = role.Mode()
		}
		appendStep(OperationStep{
			ID: "release:" + name, Kind: StepWorkloadRelease, Component: name,
			Service: name, DataEffect: DataEffectNone,
			Strategy: strategy, Mutation: true,
		})
	}

	if hasLifecycleHook(cfg, "post_release") {
		appendStep(OperationStep{
			ID: "hook:post_release", Kind: StepHook, Component: "post_release",
			DataEffect: DataEffectUnknown, Mutation: true,
		})
	}

	appendStep(OperationStep{ID: "verify", Kind: StepVerify, DataEffect: DataEffectNone})
	appendStep(OperationStep{ID: "activate", Kind: StepActivate, DataEffect: DataEffectNone, Mutation: true})

	if hasLifecycleHook(cfg, "post_deploy") {
		appendStep(OperationStep{
			ID: "hook:post_deploy", Kind: StepHook, Component: "post_deploy",
			DataEffect: DataEffectUnknown, Mutation: true,
		})
	}
	return steps, nil
}

func orderedComponents(cfg *app.Resolved, componentType string) []string {
	names := make([]string, 0, len(cfg.Workloads))
	for name, component := range cfg.Workloads {
		if component.Role == componentType {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func workloadOrder(cfg *app.Resolved) []string {
	if len(cfg.Deployment.Order) > 0 {
		return append([]string(nil), cfg.Deployment.Order...)
	}
	if len(cfg.ReleaseOrder()) > 0 {
		return append([]string(nil), cfg.ReleaseOrder()...)
	}
	return orderedWorkloadNames(cfg)
}

func orderedWorkloadNames(cfg *app.Resolved) []string {
	names := make([]string, 0, len(cfg.Workloads))
	for name, component := range cfg.Workloads {
		if component.Role == "application" || component.Role == "worker" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		for name := range cfg.Workloads {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func hasLifecycleHook(cfg *app.Resolved, name string) bool {
	if hook, ok := cfg.Hooks[name]; ok {
		return hook.Run != ""
	}
	hook, ok := cfg.Hooks[name]
	return ok && hook.Run != ""
}
