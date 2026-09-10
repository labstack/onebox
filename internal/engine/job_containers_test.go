package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// jobContainerFake answers the running-container probe. `running` is
// `<id> <operation> <epoch>` lines, exactly as the label probe formats them.
func jobContainerFake(running []string) *transport.Fake {
	return &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "label='ob.operation'"):
			return transport.Result{Stdout: strings.Join(running, "\n") + "\n"}, true
		}
		return transport.Result{}, false
	}}
}

func jobContainerEngine(t *testing.T, f *transport.Fake) *Engine {
	t.Helper()
	return New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
}

func TestRefuseWhileAnotherOperationsJobContainerRuns(t *testing.T) {
	f := jobContainerFake([]string{"abc123def456 J1 4"})
	err := jobContainerEngine(t, f).refuseForeignJobContainers(context.Background(), "J2", 4)
	if err == nil {
		t.Fatal("a live job container from another operation must refuse")
	}
	for _, want := range []string{"J1", "abc123def456", "still running"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q: %v", want, err)
		}
	}
}

// This operation's own container is not a reason to refuse itself — a deploy
// runs gate jobs under its own id.
func TestRefuseAllowsThisOperationsOwnContainer(t *testing.T) {
	f := jobContainerFake([]string{"abc123def456 J1 4"})
	if err := jobContainerEngine(t, f).refuseForeignJobContainers(context.Background(), "J1", 4); err != nil {
		t.Fatalf("own container refused: %v", err)
	}
}

func TestRefuseCatchesAContainerWithAnEmptyOperationLabel(t *testing.T) {
	f := jobContainerFake([]string{"abc123def456  "})
	err := jobContainerEngine(t, f).refuseForeignJobContainers(context.Background(), "J2", 4)
	if err == nil {
		t.Fatal("an unattributable job container must refuse")
	}
	if !strings.Contains(err.Error(), "empty") || !strings.Contains(err.Error(), "abc123def456") {
		t.Fatalf("refusal did not name the problem: %v", err)
	}
}

// A sealed job plan carries one operation id for its whole life and is
// re-runnable, and AcquireLock hands the lock straight back to a caller
// presenting the id already written in it. Matching the operation alone would
// let one run exempt the container another invocation of it left behind.
func TestRefuseCatchesAnotherInvocationOfTheSameOperation(t *testing.T) {
	f := jobContainerFake([]string{"abc123def456 J1 4"})
	err := jobContainerEngine(t, f).refuseForeignJobContainers(context.Background(), "J1", 5)
	if err == nil {
		t.Fatal("an earlier invocation of the same plan must refuse")
	}
	for _, want := range []string{"another invocation", "J1", "epoch 4", "this run is epoch 5", "abc123def456"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q: %v", want, err)
		}
	}
}

// A container carrying no epoch label cannot be shown to belong to this
// invocation, so it is not exempt from it either.
func TestRefuseDoesNotExemptAContainerWithNoEpoch(t *testing.T) {
	f := jobContainerFake([]string{"abc123def456 J1 "})
	err := jobContainerEngine(t, f).refuseForeignJobContainers(context.Background(), "J1", 4)
	if err == nil || !strings.Contains(err.Error(), "carrying no ob.epoch label") {
		t.Fatalf("unlabelled epoch = %v, want a refusal saying it cannot be placed", err)
	}
}

// A line with leading whitespace must still yield its container. Cutting on the
// first space without stripping it yields an empty id, and the container is
// dropped in silence — invisible to the check that exists to see it.
func TestRefuseSeesAContainerOnAPaddedLine(t *testing.T) {
	f := jobContainerFake([]string{"   abc123def456 other-op 2"})
	err := jobContainerEngine(t, f).refuseForeignJobContainers(context.Background(), "J1", 1)
	if err == nil || !strings.Contains(err.Error(), "other-op") {
		t.Fatalf("padded line = %v, want the container refused", err)
	}
}
