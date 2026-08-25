package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"gopkg.in/yaml.v3"
)

// WorkloadAction is the sealed action chosen for one long-running workload.
type WorkloadAction string

const (
	WorkloadActionRolling  WorkloadAction = "rolling"
	WorkloadActionRecreate WorkloadAction = "recreate"
	WorkloadActionRetain   WorkloadAction = "retain"
)

// WorkloadPlan separates the workload's stable runtime identity from the
// application release identity used by resume and abort.
type WorkloadPlan struct {
	Action   WorkloadAction `json:"action"`
	Revision string         `json:"revision"`
	Reason   string         `json:"reason"`
}

func (p WorkloadPlan) Retained() bool { return p.Action == WorkloadActionRetain }

func (e *Engine) validateRetainedWorkloads(ctx context.Context) error {
	retained := map[string]WorkloadPlan{}
	for name, plan := range e.Opts.WorkloadPlans {
		if plan.Retained() {
			retained[name] = plan
		}
	}
	if len(retained) == 0 {
		return nil
	}
	containers, err := e.projectContainers(ctx)
	if err != nil {
		return fmt.Errorf("observe retained workloads: %w", err)
	}
	names := make([]string, 0, len(retained))
	for name := range retained {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		plan := retained[name]
		workload, ok := e.Spec.Workloads[name]
		if !ok {
			return fmt.Errorf("retained workload %s is absent from the release snapshot", name)
		}
		observed := containers[name]
		if len(observed) != workload.Count() {
			return fmt.Errorf("retained workload %s replica count changed since plan — abort and deploy again", name)
		}
		for _, container := range observed {
			if container.revision != plan.Revision {
				return fmt.Errorf("retained workload %s revision changed since plan — abort and deploy again", name)
			}
			if container.health != "healthy" && container.health != "none" {
				return fmt.Errorf("retained workload %s is no longer ready — abort and deploy again", name)
			}
		}
	}
	return nil
}

func retainableWorkload(observed []svcContainer, revision string, replicas int) bool {
	if revision == "" || len(observed) != replicas {
		return false
	}
	for _, container := range observed {
		if container.revision != revision || container.health != "healthy" && container.health != "none" {
			return false
		}
	}
	return true
}

func (e *Engine) releaseWorkloadRevisions(ctx context.Context, composePath string) (map[string]string, error) {
	result, err := e.T.Run(ctx, "cat "+q(composePath))
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("read release runtime revisions failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	var runtime struct {
		Services map[string]struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(result.Stdout), &runtime); err != nil {
		return nil, fmt.Errorf("decode release runtime revisions: %w", err)
	}
	revisions := make(map[string]string, len(runtime.Services))
	for name, service := range runtime.Services {
		if revision := strings.TrimSpace(service.Labels[app.WorkloadRevisionLabel]); revision != "" {
			revisions[name] = revision
		}
	}
	return revisions, nil
}
