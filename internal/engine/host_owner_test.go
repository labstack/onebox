package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestForeignHostOwnerBlocksMutationsBeforeEffects(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Engine) error
	}{
		{"bootstrap", func(ctx context.Context, engine *Engine) error { return engine.Bootstrap(ctx, "R1") }},
		{"preflight", func(ctx context.Context, engine *Engine) error { return engine.Preflight(ctx) }},
		{"deploy", func(ctx context.Context, engine *Engine) error { return engine.Deploy(ctx, "R1", t.TempDir()) }},
		{"resume", func(ctx context.Context, engine *Engine) error { _, err := engine.ResumeWithJournalID(ctx); return err }},
		{"abort", func(ctx context.Context, engine *Engine) error {
			_, err := engine.AbortWithJournalID(ctx, false)
			return err
		}},
		{"rollback", func(ctx context.Context, engine *Engine) error {
			_, err := engine.RollbackWithJournalID(ctx)
			return err
		}},
		{"service apply", func(ctx context.Context, engine *Engine) error { return engine.ServiceApply(ctx, "R1", true) }},
		{"secrets push", func(ctx context.Context, engine *Engine) error {
			_, err := engine.SecretsPushBatch(ctx, nil)
			return err
		}},
		{"destroy", func(ctx context.Context, engine *Engine) error { return engine.Destroy(ctx, true, false) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if strings.Contains(command, "_host/owner") {
					return transport.Result{Stdout: "another-app\n"}, true
				}
				return transport.Result{}, false
			}}
			engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep, ForceLock: true, Environment: "production"})
			err := test.run(context.Background(), engine)
			if err == nil || !strings.Contains(err.Error(), "another-app") {
				t.Fatalf("foreign owner was accepted: %v", err)
			}
			for _, command := range fake.Commands {
				if !strings.Contains(command, "_host/owner") {
					t.Fatalf("foreign-owner refusal performed another target operation: %s", command)
				}
			}
			if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
				t.Fatalf("foreign-owner refusal wrote target input: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
			}
		})
	}
}

func TestHostOwnerReadFailureIsNotTreatedAsUnowned(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "_host/owner") {
			return transport.Result{ExitCode: 1, Stderr: "permission denied"}, true
		}
		return transport.Result{}, false
	}}
	engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := engine.RequireHostOwner(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read host owner record") {
		t.Fatalf("read failure was treated as an unowned host: %v", err)
	}
}

// Exit 4 is the probe's refusal of a record that exists but is not a regular
// file. It carries no stderr, so reporting it as a failed read would print an
// empty reason for a condition preflight names and offers a remedy for.
func TestHostOwnerNonRegularRecordIsNamed(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "_host/owner") {
			return transport.Result{ExitCode: 4}, true
		}
		return transport.Result{}, false
	}}
	engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := engine.RequireHostOwner(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a non-regular owner record was not named: %v", err)
	}
}

// The record is mode 0600, so "present, regular, not ours to read" is its most
// likely refusal — and the probe suppresses stderr for it. Falling through to
// the generic branch would report the path that decides host ownership with an
// exit code and no cause.
func TestHostOwnerUnreadableRecordIsNamed(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "_host/owner") {
			return transport.Result{ExitCode: app.ProbeUnreadable}, true
		}
		return transport.Result{}, false
	}}
	engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := engine.RequireHostOwner(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("an unreadable owner record was not named: %v", err)
	}
}

func TestHostOwnerInvalidRecordFailsClosed(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "_host/owner") {
			return transport.Result{Stdout: "bad/name\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := engine.RequireHostOwner(context.Background()); err == nil || !strings.Contains(err.Error(), "record") || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid owner record was accepted: %v", err)
	}
}

func TestClaimHostOwnerRechecksUnderLock(t *testing.T) {
	reads := 0
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "_host/owner") && strings.Contains(command, "cat ") {
			reads++
			if reads == 1 {
				return transport.Result{ExitCode: 3}, true
			}
			return transport.Result{Stdout: "another-app\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := engine.claimHostOwner(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another-app") {
		t.Fatalf("concurrent owner claim was accepted: %v", err)
	}
	for _, command := range fake.Commands {
		if strings.Contains(command, "printf '%s\\n'") && strings.Contains(command, "_host/owner") {
			t.Fatalf("foreign owner was overwritten: %s", command)
		}
	}
}

func TestClaimHostOwnerReportsAtomicWriteFailure(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "_host/owner") && strings.Contains(command, "cat "):
			return transport.Result{ExitCode: 3}, true
		case strings.Contains(command, "printf '%s\\n'") && strings.Contains(command, "_host/owner"):
			return transport.Result{ExitCode: 74, Stderr: "read-only filesystem"}, true
		default:
			return transport.Result{}, false
		}
	}}
	engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := engine.claimHostOwner(context.Background()); err == nil || !strings.Contains(err.Error(), "record host owner") {
		t.Fatalf("owner write failure was hidden: %v", err)
	}
}

// The structured envelope never renders Error(), so the refused command has to
// reach the consumer through guidance. Handing an operator who ran `ob abort`
// the command `ob resume` proposes the opposite action, and an agent that
// executes resolving commands fixes forward instead of rolling back.
func TestMigrationGateGuidanceMatchesTheRefusedCommand(t *testing.T) {
	for refused, want := range map[string]string{
		// Never the break-glass flag: it classifies as a resolving command,
		// which an agent may run, and running it rolls back a release with
		// rollback-unknown data effects — the decision this gate exists to
		// put in front of a human. A diagnostic is the most an abort refusal
		// may publish.
		"abort":    "ob audit --output json",
		"recovery": "ob resume --output ndjson",
		"":         "ob resume --output ndjson",
	} {
		err := &MigrationGateClosedError{InterruptedID: "R1", Refused: refused}
		got := err.GuidanceCommand()
		if got != want {
			t.Errorf("Refused=%q guidance = %q, want %q", refused, got, want)
		}
		if err.Code() != "migration_gate_closed" {
			t.Errorf("Refused=%q lost its code", refused)
		}
		if strings.Contains(got, "--break-migration-gate") {
			t.Errorf("Refused=%q publishes the break-glass flag as a runnable command", refused)
		}
		// The flag still has to reach a human, who is the one allowed to use it.
		if !strings.Contains(err.Error(), "--break-migration-gate") {
			t.Errorf("Refused=%q no longer names the override for an operator", refused)
		}
	}
}
