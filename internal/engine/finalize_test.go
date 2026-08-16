package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
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
// activatedFakeWithoutActivationResult is the state a failed activation-journal
// append leaves: the intent is recorded, the result never landed.
func activatedFakeWithoutActivationResult(t *testing.T, tail ...journal.Record) *transport.Fake {
	t.Helper()
	return buildActivatedFake(t, false, tail...)
}

func activatedFake(t *testing.T, tail ...journal.Record) *transport.Fake {
	t.Helper()
	return buildActivatedFake(t, true, tail...)
}

func buildActivatedFake(t *testing.T, activationResult bool, tail ...journal.Record) *transport.Fake {
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
	}
	if activationResult {
		records = append(records, journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "activation", Event: "result", Status: "ok", Detail: "release=" + engineTestDeployReleaseID})
	} else {
		records = append(records, journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "activation", Event: "intent", Detail: "release=" + engineTestDeployReleaseID})
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

// supersededManifest is what a manual rollback leaves on the release it left.
func supersededManifest(f *transport.Fake, releaseID, predecessor string) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	manifest, err := release.NewManifest(releaseID, release.KindApplication, at)
	if err != nil {
		panic(err)
	}
	for i, state := range []release.State{release.StateVerified, release.StateServing, release.StateSuperseded} {
		predecessorFor := ""
		if state == release.StateServing {
			predecessorFor = predecessor
		}
		if err := manifest.Transition(state, at.Add(time.Duration(i+1)*time.Second), predecessorFor); err != nil {
			panic(err)
		}
	}
	command, input, err := release.ManifestWrite(testConfig().NamesFor("production"), manifest)
	if err != nil {
		panic(err)
	}
	if result, runErr := f.RunInput(context.Background(), command, input); runErr != nil || result.ExitCode != 0 {
		panic("seed superseded application manifest")
	}
	f.Commands = nil
	f.Inputs = nil
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

// The activation checkpoint is cleared last, after the journal records the
// activation, so a clear that fails leaves a checkpoint behind on a release
// that is live and fully activated. Refusing on that would be permanent:
// nothing on the resume path retries the clear, so every `ob resume` would
// repeat it, and the only escapes are rolling a healthy release back or
// deploying over it. Finalize finishes the interrupted step instead.
func TestFinalizeCompletesAClearThatDidNotLand(t *testing.T) {
	f := activatedFake(t)
	checkpoint, err := release.NewActivationCheckpoint(
		engineTestDeployReleaseID, engineTestPreviousReleaseID,
		release.ActivationPredecessorSuperseded, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
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

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("a checkpoint left by a failed clear must not refuse a finalize: %v", err)
	}
	if _, err := release.ReadActivationCheckpoint(context.Background(), f, e.Names()); err == nil {
		t.Fatal("finalize left the stale checkpoint behind, so the next resume repeats the problem")
	}
}

