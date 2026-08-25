package onebox

import (
	"fmt"
	"path"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"gopkg.in/yaml.v3"
)

type renderedWorkload struct {
	Labels map[string]any `yaml:"labels"`
}

type renderedRuntime struct {
	Services map[string]renderedWorkload `yaml:"services"`
}

func workloadRevisions(rendered string) (map[string]string, error) {
	var runtime renderedRuntime
	if err := yaml.Unmarshal([]byte(rendered), &runtime); err != nil {
		return nil, fmt.Errorf("decode rendered runtime revisions: %w", err)
	}
	revisions := make(map[string]string, len(runtime.Services))
	for name, service := range runtime.Services {
		value, ok := service.Labels[app.WorkloadRevisionLabel]
		if !ok {
			continue
		}
		revision := strings.TrimSpace(fmt.Sprint(value))
		if revision != "" {
			revisions[name] = revision
		}
	}
	return revisions, nil
}

func planWorkloadActions(cfg *app.Resolved, steps []OperationStep, planned string, host engine.HostState, redeployOnly bool) ([]OperationStep, error) {
	plannedRevisions, err := workloadRevisions(planned)
	if err != nil {
		return nil, err
	}
	nonRetainable := nonRetainableWorkloads(cfg)
	plannedSteps := append([]OperationStep(nil), steps...)
	for index := range plannedSteps {
		step := &plannedSteps[index]
		if step.Kind != StepWorkloadRelease {
			continue
		}
		workload, ok := cfg.Workloads[step.Service]
		if !ok {
			return nil, fmt.Errorf("plan workload %q: declaration is missing", step.Service)
		}
		revision := plannedRevisions[step.Service]
		if revision == "" {
			return nil, fmt.Errorf("plan workload %q: rendered runtime has no %s label", step.Service, app.WorkloadRevisionLabel)
		}
		step.Revision = revision
		step.Action = step.Strategy
		step.Mutation = true
		switch {
		case host.CurrentRelease == "":
			step.Reason = "initial_deploy"
		case redeployOnly:
			// A no-op plan executes only when the operator supplies --redeploy.
			// Keep that fresh roll explicit in the sealed graph rather than
			// approving retain and silently widening execution afterwards.
			step.Reason = "redeploy_only"
		case nonRetainable[step.Service] != "":
			step.Reason = nonRetainable[step.Service]
		case !observedRevisionMatches(host.WorkloadRevisions[step.Service], revision, workload.Count()):
			if !observedRevisionsAvailable(host.WorkloadRevisions[step.Service]) {
				step.Reason = "revision_unavailable"
			} else {
				step.Reason = "runtime_changed_or_drifted"
			}
		case !observedHealthRetainable(host.WorkloadHealth[step.Service], workload.Count()):
			step.Reason = "health_not_ready"
		default:
			step.Action = string(engine.WorkloadActionRetain)
			step.Reason = "runtime_unchanged"
			step.Mutation = false
		}
	}
	return plannedSteps, nil
}

func observedRevisionsAvailable(observed []string) bool {
	if len(observed) == 0 {
		return false
	}
	for _, revision := range observed {
		if revision == "" {
			return false
		}
	}
	return true
}

func observedRevisionMatches(observed []string, expected string, count int) bool {
	if len(observed) != count {
		return false
	}
	for _, revision := range observed {
		if revision != expected {
			return false
		}
	}
	return true
}

func observedHealthRetainable(observed []string, count int) bool {
	if len(observed) != count {
		return false
	}
	for _, health := range observed {
		if health != "healthy" && health != "none" {
			return false
		}
	}
	return true
}

func nonRetainableWorkloads(cfg *app.Resolved) map[string]string {
	bound := make(map[string]string, len(cfg.Workloads))
	for name, workload := range cfg.Workloads {
		switch {
		case workload.Compose != "":
			bound[name] = "compose_reference_ambiguous"
		case hasReleasePathDependency(workload):
			bound[name] = "release_path_dependency"
		}
	}
	return bound
}

func hasReleasePathDependency(workload app.Workload) bool {
	for _, volume := range workload.Volumes {
		if volume.IsBind() && !path.IsAbs(volume.Source) {
			return true
		}
	}
	return false
}

func engineWorkloadPlans(steps []OperationStep) map[string]engine.WorkloadPlan {
	plans := map[string]engine.WorkloadPlan{}
	for _, step := range steps {
		if step.Kind != StepWorkloadRelease {
			continue
		}
		plans[step.Service] = engine.WorkloadPlan{
			Action: engine.WorkloadAction(step.Action), Revision: step.Revision, Reason: step.Reason,
		}
	}
	return plans
}
