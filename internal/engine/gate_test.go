package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/transport"
)

// gateFake: a deploy where a PREVIOUS release exists (R0), verify fails, and
// the migrate hook's result file content is controlled by `result`.
func gateFake(migrateResult string) *transport.Fake {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "curl -fsS"):
			// the new release fails verify; once auto-rollback replayed the
			// previous release, verify goes green again
			for _, c := range f.Commands {
				if strings.Contains(c, "releases/R0/compose.yaml") {
					return transport.Result{ExitCode: 0}, true
				}
			}
			return transport.Result{ExitCode: 22, Stderr: "500"}, true
		case strings.Contains(cmd, "cat") && strings.Contains(cmd, "job-migrate-result"):
			return transport.Result{Stdout: migrateResult}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R0\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "R0\nR1\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestGateOpenAutoRollsBack(t *testing.T) {
	f := gateFake("changed=false\n")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	err := e.Deploy(context.Background(), "R1", t.TempDir())
	if err == nil {
		t.Fatal("failed verify must still return an error")
	}
	if !strings.Contains(err.Error(), "auto-rolled back") {
		t.Fatalf("gate open must auto-rollback: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "releases/R0/compose.yaml") {
		t.Fatalf("auto-rollback must replay previous release:\n%s", seq)
	}
	if strings.Contains(seq, "ln -sfn 'releases/R1'") {
		t.Fatal("failed release must not be activated")
	}
}

// A job with no same-named hook auto-runs `compose run --rm --no-deps <job>` —
// no `migrate` hook needed in yeet.yml.
func TestJobAutoRunsWithoutHook(t *testing.T) {
	cfg := testConfig()
	cfg.Hooks = map[string]config.Hook{} // drop the migrate hook; migrate stays a job
	f := happyFake()
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "R1", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "run --rm --no-deps migrate") {
		t.Fatalf("a job without a hook must auto-run compose run:\n%s", seq)
	}
	// gate protocol still applies to the auto-run job.
	if !strings.Contains(seq, "YEET_RESULT_FILE=") {
		t.Fatalf("auto-run job must run under the gate protocol:\n%s", seq)
	}
}

func TestGateClosedHaltsAndPages(t *testing.T) {
	f := gateFake("") // migrate wrote nothing — fail safe
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), "R1", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("closed gate must halt-and-page: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "releases/R0/compose.yaml") {
		t.Fatalf("closed gate must NOT auto-rollback:\n%s", seq)
	}
}

func TestExpandOnlyPromiseOverridesClosedGate(t *testing.T) {
	f := gateFake("") // silent migrate
	cfg := testConfig()
	cfg.Migrations = "expand-only"
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), "R1", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "auto-rolled back") {
		t.Fatalf("expand-only must permit auto-rollback: %v", err)
	}
}

func TestNoRollbackFlagAlwaysHalts(t *testing.T) {
	f := gateFake("changed=false\n")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, NoRollback: true})
	err := e.Deploy(context.Background(), "R1", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--no-rollback") {
		t.Fatalf("--no-rollback must halt even with open gate: %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "releases/R0/compose.yaml") {
		t.Fatal("--no-rollback must not roll back")
	}
}

func TestMigrateHookGetsResultFileEnv(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "R1", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	found := false
	for _, c := range f.Commands {
		if strings.Contains(c, "YEET_RESULT_FILE=") && strings.Contains(c, "run --rm --no-deps migrate") {
			found = true
		}
	}
	if !found {
		t.Fatalf("migrate hook must receive YEET_RESULT_FILE:\n%s", strings.Join(f.Commands, "\n"))
	}
}