// A refused finalize changed nothing it claimed to, so it must not consume the
// checkpoint on the way out: recovery and retention both read that file as
// durable evidence.
func TestARefusedFinalizeLeavesTheCheckpointIntact(t *testing.T) {
	f := activatedFake(t)
	checkpoint, err := release.NewActivationCheckpoint(
		engineTestDeployReleaseID, engineTestPreviousReleaseID,
		release.ActivationPredecessorSuperseded, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
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
	// A workload is no longer running, so the live check refuses.
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps --filter label=ob.app=") {
			return transport.Result{Stdout: "NEW1|web|" + engineTestDeployReleaseID + "|Up (healthy)\n"}, true
		}
		return base(cmd)
	}

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	var refused *FinalizeRefusedError
	if err := e.Resume(context.Background()); !errors.As(err, &refused) {
		t.Fatalf("resume error = %v, want a typed finalize_refused", err)
	}
	if _, err := release.ReadActivationCheckpoint(context.Background(), f, e.Names()); err != nil {
		t.Fatalf("a refused finalize consumed the checkpoint it refused on: %v", err)
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
	// A skip is its own event: recorded as evidence, never marked done, so a
	// later finalize retries the cleanup once whatever blocked it is fixed.
	if !strings.Contains(seq, `"sub_step":"finalize:retention","event":"skip","detail":"`+retentionSkipped+`"`) {
		t.Fatalf("the journal must record what cleanup declined to do:\n%s", seq)
	}
	if strings.Contains(seq, `"sub_step":"finalize:retention","event":"result"`) {
		t.Fatalf("a declined step must not be recorded as a completed result:\n%s", seq)
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

// A manual `ob rollback` journals under the release it restores, so it never
// settles the interrupted deploy's own journal and that deploy stays the newest
// actionable one. Its manifest is superseded by then, and replaying it would
// start containers of a release the host has already moved past — so the
// refusal has to land before the first role rolls, not at activation.
func TestResumeRefusesASupersededReleaseBeforeAnyEffect(t *testing.T) {
	f := happyFake()
	supersededManifest(f, engineTestDeployReleaseID, engineTestPreviousReleaseID)
	jr := journalLines(
		journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "deploy", Event: "start", Detail: "prev=" + engineTestPreviousReleaseID, TS: "2026-07-03T00:00:00Z"},
		journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "pre-release", SubStep: journal.EffectBaselineSubStep, Event: "result", Status: "ok", RollbackSafe: true},
		journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "transfer", Event: "result", Status: "ok"},
		journal.Record{DeployID: engineTestDeployReleaseID, Epoch: 2, Phase: "release", Role: "web", Event: "result", Status: "ok"},
	)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal"):
			return transport.Result{Stdout: journalMarkerLine + engineTestDeployReleaseID + ".jsonl\n" + jr}, true
		case strings.Contains(cmd, "test -d"):
			return transport.Result{ExitCode: 0}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Resume(context.Background())
	var refused *ActivationRefusedError
	if !errors.As(err, &refused) || refused.State != release.StateSuperseded {
		t.Fatalf("resume error = %v, want activation_refused for a superseded release", err)
	}
	seq := strings.Join(f.Commands, "\n")
	for _, forbidden := range []string{"--force-recreate --timeout 30 worker", "--scale web=", "run --rm --no-deps", "ln -sfn"} {
		if strings.Contains(seq, forbidden) {
			t.Fatalf("a superseded release must be refused before any effect (%s):\n%s", forbidden, seq)
		}
	}
}

// A manifest that still claims the previous outcome is the state the outcome
// field exists to prevent, so the failure to stamp it must reach the operator —
// not be replaced by the step failure that triggered it.
func TestFailedOutcomeReportsAFailedManifestWrite(t *testing.T) {
	f := activatedFake(t)
	// Not already failed: recordOutcome short-circuits when the manifest
	// carries the outcome it is about to write, so the default seed would make
	// the write this test needs to fail never happen at all.
	seedServingApplicationManifest(f, engineTestDeployReleaseID, engineTestPreviousReleaseID, release.OutcomeSucceeded)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		// The post-deploy hook fails...
		if strings.Contains(cmd, "notify-release") {
			return transport.Result{ExitCode: 1, Stderr: "hook exploded"}, true
		}
		// ...and so does the manifest write that records the outcome.
		if strings.Contains(cmd, "manifest.json.tmp") {
			return transport.Result{ExitCode: 1, Stderr: "disk full"}, true
		}
		return base(cmd)
	}

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Resume(context.Background())
	if err == nil {
		t.Fatal("resume reported success with an unstamped manifest")
	}
	var typed *PostActivationFailedError
	if !errors.As(err, &typed) {
		t.Errorf("error = %v, want it to carry PostActivationFailedError", err)
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("the manifest write failure was dropped: %v", err)
	}
}

// The success write is an exit from the post-activation steps too: every step
// has already run, so an interrupt there must not report "cancelled, nothing
// was changed" for a release that is live and fully finalized but one write.
func TestASuccessfulFinalizeStillTypesItsOutcomeWriteFailure(t *testing.T) {
	f := activatedFake(t)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "manifest.json.tmp") {
			return transport.Result{ExitCode: 1, Stderr: "transport lost"}, true
		}
		return base(cmd)
	}

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Resume(context.Background())
	var typed *PostActivationFailedError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want a typed post_activation_failed", err)
	}
	if typed.Code() != "post_activation_failed" {
		t.Errorf("code = %q", typed.Code())
	}
}

