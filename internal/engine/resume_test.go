package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/transport"
)

func journalLines(recs ...journal.Record) string {
	var lines []string
	for _, r := range recs {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	return strings.Join(lines, "\n") + "\n"
}

// interruptedFake: R1 crashed after web rolled; worker pending. R0 is live.
func interruptedFake(gateDetail string) *transport.Fake {
	return interruptedFakeWithPolicy(gateDetail, false)
}

func interruptedFakeWithPolicy(gateDetail string, policySafe bool) *transport.Fake {
	f := happyFake()
	jr := journalLines(
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "deploy", Event: "start", Detail: "prev=R0", Operator: "v@mac", TS: "2026-07-03T00:00:00Z"},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "transfer", Event: "result", Status: "ok"},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "pre-release", SubStep: "migrate", Event: "result", Status: "ok", Detail: gateDetail, RollbackPolicySafe: policySafe},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "release", Role: "web", Event: "result", Status: "ok"},
	)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal"):
			return transport.Result{Stdout: journalMarkerLine + "R1.jsonl\n" + jr}, true
		case strings.Contains(cmd, "test -d"):
			return transport.Result{ExitCode: 0}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R0\n"}, true
		case strings.Contains(cmd, "ls -1 '/var/lib/ob/sample/releases'"):
			return transport.Result{Stdout: "R0\nR1\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestResumeSkipsCompletedStepsAndFinishes(t *testing.T) {
	f := interruptedFake("changed=false")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	if !e.gateOpen {
		t.Fatal("resume must restore the completed job's changed=false gate result")
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "--scale web=2") {
		t.Fatalf("web already rolled — resume must skip it:\n%s", seq)
	}
	if strings.Contains(seq, "OB_RESULT_FILE") {
		t.Fatalf("migrate already ran — resume must not re-run it:\n%s", seq)
	}
	if !strings.Contains(seq, "--force-recreate --timeout 30 worker") {
		t.Fatalf("pending worker must be released:\n%s", seq)
	}
	if !strings.Contains(seq, "ln -sfn 'releases/R1'") {
		t.Fatalf("resumed deploy must activate:\n%s", seq)
	}
	if !strings.Contains(seq, `"event":"finish","status":"ok"`) {
		t.Fatalf("resumed deploy must journal finish:\n%s", seq)
	}
}

func TestResumeUsesInterruptedReleaseSnapshotAfterConfigEdit(t *testing.T) {
	f := happyFake()
	jr := journalLines(
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "deploy", Event: "start", Detail: "prev=R0", TS: "2026-07-03T00:00:00Z"},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "pre-release", SubStep: journal.EffectBaselineSubStep, Event: "result", Status: "ok", RollbackSafe: true},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "transfer", Event: "result", Status: "ok"},
	)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal"):
			return transport.Result{Stdout: journalMarkerLine + "R1.jsonl\n" + jr}, true
		case strings.Contains(cmd, "/releases/R1/ob.snapshot.yml"):
			return transport.Result{Stdout: oldSnapshot}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R0\n"}, true
		}
		return base(cmd)
	}

	cfg := testConfig()
	cfg.Workloads = map[string]app.Workload{"web": cfg.Workloads["web"]}
	cfg.Deployment.Order = []string{"web"}
	cfg.Services = nil
	cfg.Verifications = nil
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "--force-recreate --timeout 30 worker") {
		t.Fatalf("resume did not use the interrupted snapshot's worker choreography:\n%s", seq)
	}
	if strings.Contains(seq, "--scale web=") {
		t.Fatalf("resume used the edited working-tree web choreography:\n%s", seq)
	}
}

