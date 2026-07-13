package onebox

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/config"
)

func TestDeploymentGraphIsDeterministicAndOrdered(t *testing.T) {
	t.Parallel()
	cfg := operationGraphConfig()

	first, err := deploymentGraph(cfg, "20260712-120000-abcd")
	if err != nil {
		t.Fatal(err)
	}
	second, err := deploymentGraph(cfg, "20260712-120000-abcd")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("graph is not deterministic:\nfirst:  %#v\nsecond: %#v", first, second)
	}

	wantIDs := []string{
		"preflight", "transfer", "job:assets", "job:migrate",
		"hook:pre_release", "release:worker", "release:web",
		"hook:post_release", "verify", "activate", "hook:post_deploy",
	}
	if got := stepIDs(first); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("step IDs = %v, want %v", got, wantIDs)
	}
	for i, step := range first {
		if i == 0 {
			if len(step.DependsOn) != 0 {
				t.Fatalf("first step dependencies = %v, want none", step.DependsOn)
			}
			continue
		}
		want := []string{first[i-1].ID}
		if !reflect.DeepEqual(step.DependsOn, want) {
			t.Fatalf("step %q dependencies = %v, want %v", step.ID, step.DependsOn, want)
		}
	}
	if first[2].DataEffect != DataEffectNone || first[3].DataEffect != DataEffectMigration {
		t.Fatalf("job data effects were not preserved: %#v %#v", first[2], first[3])
	}
	if first[5].Strategy != "recreate" || first[6].Strategy != "rolling" {
		t.Fatalf("workload strategies were not preserved: %#v %#v", first[5], first[6])
	}
}

func TestDeploymentGraphNeverContainsHookBodies(t *testing.T) {
	t.Parallel()
	cfg := operationGraphConfig()
	graph, err := deploymentGraph(cfg, "20260712-120000-abcd")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	for _, secretBody := range []string{"PRE_RELEASE_SECRET", "POST_RELEASE_SECRET", "POST_DEPLOY_SECRET", "JOB_SECRET"} {
		if strings.Contains(string(encoded), secretBody) {
			t.Fatalf("graph exposed hook body %q: %s", secretBody, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"component":"pre_release"`) {
		t.Fatalf("graph omitted redacted hook identity: %s", encoded)
	}
}

func TestDeploymentGraphPreservesLogicalAndComposeServiceAliases(t *testing.T) {
	t.Parallel()
	cfg := operationGraphConfig()
	job := cfg.Components["migrate"]
	job.Service = "db_migrate"
	cfg.Components["migrate"] = job
	web := cfg.Components["web"]
	web.Service = "http"
	cfg.Components["web"] = web

	graph, err := deploymentGraph(cfg, "20260712-120000-abcd")
	if err != nil {
		t.Fatal(err)
	}
	if got := graph[3]; got.ID != "job:db_migrate" || got.Component != "migrate" || got.Service != "db_migrate" {
		t.Fatalf("job alias was not preserved: %#v", got)
	}
	if got := graph[6]; got.ID != "release:web" || got.Component != "web" || got.Service != "http" {
		t.Fatalf("workload alias was not preserved: %#v", got)
	}
}

func TestDeploymentGraphOmitsAbsentHooksAndJobs(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Components: map[string]config.Component{
			"web": {
				Type: "application",
				Deployment: &config.ComponentDeployment{
					Strategy: "rolling",
				},
			},
		},
		Deployment: config.Deployment{Order: []string{"web"}},
	}
	graph, err := deploymentGraph(cfg, "20260712-120000-abcd")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"preflight", "transfer", "release:web", "verify", "activate"}
	if got := stepIDs(graph); !reflect.DeepEqual(got, want) {
		t.Fatalf("step IDs = %v, want %v", got, want)
	}
}

func TestDeploymentClassificationDoesNotOverstateFirstDeployRollback(t *testing.T) {
	t.Parallel()
	steps := []OperationStep{{ID: "preflight", Kind: StepPreflight, DataEffect: DataEffectNone}}
	_, firstReversibility, _ := classifyDeployment(steps, "")
	_, laterReversibility, _ := classifyDeployment(steps, "R1")
	if firstReversibility != ReversibilityConditional || laterReversibility != ReversibilityReversible {
		t.Fatalf("reversibility first=%s later=%s", firstReversibility, laterReversibility)
	}
}

func operationGraphConfig() *config.Config {
	return &config.Config{
		Components: map[string]config.Component{
			"web": {
				Type: "application",
				Deployment: &config.ComponentDeployment{
					Strategy: "rolling",
				},
			},
			"worker": {
				Type: "worker",
				Deployment: &config.ComponentDeployment{
					Strategy: "recreate",
				},
			},
			"migrate": {
				Type: "job", DataEffect: "migration",
				Command: &config.Hook{Run: "echo JOB_SECRET"},
			},
			"assets": {Type: "job", DataEffect: "none"},
		},
		Deployment: config.Deployment{Order: []string{"worker", "web"}},
		LifecycleHooks: map[string]config.Hook{
			"pre_release":  {Run: "echo PRE_RELEASE_SECRET"},
			"post_release": {Run: "echo POST_RELEASE_SECRET"},
			"post_deploy":  {Run: "echo POST_DEPLOY_SECRET"},
		},
	}
}

func stepIDs(steps []OperationStep) []string {
	ids := make([]string, len(steps))
	for i, step := range steps {
		ids[i] = step.ID
	}
	return ids
}
