package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

func seedServingApplicationManifest(f *transport.Fake, releaseID, predecessor string, outcome release.OperationOutcome) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	manifest, err := release.NewManifest(releaseID, release.KindApplication, at)
	if err != nil {
		panic(err)
	}
	if err := manifest.Transition(release.StateVerified, at.Add(time.Second), ""); err != nil {
		panic(err)
	}
	if err := manifest.Transition(release.StateServing, at.Add(2*time.Second), predecessor); err != nil {
		panic(err)
	}
	if err := manifest.RecordOperationOutcome(outcome, at.Add(3*time.Second)); err != nil {
		panic(err)
	}
	command, input, err := release.ManifestWrite(testConfig().NamesFor("production"), manifest)
	if err != nil {
		panic(err)
	}
	if result, runErr := f.RunInput(context.Background(), command, input); runErr != nil || result.ExitCode != 0 {
		panic("seed serving application manifest")
	}
	f.Commands = nil
	f.Inputs = nil
}

// The snapshot a finalized release replays from. Recovery uses the immutable
// snapshot staged with the release, never the working tree, so the hook under
// test has to live here.
const engineProjectWithPostDeployHook = engineProject + `
hooks:
  post_deploy: {run: notify-release}
`

// activatedFake is a deploy that got past activation and then failed in the
// post-activation steps: the release is current, serving, and healthy, and the
// journal's last record is finish:fail.
func activatedFake(t *testing.T, tail ...journal.Record) *transport.Fake {
	t.Helper()
	f := happyFake()
	// serving/failed is what a post-activation failure actually leaves behind;
	// seeding succeeded would make the outcome assertions vacuous.
	seedServingApplicationManifest(f, engineTestDeployReleaseID, engineTestPreviousReleaseID, release.OutcomeFailed)
	records := []journal.Record{
		{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "deploy", Event: "start", Detail: "prev=" + engineTestPreviousReleaseID, Operator: "v@mac", TS: "2026-07-03T00:00:00Z"},
		{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "pre-release", SubStep: journal.EffectBaselineSubStep, Event: "result", Status: "ok", RollbackSafe: true},
		{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "transfer", Event: "result", Status: "ok"},
		{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "pre-release", SubStep: "job:migrate", Event: "result", Status: "ok", Detail: "changed=false"},
		{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "release", Role: "web", Event: "result", Status: "ok"},
		{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "release", Role: "worker", Event: "result", Status: "ok"},
		{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "verify", Event: "result", Status: "ok"},
		{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "activation", Event: "result", Status: "ok", Detail: "release=" + engineTestDeployReleaseID},
	}
	for _, record := range tail {
		record.DeployID, record.Epoch = engineTestDeployReleaseID, 2
		records = append(records, record)
	}
	records = append(records, journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "deploy", Event: "finish", Status: "fail", Detail: "post-deploy: hook post_deploy failed"})
	lines := journalLines(records...)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal"):
			return transport.Result{Stdout: journalMarkerLine + engineTestDeployReleaseID + ".jsonl\n" + lines}, true
		case strings.Contains(cmd, "test -d"):
			return transport.Result{ExitCode: 0}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestDeployReleaseID + "\n"}, true
		case strings.Contains(cmd, "ob.snapshot.yml"):
			return transport.Result{Stdout: engineProjectWithPostDeployHook}, true
		case strings.Contains(cmd, "docker ps --filter label=ob.app="):
			return transport.Result{Stdout: "NEW1|web|" + engineTestDeployReleaseID + "|Up 2 minutes (healthy)\n" +
				"W1|worker|" + engineTestDeployReleaseID + "|Up 2 minutes\n"}, true
		}
		return base(cmd)
	}
	return f
}

func storedManifest(t *testing.T, f *transport.Fake, releaseID string) release.Manifest {
	t.Helper()
	manifest, err := release.ReadManifest(context.Background(), f, testConfig().NamesFor("production"), releaseID)
	if err != nil {
		t.Fatalf("read manifest %s: %v", releaseID, err)
	}
	return manifest
}