// The checkpoint clear runs after the release is live and journalled, so a
// failure there is not "nothing was changed". Untyped, an interrupt at that
// moment ships as outcome cancelled and exit 2 for a deploy whose new
// generation is serving.
func TestAFailedCheckpointClearIsTypedAsPostActivation(t *testing.T) {
	f := activatedFake(t)
	checkpoint, err := release.NewActivationCheckpoint(
		engineTestDeployReleaseID, engineTestPreviousReleaseID,
		release.ActivationPredecessorSuperseded, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
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
	// Installed after seeding, and excluding the checkpoint WRITE, whose trap
	// also contains `rm -f "$tmp"`. e.mutate prefixes the command with fence
	// checks, so this cannot anchor on a prefix either.
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "rm -f") && strings.Contains(cmd, "activation.json") && !strings.Contains(cmd, "mktemp") {
			return transport.Result{ExitCode: 1, Stderr: "transport lost"}, true
		}
		return base(cmd)
	}

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	var typed *PostActivationFailedError
	if err := e.Resume(context.Background()); !errors.As(err, &typed) {
		t.Fatalf("error = %v, want a typed post_activation_failed", err)
	}
}

// A failed activation-journal append leaves the release serving with only the
// intent recorded. Refusing on the journal alone makes that permanent: nothing
// rewrites the record, every resume repeats the refusal, the outcome stays
// pending on a healthy release, and the only offered escape rolls it back. A
// checkpoint at the sequence's last phase is the other durable proof that
// activation completed.
func TestFinalizeAcceptsACompletedCheckpointWhenTheJournalAppendFailed(t *testing.T) {
	f := activatedFakeWithoutActivationResult(t)
	checkpoint, err := release.NewActivationCheckpoint(
		engineTestDeployReleaseID, engineTestPreviousReleaseID,
		release.ActivationPredecessorSuperseded, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
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

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("finalize refused a release its checkpoint proves was activated: %v", err)
	}
	if _, err := release.ReadActivationCheckpoint(context.Background(), f, e.Names()); err == nil {
		t.Error("the completed checkpoint was left behind")
	}
}

// Without that proof the refusal still stands: a checkpoint at an earlier
// phase means activation genuinely did not finish.
func TestFinalizeStillRefusesWithAnIncompleteCheckpointAndNoJournalRecord(t *testing.T) {
	f := activatedFakeWithoutActivationResult(t)
	checkpoint, err := release.NewActivationCheckpoint(
		engineTestDeployReleaseID, engineTestPreviousReleaseID,
		release.ActivationPrepared, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
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

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	var refused *FinalizeRefusedError
	if err := e.Resume(context.Background()); !errors.As(err, &refused) {
		t.Fatalf("error = %v, want a typed finalize_refused", err)
	}
}

// The checkpoint is the only proof of activation on this path, and finalize
// deletes it. If the record it stood in for is not written first, a later
// failure in the same run leaves neither evidence source, and every future
// resume refuses permanently on a healthy, live release.
func TestFinalizeJournalsTheReconstructedActivationBeforeClearingTheCheckpoint(t *testing.T) {
	f := activatedFakeWithoutActivationResult(t)
	checkpoint, err := release.NewActivationCheckpoint(
		engineTestDeployReleaseID, engineTestPreviousReleaseID,
		release.ActivationPredecessorSuperseded, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
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

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}

	appended, cleared := -1, -1
	for i, cmd := range f.Commands {
		if appended < 0 && strings.Contains(cmd, `"phase":"activation"`) && strings.Contains(cmd, `"event":"result"`) {
			appended = i
		}
		if cleared < 0 && strings.Contains(cmd, "rm -f") && strings.Contains(cmd, "activation.json") && !strings.Contains(cmd, "mktemp") {
			cleared = i
		}
	}
	if appended < 0 {
		t.Fatal("the activation the checkpoint proved was never written to the journal")
	}
	if cleared >= 0 && cleared < appended {
		t.Fatalf("checkpoint cleared at %d, before the activation was journalled at %d", cleared, appended)
	}
}

// Transition(StateServing) sets the outcome to succeeded, so an exit that skips
// failedOutcome leaves the manifest claiming a finished operation for one that
// failed — the state the outcome field exists to prevent.
func TestAFailedCheckpointClearRecordsAFailedOutcome(t *testing.T) {
	f := activatedFake(t)
	checkpoint, err := release.NewActivationCheckpoint(
		engineTestDeployReleaseID, engineTestPreviousReleaseID,
		release.ActivationPredecessorSuperseded, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
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
	seedServingApplicationManifest(f, engineTestDeployReleaseID, engineTestPreviousReleaseID, release.OutcomeSucceeded)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "rm -f") && strings.Contains(cmd, "activation.json") && !strings.Contains(cmd, "mktemp") {
			return transport.Result{ExitCode: 1, Stderr: "transport lost"}, true
		}
		return base(cmd)
	}

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err == nil {
		t.Fatal("resume reported success with an uncleared checkpoint")
	}
	var wroteFailed bool
	for _, input := range f.Inputs {
		if strings.Contains(input, `"operation_outcome"`) && strings.Contains(input, `"failed"`) {
			wroteFailed = true
		}
	}
	if !wroteFailed {
		t.Error("the manifest still claims a finished operation after a failed clear")
	}
}

