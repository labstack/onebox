package engine

import (
	"io"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// A plan is read as a promise about what the deploy will do. Recreate only
// signals and waits when `drain.wait` was authored, so a plan that prints the
// step for a workload that never set one describes a deploy nobody gets.
func TestPlanPromisesADrainWaitOnlyWhenTheDeployTakesOne(t *testing.T) {
	config := testConfig()
	workload := config.Workloads["web"]
	workload.Strategy = "recreate"
	workload.Health = nil
	workload.Drain = &app.Drain{Signal: "USR1"}
	config.Workloads["web"] = workload

	// Scoped to this workload: the fixture's worker authors a drain wait of its
	// own, and that line is correct.
	lines := strings.Join(newPlanEngine(t, config).Describe("/var/lib/ob/sample/releases/R1/compose.yaml"), "\n")
	if strings.Contains(lines, "<current web>") {
		t.Fatalf("the plan promises a drain step the deploy does not take:\n%s", lines)
	}

	withWait := workload
	withWait.Drain = &app.Drain{Signal: "USR1", Wait: "12s"}
	config.Workloads["web"] = withWait
	lines = strings.Join(newPlanEngine(t, config).Describe("/var/lib/ob/sample/releases/R1/compose.yaml"), "\n")
	if !strings.Contains(lines, "--signal=USR1 <current web>; wait up to 12s for exit") {
		t.Fatalf("the plan omits the drain step the deploy does take:\n%s", lines)
	}
}

func newPlanEngine(t *testing.T, config *app.Resolved) *Engine {
	t.Helper()
	return New(config, testProject(t), &transport.Fake{}, Options{Out: io.Discard, Sleep: noSleep})
}

// recreate sends the drain signal for every authored wait, TERM included, and
// then waits up to the bound for exit. A plan that shows the step only for a
// non-default signal hides a kill and wait the deploy will certainly take.
func TestPlanShowsTheDrainStepForTheDefaultSignalToo(t *testing.T) {
	config := testConfig()
	workload := config.Workloads["web"]
	workload.Strategy = "recreate"
	workload.Health = nil
	workload.Drain = &app.Drain{Wait: "15s"} // no signal: TERM
	config.Workloads["web"] = workload

	lines := strings.Join(newPlanEngine(t, config).Describe("/var/lib/ob/sample/releases/R1/compose.yaml"), "\n")
	if !strings.Contains(lines, "--signal=TERM <current web>; wait up to 15s for exit") {
		t.Fatalf("the plan hides the drain step recreate will take:\n%s", lines)
	}
}