// A project whose job carries a schedule, so the middle post-activation step
// has work to do and can be made to fail.
var engineProjectWithScheduledJob = strings.Replace(engineProject, "services:", `  report:
    role: job
    image: ghcr.io/x/app:v2
    command: report
    when: manual
    data_effect: none
    schedule: {cron: "0 2 * * *", timezone: UTC}
services:`, 1)

func scheduledJobConfig() *app.Resolved {
	spec, err := app.LoadBytes([]byte(engineProjectWithScheduledJob), "ob.yml")
	if err != nil {
		panic("scheduled-job fixture does not load: " + err.Error())
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		panic("scheduled-job fixture does not resolve: " + err.Error())
	}
	if resolved.Hooks == nil {
		resolved.Hooks = map[string]app.Command{}
	}
	return resolved
}

func scheduledJobFake() *transport.Fake {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemd-analyze calendar") {
			return transport.Result{Stdout: "ok\n"}, true
		}
		return base(cmd)
	}
	return f
}

// The contract's own scenario: an operation that fails after healthy activation
// records a failed outcome and keeps the truthful serving state.
func TestPostActivationFailureRecordsAFailedOutcomeAndKeepsServing(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "notify-release") {
			return transport.Result{ExitCode: 1, Stderr: "webhook unreachable"}, true
		}
		return base(cmd)
	}
	cfg := testConfig()
	cfg.Hooks["post_deploy"] = app.Command{Run: "notify-release"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "post-deploy:") {
		t.Fatalf("deploy error = %v, want a post-deploy failure", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "ln -sfn 'releases/20260101-000000-aaa111'") {
		t.Fatalf("the release must still activate:\n%s", seq)
	}
	manifest := storedManifest(t, f, "20260101-000000-aaa111")
	if manifest.State != release.StateServing || manifest.OperationOutcome != release.OutcomeFailed {
		t.Fatalf("manifest = %s/%s, want serving/failed", manifest.State, manifest.OperationOutcome)
	}
	for _, want := range []string{
		`"phase":"finalize","sub_step":"finalize:retention","event":"result","status":"ok"`,
		`"phase":"finalize","sub_step":"finalize:post_deploy","event":"result","status":"fail"`,
	} {
		if !strings.Contains(seq, want) {
			t.Fatalf("journal record missing %s:\n%s", want, seq)
		}
	}
}

// Resume after activation completes only what remains: no role is rolled, the
// symlink is not switched again, and the completed steps do not repeat.
func TestResumeFinalizesAfterActivationWithoutReplayingTheDeploy(t *testing.T) {
	f := activatedFake(t,
		journal.Record{Phase: "finalize", SubStep: finalizeRetentionSubStep, Event: "result", Status: "ok"},
		journal.Record{Phase: "finalize", SubStep: finalizeSchedulesSubStep, Event: "result", Status: "ok"},
		journal.Record{Phase: "finalize", SubStep: finalizePostDeploySubStep, Event: "result", Status: "fail", Detail: "webhook unreachable"},
	)
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	for _, forbidden := range []string{"--scale web=", "--force-recreate --timeout 30 worker", "ln -sfn 'releases/" + engineTestDeployReleaseID + "'", "OB_RESULT_FILE"} {
		if strings.Contains(seq, forbidden) {
			t.Fatalf("finalize must not replay the deploy (%s):\n%s", forbidden, seq)
		}
	}
	if !strings.Contains(seq, "notify-release") {
		t.Fatalf("the step that failed must run again:\n%s", seq)
	}
	if !strings.Contains(seq, `"event":"finish","status":"ok"`) {
		t.Fatalf("finalize must reach a terminal state:\n%s", seq)
	}
	manifest := storedManifest(t, f, engineTestDeployReleaseID)
	if manifest.State != release.StateServing || manifest.OperationOutcome != release.OutcomeSucceeded {
		t.Fatalf("manifest = %s/%s, want serving/succeeded", manifest.State, manifest.OperationOutcome)
	}
}

