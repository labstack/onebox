package onebox

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
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
		"job:cleanup", "hook:post_release", "verify", "activate", "hook:post_deploy",
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
	for _, step := range first {
		if step.ID == "job:nightly" {
			t.Fatal("manual job entered the deploy operation graph")
		}
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

func TestDeploymentGraphOmitsAbsentHooksAndJobs(t *testing.T) {
	t.Parallel()
	spec, err := app.LoadBytes([]byte(`
api_version: onebox.run/v1
app: sample
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1, strategy: rolling, health: {http: /healthz, port: 8080}}
deployment: {order: [web]}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	graph, gerr := deploymentGraph(cfg, "20260712-120000-abcd")
	if gerr != nil {
		t.Fatal(gerr)
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

func operationGraphConfig() *app.Resolved {
	spec, err := app.LoadBytes([]byte(`
api_version: onebox.run/v1
app: sample
environments: {production: {server: root@h}}
workloads:
  web:    {role: application, image: x:1, strategy: rolling, health: {http: /healthz, port: 8080}}
  worker: {role: worker, image: x:1, strategy: recreate}
  migrate: {role: job, image: x:1, command: "echo JOB_SECRET", when: pre_release, data_effect: migration}
  assets:  {role: job, image: x:1, when: pre_release, data_effect: none}
  cleanup: {role: job, image: x:1, when: post_release, data_effect: none}
  nightly: {role: job, image: x:1, when: manual, data_effect: none, schedule: {cron: "0 2 * * *"}}
deployment:
  order: [worker, web]
hooks:
  pre_release:  {run: "echo PRE_RELEASE_SECRET"}
  post_release: {run: "echo POST_RELEASE_SECRET"}
  post_deploy:  {run: "echo POST_DEPLOY_SECRET"}
`), "ob.yml")
	if err != nil {
		panic("operation graph fixture does not load: " + err.Error())
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		panic("operation graph fixture does not resolve: " + err.Error())
	}
	return resolved
}

func stepIDs(steps []OperationStep) []string {
	ids := make([]string, len(steps))
	for i, step := range steps {
		ids[i] = step.ID
	}
	return ids
}
