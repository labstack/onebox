package app

import (
	"fmt"
	"strings"
	"testing"
)

func executionWorkload() Workload {
	return Workload{
		Role: "job", When: "manual", DataEffect: DataEffectNone,
		Schedule: &JobSchedule{Timeout: "1h"},
		Execution: &JobExecution{Steps: []JobStep{
			{ID: "sync", Command: []string{"sync"}, Outputs: []string{"RELEASE"}},
			{ID: "index", Command: []string{"index"}, Inputs: map[string]string{"RELEASE_ID": "sync.RELEASE"}},
		}},
	}
}

func TestValidateJobExecution(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Workload)
		want string
	}{
		{"valid", func(w *Workload) {}, ""},
		{"single command", func(w *Workload) { w.Execution.Steps = nil }, ""},
		{"disabled", func(w *Workload) { w.Execution = nil; w.Role = "application" }, ""},
		{"default when", func(w *Workload) { w.When = "" }, ""},
		{"retention boundary", func(w *Workload) { w.Execution.Retention = "30d" }, ""},
		{"not job", func(w *Workload) { w.Role = "application" }, "native scheduled manual job"},
		{"not scheduled", func(w *Workload) { w.Schedule = nil }, "native scheduled manual job"},
		{"release hook", func(w *Workload) { w.When = "pre_release" }, "native scheduled manual job"},
		{"migration", func(w *Workload) { w.DataEffect = DataEffectMigration }, "native scheduled manual job"},
		{"compose", func(w *Workload) { w.Compose = "compose.yml#job" }, "native scheduled manual job"},
		{"retention invalid", func(w *Workload) { w.Execution.Retention = "forever" }, "retention"},
		{"retention zero", func(w *Workload) { w.Execution.Retention = "0s" }, "retention"},
		{"retention negative", func(w *Workload) { w.Execution.Retention = "-1h" }, "retention"},
		{"retention excessive", func(w *Workload) { w.Execution.Retention = "721h" }, "retention"},
		{"duplicate step", func(w *Workload) { w.Execution.Steps[1].ID = "sync" }, "unique"},
		{"bad id", func(w *Workload) { w.Execution.Steps[0].ID = "../sync" }, ".id"},
		{"missing command", func(w *Workload) { w.Execution.Steps[0].Command = nil }, "command"},
		{"empty command", func(w *Workload) { w.Execution.Steps[0].Command = []string{""} }, "command"},
		{"nul command", func(w *Workload) { w.Execution.Steps[0].Command = []string{"sync\x00"} }, "NUL"},
		{"large argument", func(w *Workload) { w.Execution.Steps[0].Command = []string{strings.Repeat("x", 4097)} }, "4096"},
		{"large command", func(w *Workload) {
			w.Execution.Steps[0].Command = []string{strings.Repeat("x", 4096), strings.Repeat("x", 4096), strings.Repeat("x", 4096), strings.Repeat("x", 4096), "x"}
		}, "16384"},
		{"many arguments", func(w *Workload) { w.Execution.Steps[0].Command = make([]string, 129) }, "128 arguments"},
		{"forward reference", func(w *Workload) { w.Execution.Steps[0].Inputs = map[string]string{"VALUE": "index.RESULT"} }, "earlier step"},
		{"self reference", func(w *Workload) { w.Execution.Steps[0].Inputs = map[string]string{"VALUE": "sync.RELEASE"} }, "earlier step"},
		{"missing output", func(w *Workload) { w.Execution.Steps[1].Inputs["RELEASE_ID"] = "sync.MISSING" }, "earlier step"},
		{"extra dot", func(w *Workload) { w.Execution.Steps[1].Inputs["RELEASE_ID"] = "sync.RELEASE.extra" }, "earlier step"},
		{"reserved input", func(w *Workload) { w.Execution.Steps[1].Inputs = map[string]string{"ONEBOX_STEP_ID": "sync.RELEASE"} }, "namespace"},
		{"env collision", func(w *Workload) { w.Env = map[string]any{"RELEASE_ID": "value"} }, "collides"},
		{"original input collision", func(w *Workload) { w.Inputs = map[string]JobInput{"RELEASE_ID": {}} }, "collides"},
		{"invalid output", func(w *Workload) { w.Execution.Steps[0].Outputs = []string{"lower"} }, "outputs"},
		{"reserved output", func(w *Workload) { w.Execution.Steps[0].Outputs = []string{"ONEBOX_VALUE"} }, "namespace"},
		{"duplicate output", func(w *Workload) { w.Execution.Steps[0].Outputs = []string{"RELEASE", "RELEASE"} }, "unique"},
		{"too many outputs", func(w *Workload) { w.Execution.Steps[0].Outputs = make([]string, 33) }, "32 outputs"},
		{"too many steps", func(w *Workload) { w.Execution.Steps = make([]JobStep, 33) }, "32 steps"},
		{"too many inputs", func(w *Workload) {
			w.Inputs = map[string]JobInput{}
			for i := range 33 {
				w.Inputs[fmt.Sprintf("INPUT_%d", i)] = JobInput{}
			}
		}, "32 original inputs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := executionWorkload()
			tc.edit(&w)
			err := validateJobExecution(w, "workloads.example")
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateJobExecutionRetryBudget(t *testing.T) {
	attempts := 2
	for _, inherited := range []bool{false, true} {
		t.Run(fmt.Sprintf("inherited=%t", inherited), func(t *testing.T) {
			w := executionWorkload()
			w.Schedule.Timeout = "1m"
			retry := &JobRetry{Attempts: &attempts, Backoff: "30s"}
			if inherited {
				w.Schedule.Retry = retry
			} else {
				for i := range w.Execution.Steps {
					w.Execution.Steps[i].Retry = retry
				}
			}
			if err := validateJobExecution(w, "workloads.example"); err == nil || !strings.Contains(err.Error(), "combined worst-case") {
				t.Fatalf("expected combined retry rejection, got %v", err)
			}
			w.Schedule.Timeout = "61s"
			if err := validateJobExecution(w, "workloads.example"); err != nil {
				t.Fatal(err)
			}
		})
	}
	w := executionWorkload()
	bad := 11
	w.Execution.Steps[0].Retry = &JobRetry{Attempts: &bad}
	if err := validateJobExecution(w, "workloads.example"); err == nil || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("expected retry validation, got %v", err)
	}
}