// A post-deploy hook is not idempotent, so a step the journal already records
// as successful must never run a second time.
func TestFinalizeDoesNotRepeatACompletedPostDeployHook(t *testing.T) {
	f := activatedFake(t,
		journal.Record{Phase: "finalize", SubStep: finalizePostDeploySubStep, Event: "result", Status: "ok"},
		journal.Record{Phase: "finalize", SubStep: finalizeRetentionSubStep, Event: "result", Status: "fail", Detail: "transport lost"},
	)
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "notify-release") {
		t.Fatalf("a completed post-deploy hook must not repeat:\n%s", seq)
	}
}

// Every evidence source must agree. Each disagreement is checked on its own so
// a future change cannot silently drop one of them and stay green.
func TestFinalizeRefusesWhenActivationEvidenceDisagrees(t *testing.T) {
	for _, tt := range []struct {
		name    string
		arrange func(f *transport.Fake)
		want    string
	}{
		{
			name: "current release is a different one",
			arrange: func(f *transport.Fake) {
				base := f.Dynamic
				f.Dynamic = func(cmd string) (transport.Result, bool) {
					if strings.Contains(cmd, "readlink") {
						return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
					}
					return base(cmd)
				}
			},
			want: "the current release is " + engineTestPreviousReleaseID,
		},
		{
			name: "manifest predecessor is not the recorded one",
			arrange: func(f *transport.Fake) {
				seedServingApplicationManifest(f, engineTestDeployReleaseID, "20251231-000000-other", release.OutcomeFailed)
			},
			want: "is not the predecessor",
		},
		{
			name: "an activation checkpoint is still open",
			arrange: func(f *transport.Fake) {
				checkpoint, err := release.NewActivationCheckpoint(engineTestDeployReleaseID, engineTestPreviousReleaseID, release.ActivationPrepared, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatal(err)
				}
				command, input, err := release.ActivationCheckpointWrite(testConfig().NamesFor("production"), checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.RunInput(context.Background(), command, input); err != nil {
					t.Fatal(err)
				}
			},
			want: "an activation checkpoint is still open",
		},
		{
			name: "a workload runs another release",
			arrange: func(f *transport.Fake) {
				base := f.Dynamic
				f.Dynamic = func(cmd string) (transport.Result, bool) {
					if strings.Contains(cmd, "docker ps --filter label=ob.app=") {
						return transport.Result{Stdout: "NEW1|web|" + engineTestDeployReleaseID + "|Up (healthy)\n" +
							"W1|worker|" + engineTestPreviousReleaseID + "|Up\n"}, true
					}
					return base(cmd)
				}
			},
			want: "workload worker runs release " + engineTestPreviousReleaseID,
		},
		{
			name: "a workload is not running",
			arrange: func(f *transport.Fake) {
				base := f.Dynamic
				f.Dynamic = func(cmd string) (transport.Result, bool) {
					if strings.Contains(cmd, "docker ps --filter label=ob.app=") {
						return transport.Result{Stdout: "NEW1|web|" + engineTestDeployReleaseID + "|Up (healthy)\n"}, true
					}
					return base(cmd)
				}
			},
			want: "workload worker is not running",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := activatedFake(t)
			tt.arrange(f)
			e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			err := e.Resume(context.Background())
			var refused *FinalizeRefusedError
			if !errors.As(err, &refused) || refused.Code() != "finalize_refused" {
				t.Fatalf("resume error = %v, want a typed finalize_refused", err)
			}
			if !strings.Contains(refused.Reason, tt.want) {
				t.Fatalf("reason = %q, want it to name %q", refused.Reason, tt.want)
			}
			if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "notify-release") || strings.Contains(seq, "rm -rf") {
				t.Fatalf("a refused finalize must not mutate the host:\n%s", seq)
			}
		})
	}
}

