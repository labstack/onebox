package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

const (
	rollbackPreviousID = "20260101-000000-aaa111"
	rollbackCurrentID  = "20260102-000000-bbb222"
)

func seedRollbackState(t *testing.T, target *transport.Fake) {
	t.Helper()
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	previous, err := release.NewManifest(rollbackPreviousID, release.KindApplication, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []release.State{release.StateVerified, release.StateServing, release.StateSuperseded} {
		if err := previous.Transition(state, at, ""); err != nil {
			t.Fatal(err)
		}
	}
	current, err := release.NewManifest(rollbackCurrentID, release.KindApplication, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Transition(release.StateVerified, at, ""); err != nil {
		t.Fatal(err)
	}
	if err := current.Transition(release.StateServing, at, rollbackPreviousID); err != nil {
		t.Fatal(err)
	}
	names := testConfig().NamesFor("production")
	if err := release.WriteManifest(context.Background(), target, names, previous); err != nil {
		t.Fatal(err)
	}
	if err := release.WriteManifest(context.Background(), target, names, current); err != nil {
		t.Fatal(err)
	}
}

// The previous release's snapshot has a DIFFERENT choreography (worker only,
// recreate) — rollback must replay THAT, not the current ob.yml.
const oldSnapshot = `
api_version: onebox.run/v1
app: sample
environments: { production: { server: deploy@h } }
workloads:
  worker: { role: worker, image: ghcr.io/x/app:v1, command: work, strategy: recreate }
deployment:
  order: [worker]
`

func TestRollbackReplaysSnapshotChoreography(t *testing.T) {
	f := happyFake()
	seedRollbackState(t, f)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/20260102-000000-bbb222\n"}, true
		}
		if strings.Contains(cmd, "ls -1") {
			return transport.Result{Stdout: "20260101-000000-aaa111\n20260102-000000-bbb222\n"}, true
		}
		if strings.Contains(cmd, "ob.snapshot.yml") {
			return transport.Result{Stdout: oldSnapshot}, true
		}
		return base(cmd)
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep, Environment: "production"})
	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "--force-recreate --timeout 30 worker") {
		t.Fatalf("snapshot choreography (worker recreate) not replayed:\n%s", seq)
	}
	// current ob.yml rolls web — snapshot doesn't; web must NOT be touched
	if strings.Contains(seq, "--scale web=2") {
		t.Fatalf("rollback used CURRENT config choreography instead of snapshot:\n%s", seq)
	}
}

func TestRollbackRefusesWithoutUsableSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name     string
		snapshot transport.Result
		want     string
	}{
		{name: "missing", snapshot: transport.Result{ExitCode: 1, Stderr: "No such file"}, want: "snapshot unavailable"},
		{name: "invalid", snapshot: transport.Result{Stdout: "not: [valid"}, want: "snapshot unusable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := happyFake()
			seedRollbackState(t, f)
			base := f.Dynamic
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch {
				case strings.Contains(cmd, "readlink"):
					return transport.Result{Stdout: "releases/20260102-000000-bbb222\n"}, true
				case strings.Contains(cmd, "ls -1"):
					return transport.Result{Stdout: "20260101-000000-aaa111\n20260102-000000-bbb222\n"}, true
				case strings.Contains(cmd, "ob.snapshot.yml"):
					return tt.snapshot, true
				}
				return base(cmd)
			}
			e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
			err := e.Rollback(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("rollback error = %v, want %q", err, tt.want)
			}
			if strings.Contains(strings.Join(f.Commands, "\n"), "ob-fenced") {
				t.Fatalf("rollback must fail before mutation:\n%s", strings.Join(f.Commands, "\n"))
			}
		})
	}
}

func TestRollbackReportsMissingFinishEvidence(t *testing.T) {
	f := happyFake()
	seedRollbackState(t, f)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/20260102-000000-bbb222\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "20260101-000000-aaa111\n20260102-000000-bbb222\n"}, true
		case strings.Contains(cmd, `"phase":"rollback","event":"finish","status":"ok"`):
			return transport.Result{ExitCode: 74, Stderr: "journal is read-only"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Rollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "journal rollback finish") {
		t.Fatalf("rollback error = %v", err)
	}
	if seq := strings.Join(f.Commands, "\n"); !strings.Contains(seq, "ln -sfn 'releases/20260101-000000-aaa111'") {
		t.Fatalf("rollback must report that activation completed before evidence failed:\n%s", seq)
	}
}

func TestRepeatedRollbackFollowsTheNewPredecessor(t *testing.T) {
	target := happyFake()
	seedRollbackState(t, target)
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "readlink") {
			current := rollbackCurrentID
			for _, recorded := range target.Commands {
				switch {
				case strings.Contains(recorded, "ln -sfn 'releases/"+rollbackPreviousID+"'"):
					current = rollbackPreviousID
				case strings.Contains(recorded, "ln -sfn 'releases/"+rollbackCurrentID+"'"):
					current = rollbackCurrentID
				}
			}
			return transport.Result{Stdout: "releases/" + current + "\n"}, true
		}
		if strings.Contains(command, "/releases/"+rollbackPreviousID+"/ob.snapshot.yml") {
			return transport.Result{Stdout: oldSnapshot}, true
		}
		return base(command)
	}
	engine := New(testConfig(), testProject(t), target, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	first, err := engine.RollbackWithJournalID(context.Background())
	if err != nil || first != rollbackPreviousID {
		t.Fatalf("first rollback = %q, %v", first, err)
	}
	second, err := engine.RollbackWithJournalID(context.Background())
	if err != nil || second != rollbackCurrentID {
		t.Fatalf("second rollback = %q, %v", second, err)
	}
	commands := strings.Join(target.Commands, "\n")
	firstAt := strings.Index(commands, "ln -sfn 'releases/"+rollbackPreviousID+"'")
	secondAt := strings.LastIndex(commands, "ln -sfn 'releases/"+rollbackCurrentID+"'")
	if firstAt < 0 || secondAt <= firstAt {
		t.Fatalf("rollback did not toggle deterministically:\n%s", commands)
	}
	current, err := release.ReadManifest(context.Background(), target, engine.Names(), rollbackCurrentID)
	if err != nil || current.State != release.StateServing || current.Predecessor != rollbackPreviousID {
		t.Fatalf("reactivated current manifest = %+v, %v", current, err)
	}
	previous, err := release.ReadManifest(context.Background(), target, engine.Names(), rollbackPreviousID)
	if err != nil || previous.State != release.StateSuperseded {
		t.Fatalf("superseded previous manifest = %+v, %v", previous, err)
	}
}