func TestResumeRefusesMissingInterruptedSnapshot(t *testing.T) {
	f := interruptedFake("changed=false")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "/releases/R1/ob.snapshot.yml") {
			return transport.Result{ExitCode: 1, Stderr: "No such file"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	id, err := e.ResumeWithJournalID(context.Background())
	if id != "R1" || err == nil || !strings.Contains(err.Error(), "snapshot unavailable") {
		t.Fatalf("resume id/error = %q, %v", id, err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "ob-fenced") {
		t.Fatalf("resume must fail before mutation:\n%s", strings.Join(f.Commands, "\n"))
	}
}

func interruptedBeforeMigrationFake(allowUnknown bool) *transport.Fake {
	f := happyFake()
	jr := journalLines(
		journal.Record{
			DeployID: "R1", Epoch: 2, Phase: "deploy", Event: "start", Detail: "prev=R0",
			Operator: "v@mac", TS: "2026-07-03T00:00:00Z",
			ApprovalDigest: "sha256:approved", ApprovalClass: "strong",
			AllowUnknownMigration: allowUnknown,
		},
		journal.Record{
			DeployID: "R1", Epoch: 2, Phase: "pre-release", SubStep: journal.EffectBaselineSubStep,
			Event: "result", Status: "ok", RollbackSafe: true,
		},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "transfer", Event: "result", Status: "ok"},
	)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal"):
			return transport.Result{Stdout: journalMarkerLine + "R1.jsonl\n" + jr}, true
		case strings.Contains(cmd, "ob.snapshot.yml"):
			return transport.Result{Stdout: strings.Replace(engineProject, "data_effect: unknown", "data_effect: migration", 1)}, true
		case strings.Contains(cmd, "test -d"):
			return transport.Result{ExitCode: 0}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R0\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestResumeRestoresOnlyExplicitUnknownMigrationAuthority(t *testing.T) {
	for _, tt := range []struct {
		name    string
		allowed bool
		wantErr bool
	}{
		{name: "plan bound authority", allowed: true},
		{name: "schema-less strong approval is not enough", allowed: false, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := interruptedBeforeMigrationFake(tt.allowed)
			cfg := testConfig()
			cfg.Workloads["migrate"] = app.Workload{Role: app.RoleJob, When: "pre_release", DataEffect: "migration"}
			var out bytes.Buffer
			e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep})
			err := e.Resume(context.Background())
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "no strong plan-bound approval") {
					t.Fatalf("resume error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, strings.Join(f.Commands, "\n"))
			}
			if !e.Opts.AllowUnknownMigration || !strings.Contains(out.String(), "authorized by strong plan-bound approval") {
				t.Fatalf("plan-bound unknown authority was not restored: opts=%v output=%s", e.Opts.AllowUnknownMigration, out.String())
			}
		})
	}
}

func TestResumeWithNothingIncomplete(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal") {
			return transport.Result{Stdout: journalMarkerLine + "R1.jsonl\n" + journalLines(
				journal.Record{DeployID: "R1", Phase: "deploy", Event: "start"},
				journal.Record{DeployID: "R1", Phase: "deploy", Event: "finish", Status: "ok"},
			)}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err == nil || !strings.Contains(err.Error(), "no incomplete") {
		t.Fatalf("want no-incomplete error, got %v", err)
	}
}

func TestAbortRefusesClosedGate(t *testing.T) {
	f := interruptedFake("changed=unknown (no result declared — gate closed, fail-safe)")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Abort(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("closed gate must refuse abort: %v", err)
	}
}

