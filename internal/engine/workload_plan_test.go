package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestValidateRetainedWorkloadsRefusesRevisionDrift(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "docker ps --filter label=ob.app=") && strings.Contains(command, "--format") {
			return transport.Result{Stdout: "W1|worker|R0|sha256:" + strings.Repeat("b", 64) + "|Up\n"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{WorkloadPlans: map[string]WorkloadPlan{
		"worker": {
			Action: WorkloadActionRetain, Revision: "sha256:" + strings.Repeat("a", 64),
			Reason: "runtime_unchanged",
		},
	}})
	err := e.validateRetainedWorkloads(context.Background())
	if err == nil || !strings.Contains(err.Error(), "revision changed since plan") {
		t.Fatalf("retained revision drift was not refused: %v", err)
	}
	f.Commands = nil
	err = e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "deploy precondition under lock: retained workload worker revision changed") {
		t.Fatalf("under-lock retained drift was not refused: %v", err)
	}
	if len(f.Uploads) != 0 {
		t.Fatalf("retained drift reached transfer: %v", f.Uploads)
	}
}
