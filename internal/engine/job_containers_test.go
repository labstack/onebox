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

// The parsing has now been wrong twice in ways a single example did not catch —
// once dropping an empty label, once dropping a padded line. This states the
// whole shape of what `docker ps` can hand back, so the next mistake fails here
// rather than in the field, where a dropped line is a container nobody sees.
func TestJobContainerParsing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   []jobContainer
	}{
		{"nothing running", "", nil},
		{"blank output", "\n\n", nil},
		{"one container", "abc123def456 op-1 4\n", []jobContainer{{"abc123def456", "op-1", "4"}}},
		{"several", "abc123def456 op-1 4\nfed654cba321 op-2 9\n",
			[]jobContainer{{"abc123def456", "op-1", "4"}, {"fed654cba321", "op-2", "9"}}},
		// A label docker cannot resolve renders empty, and the separators stay.
		{"no epoch label", "abc123def456 op-1 \n", []jobContainer{{"abc123def456", "op-1", ""}}},
		{"no operation label", "abc123def456  \n", []jobContainer{{"abc123def456", "", ""}}},
		{"no trailing separators", "abc123def456\n", []jobContainer{{"abc123def456", "", ""}}},
		{"padded line", "   abc123def456 op-1 4\n", []jobContainer{{"abc123def456", "op-1", "4"}}},
		{"carriage return", "abc123def456 op-1 4\r\n", []jobContainer{{"abc123def456", "op-1", "4"}}},
		{"no trailing newline", "abc123def456 op-1 4", []jobContainer{{"abc123def456", "op-1", "4"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				return transport.Result{Stdout: tc.stdout}, true
			}}
			got, err := jobContainerEngine(t, f).jobContainers(context.Background())
			if err != nil {
				t.Fatalf("parse %q: %v", tc.stdout, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parse %q = %+v, want %+v", tc.stdout, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parse %q [%d] = %+v, want %+v", tc.stdout, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A daemon that cannot answer must stop the operation, not report an empty host.
func TestJobContainersRefusesAnUnusableAnswer(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		return transport.Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}, true
	}}
	if _, err := jobContainerEngine(t, f).jobContainers(context.Background()); err == nil {
		t.Fatal("a failed docker ps must not read as no containers")
	}
	bad := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		return transport.Result{Stdout: "not-a-container-id op-1 4\n"}, true
	}}
	if _, err := jobContainerEngine(t, bad).jobContainers(context.Background()); err == nil {
		t.Fatal("output that is not a container id must not be trusted")
	}
}

// Exempting this run's own container must not end the scan. A deploy runs its
// own gate jobs, so its container is routinely listed first — and a foreign one
// behind it is exactly what this exists to catch.
func TestRefuseKeepsScanningPastItsOwnContainer(t *testing.T) {
	f := jobContainerFake([]string{
		"aaa111bbb222 op-mine 3",
		"ccc333ddd444 op-other 9",
	})
	err := jobContainerEngine(t, f).refuseForeignJobContainers(context.Background(), "op-mine", 3)
	if err == nil {
		t.Fatal("a foreign container behind an exempt one must still refuse")
	}
	if !strings.Contains(err.Error(), "op-other") || !strings.Contains(err.Error(), "ccc333ddd444") {
		t.Fatalf("refusal named the wrong container: %v", err)
	}
}