// A serving manifest alone is not authority to finalize: without a recorded
// activation this operation cannot claim the release that is live.
func TestFinalizeRefusesWithoutJournaledActivation(t *testing.T) {
	f := happyFake()
	seedServingApplicationManifest(f, engineTestDeployReleaseID, engineTestPreviousReleaseID, release.OutcomeFailed)
	lines := journalLines(
		journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "deploy", Event: "start", Detail: "prev=" + engineTestPreviousReleaseID, TS: "2026-07-03T00:00:00Z"},
		journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "transfer", Event: "result", Status: "ok"},
	)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal"):
			return transport.Result{Stdout: journalMarkerLine + engineTestDeployReleaseID + ".jsonl\n" + lines}, true
		case strings.Contains(cmd, "test -d"):
			return transport.Result{ExitCode: 0}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestDeployReleaseID + "\n"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	var refused *FinalizeRefusedError
	if err := e.Resume(context.Background()); !errors.As(err, &refused) {
		t.Fatalf("resume error = %v, want finalize_refused", err)
	}
	if !strings.Contains(refused.Reason, "no successful activation") {
		t.Fatalf("reason = %q", refused.Reason)
	}
}

// Retention refuses to select candidates when the chain evidence is incomplete.
// That is the contract, and it must not cost the deploy its terminal state: the
// release is healthy and serving, and cleanup can happen on any later run.
func TestRetentionEvidenceRefusalIsReportedAndDoesNotFailTheDeploy(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "/var/lib/ob/sample/activation.json") && strings.Contains(cmd, "printf 'mode=%s") {
			return transport.Result{Stdout: "mode=600\n{ this is not a checkpoint"}, true
		}
		return base(cmd)
	}
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !strings.Contains(out.String(), "release-store cleanup skipped") {
		t.Fatalf("the skipped cleanup must be reported:\n%s", out.String())
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, `"sub_step":"finalize:retention","event":"result","status":"ok","detail":"`+retentionSkipped+`"`) {
		t.Fatalf("the journal must record what cleanup declined to do:\n%s", seq)
	}
	// Host stderr reaches the operator on the local path and never the journal,
	// which is why the durable detail is a fixed phrase.
	if strings.Contains(seq, "this is not a checkpoint") {
		t.Fatalf("the durable detail must not carry host output:\n%s", seq)
	}
	// Journals are the evidence that protects release directories with no
	// readable manifest. The run that just declared the evidence incomplete must
	// not delete them either.
	if strings.Contains(seq, "rm -f '/var/lib/ob/sample/journal/") {
		t.Fatalf("a refused retention must not prune journals:\n%s", seq)
	}
	if !strings.Contains(seq, `"event":"finish","status":"ok"`) {
		t.Fatalf("the deploy must still reach a terminal state:\n%s", seq)
	}
	manifest := storedManifest(t, f, "20260101-000000-aaa111")
	if manifest.State != release.StateServing || manifest.OperationOutcome != release.OutcomeSucceeded {
		t.Fatalf("manifest = %s/%s, want serving/succeeded", manifest.State, manifest.OperationOutcome)
	}
}

// A refusal to select is not the same as a failure to delete: a prune command
// that fails is a real error and still fails the deploy.
func TestRetentionDeletionFailureStillFailsTheDeploy(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "ls -1A") || strings.Contains(cmd, "ls -1 '/var/lib/ob/sample/releases'"):
			return transport.Result{Stdout: "20250101-000000-old\n20260101-000000-aaa111\n"}, true
		case strings.Contains(cmd, "rm -rf '/var/lib/ob/sample/releases/20250101-000000-old'"):
			return transport.Result{ExitCode: 1, Stderr: "read-only file system"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "prune") {
		t.Fatalf("deploy error = %v, want a prune failure", err)
	}
	manifest := storedManifest(t, f, "20260101-000000-aaa111")
	if manifest.State != release.StateServing || manifest.OperationOutcome != release.OutcomeFailed {
		t.Fatalf("manifest = %s/%s, want serving/failed", manifest.State, manifest.OperationOutcome)
	}
}