func TestAbortUsesInterruptedEffectPolicyAfterConfigEdit(t *testing.T) {
	f := interruptedFake("changed=unknown (no result declared — gate closed, fail-safe)")
	cfg := testConfig()
	cfg.Deployment.MigrationPolicy = "expand-only"
	cfg.Workloads = map[string]app.Workload{
		// The current config now claims this is a covered migration. Abort must
		// still honor the interrupted journal, which recorded it as uncovered.
		"migrate": {Role: app.RoleJob, When: "pre_release", DataEffect: "migration"},
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Abort(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("a config edit must not cover the interrupted job retroactively: %v", err)
	}
}

func TestAbortExpandOnlyDoesNotCoverLifecycleHook(t *testing.T) {
	f := interruptedFake("changed=unknown (no result declared — gate closed, fail-safe)")
	cfg := testConfig()
	cfg.Deployment.MigrationPolicy = "expand-only"
	cfg.Workloads = map[string]app.Workload{
		"migrate": {Role: app.RoleJob, When: "pre_release", DataEffect: "migration"},
	}
	cfg.Hooks["pre_release"] = app.Command{Run: "true"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Abort(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("expand-only must not cover an untyped lifecycle hook during abort: %v", err)
	}
}

func testAbortReplaysPreviousRelease(t *testing.T, gateDetail string, policySafe bool) {
	t.Helper()
	f := interruptedFakeWithPolicy(gateDetail, policySafe)
	// abort path: web rolled to R1 — its container carries ob.release='R1';
	// replaying R0 must drain it. The fake: newcomer query for R0 returns the
	// R0 container only after R0's up --scale ran.
	base := f.Dynamic
	r0Scaled := func() bool {
		for _, c := range f.Commands {
			if strings.Contains(c, "releases/R0/compose.yaml' up -d --no-deps --no-recreate --scale web=2") {
				return true
			}
		}
		return false
	}
	oldGone := func() bool {
		for _, c := range f.Commands {
			if strings.Contains(c, "docker rm OLD1") {
				return true
			}
		}
		return false
	}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "ob.release='R0'") && strings.Contains(cmd, "service='web'") {
			if r0Scaled() {
				return transport.Result{Stdout: "PREV1\n"}, true
			}
			return transport.Result{Stdout: ""}, true
		}
		// live server set: OLD1 (the R1 container being replaced) until removed,
		// plus the R0 newcomer PREV1 once the R0 scale ran.
		if strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service='web'") && !strings.Contains(cmd, "ob.release=") {
			var ids []string
			if !oldGone() {
				ids = append(ids, "OLD1")
			}
			if r0Scaled() {
				ids = append(ids, "PREV1")
			}
			return transport.Result{Stdout: strings.Join(ids, "\n") + "\n"}, true
		}
		if strings.Contains(cmd, "ob.release='R0'") && strings.Contains(cmd, "service='worker'") {
			return transport.Result{Stdout: ""}, true // worker never completed → recreate from R0
		}
		if strings.Contains(cmd, "ob.release='R1'") {
			return transport.Result{Stdout: ""}, true // straggler sweep finds none
		}
		if strings.Contains(cmd, "inspect") && strings.Contains(cmd, "PREV1") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		return base(cmd)
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Abort(context.Background(), false); err != nil {
		t.Fatalf("abort: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "releases/R0/compose.yaml' up -d --no-deps --no-recreate --scale web=2") {
		t.Fatalf("abort must roll web back to R0:\n%s", seq)
	}
	if !strings.Contains(seq, `"event":"abort","status":"ok"`) {
		t.Fatalf("abort must journal:\n%s", seq)
	}
}

func TestAbortReplaysPreviousRelease(t *testing.T) {
	testAbortReplaysPreviousRelease(t, "changed=false", false)
}

func TestAbortAllowsRecoveredDataEffectNoneGate(t *testing.T) {
	testAbortReplaysPreviousRelease(t, "rollback-safe by data_effect=none declaration", false)
}

func TestAbortUsesInterruptedExpandOnlyPolicyAfterConfigEdit(t *testing.T) {
	// The interrupted deploy recorded expand-only coverage. A later working-tree
	// edit back to manual policy must not turn that historical decision into an
	// unsafe refusal or, in the opposite direction, weaken an uncovered attempt.
	testAbortReplaysPreviousRelease(t, "changed=unknown", true)
}

const interruptedWebSnapshot = `
api_version: onebox.run/v1
app: sample
environments: { production: { server: deploy@h } }
workloads:
  web:
    role: application
    image: ghcr.io/x/app:v2
    health: {http: /healthz, port: 7500}
deployment:
  order: [web]
`

func TestAbortUsesBothReleaseSnapshotsAfterConfigEdit(t *testing.T) {
	f := happyFake()
	jr := journalLines(
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "deploy", Event: "start", Detail: "prev=R0", TS: "2026-07-03T00:00:00Z"},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "pre-release", SubStep: journal.EffectBaselineSubStep, Event: "result", Status: "ok", RollbackSafe: true},
		journal.Record{DeployID: "R1", Epoch: 2, Phase: "transfer", Event: "result", Status: "ok"},
	)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal"):
			return transport.Result{Stdout: journalMarkerLine + "R1.jsonl\n" + jr}, true
		case strings.Contains(cmd, "/releases/R1/ob.snapshot.yml"):
			return transport.Result{Stdout: interruptedWebSnapshot}, true
		case strings.Contains(cmd, "/releases/R0/ob.snapshot.yml"):
			return transport.Result{Stdout: oldSnapshot}, true
		case strings.Contains(cmd, "service='worker'") && strings.Contains(cmd, "ob.release='R0'"):
			return transport.Result{}, true
		case strings.Contains(cmd, "service='web'") && strings.Contains(cmd, "ob.release='R1'"):
			return transport.Result{Stdout: "NEW1\n"}, true
		}
		return base(cmd)
	}

	cfg := testConfig()
	cfg.Workloads = map[string]app.Workload{"migrate": cfg.Workloads["migrate"]}
	cfg.Deployment.Order = nil
	cfg.Services = nil
	cfg.Verifications = nil
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Abort(context.Background(), false); err != nil {
		t.Fatalf("abort: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "--force-recreate --timeout 30 worker") {
		t.Fatalf("abort did not restore the previous snapshot's worker:\n%s", seq)
	}
	if !strings.Contains(seq, "docker stop -t 10 NEW1 && docker rm NEW1") {
		t.Fatalf("abort did not sweep the interrupted snapshot's web newcomer:\n%s", seq)
	}
}