// A rollback writes a checkpoint carrying the ROLLBACK TARGET's id. Keying the
// stale-checkpoint completion on manifest.ID left that case refusing on every
// resume until someone deployed again — the phase is what says the activation
// finished, not whose release it names.
func TestFinalizeClearsACompletedCheckpointForAnotherRelease(t *testing.T) {
	f := activatedFake(t)
	checkpoint, err := release.NewActivationCheckpoint(
		engineTestPreviousReleaseID, engineTestDeployReleaseID,
		release.ActivationPredecessorSuperseded, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
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

	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("a completed checkpoint from another release still refused: %v", err)
	}
	if _, err := release.ReadActivationCheckpoint(context.Background(), f, e.Names()); err == nil {
		t.Error("the completed checkpoint was left behind")
	}
}

// Journal appends past activation are exits too. The release is already
// serving, so an untyped failure here reports "cancelled, nothing was changed"
// for a live generation and leaves the manifest claiming the operation
// succeeded.
func TestPostActivationJournalAppendFailuresAreTypedAndRecorded(t *testing.T) {
	for name, marker := range map[string]string{
		"verify result":     `"phase":"verify"`,
		"activation result": `"phase":"activation"`,
	} {
		t.Run(name, func(t *testing.T) {
			f := activatedFake(t)
			seedServingApplicationManifest(f, engineTestDeployReleaseID, engineTestPreviousReleaseID, release.OutcomeSucceeded)
			base := f.Dynamic
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, marker) && strings.Contains(cmd, `"event":"result"`) {
					return transport.Result{ExitCode: 1, Stderr: "journal unwritable"}, true
				}
				return base(cmd)
			}

			e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			err := e.Resume(context.Background())
			if err == nil {
				return // this marker is not reached on the resume path
			}
			var typed *PostActivationFailedError
			if !errors.As(err, &typed) {
				t.Errorf("error = %v, want a typed post_activation_failed", err)
			}
		})
	}
}

// Every other post-activation failure is completed by re-running resume; a
// verify failure is not — resume re-enters this function, runs the same Verify
// and fails identically. Publishing a resolving command an agent may execute
// would have it loop instead of stopping to diagnose.
//
// Two halves: the helper carries guidance through, and the verify branch is
// the one that passes a diagnostic. Driving a real verify failure would need a
// project fixture with verifications, which tests the fixture more than the
// rule.
func TestFailedOutcomeCarriesGuidanceThrough(t *testing.T) {
	f := activatedFake(t)
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	manifest, err := release.ReadManifest(context.Background(), f, e.Names(), engineTestDeployReleaseID)
	if err != nil {
		t.Fatal(err)
	}

	err = e.failedOutcomeWithGuidance(context.Background(), &manifest, "ob audit --output json", errors.New("boom"))
	var typed *PostActivationFailedError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want a typed post_activation_failed", err)
	}
	if got := typed.GuidanceCommand(); got != "ob audit --output json" {
		t.Errorf("guidance = %q, want the diagnostic the caller asked for", got)
	}
	// The default is still resume for every other exit.
	err = e.failedOutcome(context.Background(), &manifest, errors.New("boom"))
	if !errors.As(err, &typed) || typed.GuidanceCommand() != "ob resume --output ndjson" {
		t.Errorf("default guidance = %q, want ob resume", typed.GuidanceCommand())
	}
}

func TestTheVerifyBranchPublishesADiagnostic(t *testing.T) {
	body, err := os.ReadFile("finalize.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	index := strings.Index(text, `fmt.Errorf("verify serving release:`)
	if index < 0 {
		t.Fatal("the verify failure branch moved; update this test")
	}
	window := text[max(0, index-300):index]
	if !strings.Contains(window, "failedOutcomeWithGuidance") || !strings.Contains(window, "ob audit --output json") {
		t.Error("the verify failure no longer publishes a diagnostic; ob resume re-runs the check that just failed")
	}
}
