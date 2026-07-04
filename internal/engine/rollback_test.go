package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// The previous release's snapshot has a DIFFERENT choreography (worker only,
// recreate) — rollback must replay THAT, not the current yeet.yml.
const oldSnapshot = `
app: monk
compose: docker-compose.yaml
environments: { production: { hosts: [deploy@h] } }
roles:
  worker: { service: worker, mode: recreate }
order: [worker]
`

func TestRollbackReplaysSnapshotChoreography(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/R2\n"}, true
		}
		if strings.Contains(cmd, "ls -1") {
			return transport.Result{Stdout: "R1\nR2\n"}, true
		}
		if strings.Contains(cmd, "yeet.snapshot.yml") {
			return transport.Result{Stdout: oldSnapshot}, true
		}
		return base(cmd)
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "--force-recreate worker") {
		t.Fatalf("snapshot choreography (worker recreate) not replayed:\n%s", seq)
	}
	// current yeet.yml rolls web — snapshot doesn't; web must NOT be touched
	if strings.Contains(seq, "--scale server=2") {
		t.Fatalf("rollback used CURRENT config choreography instead of snapshot:\n%s", seq)
	}
}

func TestRollbackFallsBackWithoutSnapshot(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/R2\n"}, true
		}
		if strings.Contains(cmd, "ls -1") {
			return transport.Result{Stdout: "R1\nR2\n"}, true
		}
		if strings.Contains(cmd, "yeet.snapshot.yml") {
			return transport.Result{ExitCode: 1, Stderr: "No such file"}, true
		}
		return base(cmd)
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback fallback: %v", err)
	}
	if !strings.Contains(out.String(), "⚠") {
		t.Fatalf("fallback must warn loudly: %s", out.String())
	}
}