const gateClosed = "changed=unknown (no result declared — gate closed, fail-safe)"

// An unreadable previous snapshot is not a gate an operator can assert past.
// --force asserts schema compatibility for the migration gate; it cannot supply
// the choreography of a release whose snapshot is gone — the release's Compose
// document records images and healthchecks but not its strategies, ordering or
// verification. Falling back to the interrupted release's choreography instead
// would revert only the roles THIS deploy happens to declare and then report the
// previous release as serving while the rest of it stayed down. So every case
// refuses, and refuses before the lock is taken or the intent is journaled.
func TestAbortRefusesUnreadablePreviousSnapshot(t *testing.T) {
	for _, prev := range []struct {
		name string
		res  transport.Result
	}{
		{"unparseable", transport.Result{Stdout: "app: sample\ncomponents: {}\n"}},
		{"absent", transport.Result{ExitCode: 1, Stderr: "cat: No such file or directory"}},
	} {
		for _, gate := range []struct {
			name   string
			detail string
			force  bool
		}{
			{"open", "changed=false", false},
			{"open-forced", "changed=false", true},
			// The case where --force carries real authority: it opens the
			// migration gate, and the abort must still stop at the snapshot.
			{"closed-forced", gateClosed, true},
		} {
			t.Run(fmt.Sprintf("%s/gate=%s", prev.name, gate.name), func(t *testing.T) {
				f := interruptedFake(gate.detail)
				base := f.Dynamic
				f.Dynamic = func(cmd string) (transport.Result, bool) {
					if strings.Contains(cmd, "/releases/R0/ob.snapshot.yml") {
						return prev.res, true
					}
					return base(cmd)
				}

				e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
				err := e.Abort(context.Background(), gate.force)
				// R0 is named so this cannot pass on a refusal of R1, the
				// interrupted release, which is read first by the same call.
				if err == nil || !strings.Contains(err.Error(), "recovery refused: release R0") {
					t.Fatalf("abort error = %v", err)
				}
				if strings.Contains(strings.Join(f.Commands, "\n"), `phase":"abort","event":"intent`) {
					t.Fatalf("abort mutated after refusing the snapshot:\n%s", strings.Join(f.Commands, "\n"))
				}
			})
		}
	}
}

func TestAbortStopsWhenIntentCannotBeJournaled(t *testing.T) {
	f := interruptedFake("changed=false")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, `"phase":"abort","event":"intent"`) {
			return transport.Result{ExitCode: 74, Stderr: "journal is read-only"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Abort(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "journal abort intent") {
		t.Fatalf("abort error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "releases/R0/compose.yaml' pull") {
		t.Fatalf("abort mutated workloads after its intent write failed:\n%s", seq)
	}
}