// The predecessor is what proves this operation activated the release now
// serving, and every resume appends its own start record naming the live
// current pointer — which after activation is the release being finalized. If
// that later record won, a second resume would compare the release against
// itself and refuse forever: the fix-forward loop this whole path exists for is
// exactly the case where the first resume fails too.
func TestFinalizeSurvivesARepeatedResume(t *testing.T) {
	f := activatedFake(t,
		journal.Record{Phase: "finalize", SubStep: finalizeRetentionSubStep, Event: "result", Status: "ok"},
		journal.Record{Phase: "finalize", SubStep: finalizeSchedulesSubStep, Event: "result", Status: "ok"},
		journal.Record{Phase: "finalize", SubStep: finalizePostDeploySubStep, Event: "result", Status: "fail", Detail: "webhook unreachable"},
		// what the first resume appended before failing again
		journal.Record{Phase: "deploy", Event: "start", Detail: "prev=" + engineTestDeployReleaseID},
		journal.Record{Phase: "finalize", SubStep: finalizePostDeploySubStep, Event: "result", Status: "fail", Detail: "webhook unreachable"},
	)
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("the second resume must still finalize: %v", err)
	}
	manifest := storedManifest(t, f, engineTestDeployReleaseID)
	if manifest.OperationOutcome != release.OutcomeSucceeded {
		t.Fatalf("outcome = %s, want succeeded", manifest.OperationOutcome)
	}
}

// The health gate before the tail. A serving release that no longer verifies is
// not finalized, is not auto-rolled-back, and records the failed outcome like
// any other post-activation failure.
func TestFinalizeStopsAtAFailingVerification(t *testing.T) {
	f := activatedFake(t)
	// A runner that died between activation and the tail leaves the outcome
	// activation itself recorded — succeeded. The failed verification is what
	// must move it, so seeding anything else would assert nothing.
	seedServingApplicationManifest(f, engineTestDeployReleaseID, engineTestPreviousReleaseID, release.OutcomeSucceeded)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "curl") {
			return transport.Result{ExitCode: 22, Stderr: "404"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Resume(context.Background())
	if err == nil || !strings.Contains(err.Error(), "verify serving release") {
		t.Fatalf("resume error = %v, want a verification failure", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "notify-release") || strings.Contains(seq, `"phase":"finalize"`) {
		t.Fatalf("no post-activation step may run after a failed verification:\n%s", seq)
	}
	if strings.Contains(seq, "ln -sfn 'releases/"+engineTestPreviousReleaseID+"'") {
		t.Fatalf("a failed finalize verification must not roll back a serving release:\n%s", seq)
	}
	manifest := storedManifest(t, f, engineTestDeployReleaseID)
	if manifest.State != release.StateServing || manifest.OperationOutcome != release.OutcomeFailed {
		t.Fatalf("manifest = %s/%s, want serving/failed", manifest.State, manifest.OperationOutcome)
	}
}

// The middle step has the same contract as the two either side of it.
func TestScheduleSyncFailureIsAPostActivationFailure(t *testing.T) {
	f := scheduledJobFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl enable --now") {
			return transport.Result{ExitCode: 1, Stderr: "systemd is not running"}, true
		}
		return base(cmd)
	}
	cfg := scheduledJobConfig()
	cfg.Hooks["post_deploy"] = app.Command{Run: "notify-release"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "schedules:") {
		t.Fatalf("deploy error = %v, want a schedules failure", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, `"sub_step":"finalize:schedules","event":"result","status":"fail"`) {
		t.Fatalf("the failed step must be journaled:\n%s", seq)
	}
	if strings.Contains(seq, "notify-release") {
		t.Fatalf("a later step must not run after an earlier one failed:\n%s", seq)
	}
	manifest := storedManifest(t, f, "20260101-000000-aaa111")
	if manifest.State != release.StateServing || manifest.OperationOutcome != release.OutcomeFailed {
		t.Fatalf("manifest = %s/%s, want serving/failed", manifest.State, manifest.OperationOutcome)
	}
}

// Schedules sync before the post-deploy hook so a hook that inspects the
// schedule sees the one this release declares. Order is a contract, not an
// accident of the table's layout.
func TestScheduleSyncRunsBeforeThePostDeployHook(t *testing.T) {
	f := scheduledJobFake()
	cfg := scheduledJobConfig()
	cfg.Hooks["post_deploy"] = app.Command{Run: "notify-release"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	schedules, hook := strings.Index(seq, "systemctl enable --now"), strings.Index(seq, "notify-release")
	if schedules < 0 || hook < 0 || schedules > hook {
		t.Fatalf("schedules must sync before the post-deploy hook (schedules=%d hook=%d):\n%s", schedules, hook, seq)
	}
}
