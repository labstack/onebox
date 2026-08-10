package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
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
				if strings.Contains(c, "releases/"+engineTestPreviousReleaseID+"/compose.yaml") {
					return transport.Result{ExitCode: 0}, true
				}
			}
			return transport.Result{ExitCode: 22, Stderr: "500"}, true
		case strings.Contains(cmd, "job-migrate-result"):
			return transport.Result{Stdout: migrateResult}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: engineTestPreviousReleaseID + "\n" + engineTestDeployReleaseID + "\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestGateOpenAutoRollsBack(t *testing.T) {
	f := gateFake("changed=false\n")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil {
		t.Fatal("failed verify must still return an error")
	}
	if !strings.Contains(err.Error(), "auto-rolled back") {
		t.Fatalf("gate open must auto-rollback: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "releases/"+engineTestPreviousReleaseID+"/compose.yaml") {
		t.Fatalf("auto-rollback must replay previous release:\n%s", seq)
	}
	if strings.Contains(seq, "ln -sfn 'releases/"+engineTestDeployReleaseID+"'") {
		t.Fatal("failed release must not be activated")
	}
}

func TestAutoRollbackUsesPreviousReleaseSnapshot(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
		case strings.Contains(cmd, "/releases/"+engineTestPreviousReleaseID+"/ob.snapshot.yml"):
			return transport.Result{Stdout: oldSnapshot}, true
		case strings.Contains(cmd, "service='worker'") && strings.Contains(cmd, "ob.release='"+engineTestPreviousReleaseID+"'"):
			return transport.Result{}, true
		case strings.Contains(cmd, "curl -fsS"):
			return transport.Result{ExitCode: 22, Stderr: "500"}, true
		}
		return base(cmd)
	}
	cfg := testConfig()
	cfg.Workloads = map[string]app.Workload{"web": cfg.Workloads["web"]}
	cfg.Deployment.Order = []string{"web"}
	cfg.Services = nil
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "auto-rolled back") {
		t.Fatalf("deploy error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "releases/"+engineTestPreviousReleaseID+"/compose.yaml") || !strings.Contains(seq, "--force-recreate --timeout 30 worker") {
		t.Fatalf("auto-rollback did not use the previous release snapshot:\n%s", seq)
	}
	if strings.Count(seq, "--scale web=") != 1 {
		t.Fatalf("auto-rollback replayed the edited web choreography:\n%s", seq)
	}
}

func TestAutoRollbackStopsWhenIntentCannotBeJournaled(t *testing.T) {
	f := gateFake("changed=false\n")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, `"phase":"auto-rollback","event":"intent"`) {
			return transport.Result{ExitCode: 74, Stderr: "journal is read-only"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "journal auto-rollback intent") {
		t.Fatalf("deploy error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "docker stop -t 10 NEW1") || strings.Contains(seq, "releases/"+engineTestPreviousReleaseID+"/compose.yaml' pull") {
		t.Fatalf("auto-rollback mutated after its intent write failed:\n%s", seq)
	}
}

func TestRemoveNewcomersRejectsRemoteRemovalFailure(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "service='web'") && strings.Contains(cmd, "ob.release='R1'"):
			return transport.Result{Stdout: "NEW1\n"}, true
		case strings.Contains(cmd, "docker stop -t 10 NEW1"):
			return transport.Result{ExitCode: 55, Stderr: "daemon refused"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.removeNewcomers(context.Background(), "R1")
	if err == nil || !strings.Contains(err.Error(), "remove newcomer NEW1 failed (exit 55): daemon refused") {
		t.Fatalf("remove newcomers error = %v", err)
	}
}

// A job with no same-named hook auto-runs `compose run --rm --no-deps <job>` —
// no `migrate` hook needed in ob.yml.
func TestJobAutoRunsWithoutHook(t *testing.T) {
	cfg := testConfig()
	cfg.Hooks = map[string]app.Command{} // drop the migrate hook; migrate stays a job
	f := happyFake()
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "run --rm --no-deps -e OB_RESULT_FILE=/run/onebox/job-result") {
		t.Fatalf("a job without a hook must auto-run compose run:\n%s", seq)
	}
	// gate protocol still applies to the auto-run job.
	if !strings.Contains(seq, "OB_RESULT_FILE=") {
		t.Fatalf("auto-run job must run under the gate protocol:\n%s", seq)
	}
}

func TestDeployRunsOnlyAutomaticJobsInTheirDeclaredPhase(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["cleanup"] = app.Workload{Role: app.RoleJob, When: "post_release", DataEffect: "none"}
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
	}
	cfg.Hooks["cleanup"] = app.Command{Run: "echo POST_RELEASE_JOB_MARKER"}
	cfg.Hooks["nightly"] = app.Command{Run: "echo MANUAL_JOB_MARKER"}
	f := happyFake()
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "MANUAL_JOB_MARKER") {
		t.Fatalf("manual job executed during deploy:\n%s", seq)
	}
	releaseAt := strings.Index(seq, "--force-recreate --timeout 30 worker")
	postJobAt := strings.Index(seq, "POST_RELEASE_JOB_MARKER")
	if releaseAt < 0 || postJobAt < 0 || postJobAt < releaseAt {
		t.Fatalf("post-release job did not run after workload release:\n%s", seq)
	}
	if !strings.Contains(seq, `"phase":"post-release","sub_step":"job:cleanup"`) {
		t.Fatalf("post-release job result was not journaled in its declared phase:\n%s", seq)
	}
}

func TestDeployPersistsSafeEffectBaseline(t *testing.T) {
	cfg := testConfig()
	cfg.Hooks = map[string]app.Command{}
	f := happyFake()
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, `"sub_step":"`+journal.EffectBaselineSubStep+`"`) {
		t.Fatalf("deploy must persist a rollback-safe baseline before effects:\n%s", seq)
	}
}

func TestJobDoesNotRunWhenIntentCannotBeJournaled(t *testing.T) {
	f := happyFake()
	f.Err = func(cmd string) error {
		if strings.Contains(cmd, `"sub_step":"job:migrate"`) && strings.Contains(cmd, `"event":"intent"`) {
			return errors.New("journal unavailable")
		}
		return nil
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	jw := &journal.Writer{T: f, Names: e.Names(), DeployID: "R1", Epoch: 1}
	err := e.runJobs(context.Background(), jw, nil, "/remote", "/remote/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "journal unavailable") {
		t.Fatalf("intent journal failure must stop the job: %v", err)
	}
	if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "OB_RESULT_FILE") {
		t.Fatalf("job ran without a durable intent:\n%s", seq)
	}
}

func TestLifecycleHookDoesNotRunWhenIntentCannotBeJournaled(t *testing.T) {
	f := happyFake()
	f.Err = func(cmd string) error {
		if strings.Contains(cmd, `"sub_step":"hook:pre_release"`) && strings.Contains(cmd, `"event":"intent"`) {
			return errors.New("journal unavailable")
		}
		return nil
	}
	cfg := testConfig()
	cfg.Hooks["pre_release"] = app.Command{Run: "echo SHOULD_NOT_RUN"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	jw := &journal.Writer{T: f, Names: e.Names(), DeployID: "R1", Epoch: 1}
	err := e.runRollbackEffectHook(context.Background(), jw, nil, "pre_release", "/remote", "/remote/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "journal unavailable") {
		t.Fatalf("intent journal failure must stop the hook: %v", err)
	}
	if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "SHOULD_NOT_RUN") {
		t.Fatalf("hook ran without a durable intent:\n%s", seq)
	}
}

func TestGateClosedHaltsAndPages(t *testing.T) {
	f := gateFake("") // migrate wrote nothing — fail safe
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("closed gate must halt-and-page: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "releases/"+engineTestPreviousReleaseID+"/compose.yaml") {
		t.Fatalf("closed gate must NOT auto-rollback:\n%s", seq)
	}
}

func assertRollbackUnavailableMessage(t *testing.T, got string) {
	t.Helper()
	for _, want := range []string{
		"automatic rollback is unavailable after this step",
		"if a later step fails, halt, fix-forward, then run `ob resume`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rollback consequence missing %q from message:\n%s", want, got)
		}
	}
	for _, old := range []string{"gate closed", "gate stays closed"} {
		if strings.Contains(got, old) {
			t.Fatalf("ambiguous phrase %q remains in message:\n%s", old, got)
		}
	}
}

func TestUnknownJobMessagesExplainRollbackConsequence(t *testing.T) {
	t.Run("no result declaration", func(t *testing.T) {
		f := happyFake()
		e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
		safe, detail, err := e.runOneJob(context.Background(), "migrate", "/remote", "/remote/compose.yaml")
		if err != nil {
			t.Fatalf("run job: %v", err)
		}
		if safe {
			t.Fatal("a job without a result declaration must remain rollback-unsafe")
		}
		assertRollbackUnavailableMessage(t, detail)
	})

	t.Run("local hook", func(t *testing.T) {
		cfg := testConfig()
		cfg.Hooks["migrate"] = app.Command{Run: "true", Local: true}
		e := New(cfg, testProject(t), happyFake(), Options{
			Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: t.TempDir(),
		})
		safe, detail, err := e.runOneJob(context.Background(), "migrate", "/remote", "/remote/compose.yaml")
		if err != nil {
			t.Fatalf("run local job: %v", err)
		}
		if safe {
			t.Fatal("a local hook without a data-effect declaration must remain rollback-unsafe")
		}
		assertRollbackUnavailableMessage(t, detail)
	})
}

func TestRecoveredUnknownJobMessageExplainsRollbackConsequence(t *testing.T) {
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), happyFake(), Options{Out: &out, Sleep: noSleep})
	if err := e.runJobs(context.Background(), nil, map[string]bool{"job:migrate": true}, "/remote", "/remote/compose.yaml"); err != nil {
		t.Fatalf("recover completed job: %v", err)
	}
	assertRollbackUnavailableMessage(t, out.String())
}

func TestFailedDeployRollbackDebtSurvivesNextDeploy(t *testing.T) {
	f := gateFake("changed=false\n") // R2's retry is locally safe
	failed := journalLines(
		journal.Record{DeployID: "R1", Epoch: 1, Phase: "deploy", Event: "start", Detail: "prev=R0"},
		journal.Record{DeployID: "R1", Epoch: 1, Phase: "pre-release", SubStep: journal.EffectBaselineSubStep, Event: "result", Status: "ok", RollbackSafe: true},
		journal.Record{DeployID: "R1", Epoch: 1, Phase: "pre-release", SubStep: "job:migrate", Event: "intent"},
		journal.Record{DeployID: "R1", Epoch: 1, Phase: "pre-release", SubStep: "job:migrate", Event: "result", Status: "ok", Detail: "changed=unknown"},
		journal.Record{DeployID: "R1", Epoch: 1, Phase: "deploy", Event: "finish", Status: "fail"},
	)
	maintenance := journalLines(
		journal.Record{DeployID: "R1-service", Epoch: 1, Phase: "service-apply", Event: "start"},
		journal.Record{DeployID: "R1-service", Epoch: 1, Phase: "service-apply", Event: "finish", Status: "ok"},
	)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "for f in") && strings.Contains(cmd, "/var/lib/ob/sample/journal") {
			return transport.Result{Stdout: journalMarkerLine + "R1.jsonl\n" + failed +
				journalMarkerLine + "R1-service.jsonl\n" + maintenance}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("a safe retry must not erase R1's uncovered effect: %v", err)
	}
	if seq := strings.Join(f.Commands, "\n"); strings.Contains(seq, "releases/"+engineTestPreviousReleaseID+"/compose.yaml") {
		t.Fatalf("rollback debt must prevent R2 from auto-rolling back to R0:\n%s", seq)
	}
}

func TestExpandOnlyPromiseOverridesClosedGate(t *testing.T) {
	f := gateFake("") // silent migrate
	cfg := testConfig()
	cfg.Deployment.MigrationPolicy = "expand-only"
	cfg.Workloads["migrate"] = app.Workload{Role: app.RoleJob, When: "pre_release", DataEffect: "migration"}
	e := New(cfg, testProject(t), f, Options{
		Out: &bytes.Buffer{}, Sleep: noSleep,
		ApprovalDigest: "sha256:approved", ApprovalClass: "strong", AllowUnknownMigration: true,
	})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "auto-rolled back") {
		t.Fatalf("expand-only must permit auto-rollback: %v", err)
	}
}

func TestDataEffectNoneOpensGateWithoutResultFile(t *testing.T) {
	f := gateFake("")
	cfg := testConfig()
	cfg.Workloads["migrate"] = app.Workload{Role: app.RoleJob, When: "pre_release", DataEffect: "none"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "auto-rolled back") {
		t.Fatalf("data_effect=none must keep application rollback safe: %v", err)
	}
}

func TestExpandOnlyDoesNotCoverUnknownJob(t *testing.T) {
	f := gateFake("")
	cfg := testConfig()
	cfg.Deployment.MigrationPolicy = "expand-only"
	cfg.Workloads["migrate"] = app.Workload{Role: app.RoleJob, When: "pre_release", DataEffect: "unknown"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("expand-only must not cover an unknown data effect: %v", err)
	}
}

func TestExpandOnlyDoesNotCoverLifecycleHook(t *testing.T) {
	f := gateFake("")
	cfg := testConfig()
	cfg.Deployment.MigrationPolicy = "expand-only"
	cfg.Workloads["migrate"] = app.Workload{Role: app.RoleJob, When: "pre_release", DataEffect: "migration"}
	cfg.Hooks["pre_release"] = app.Command{Run: "true"}
	e := New(cfg, testProject(t), f, Options{
		Out: &bytes.Buffer{}, Sleep: noSleep,
		ApprovalDigest: "sha256:approved", ApprovalClass: "strong", AllowUnknownMigration: true,
	})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "HALT-AND-PAGE") {
		t.Fatalf("expand-only must not cover an untyped lifecycle hook: %v", err)
	}
	if seq := strings.Join(f.Commands, "\n"); !strings.Contains(seq, `"sub_step":"hook:pre_release"`) {
		t.Fatalf("lifecycle effect must be persisted for abort recovery:\n%s", seq)
	}
}

func TestNoRollbackFlagAlwaysHalts(t *testing.T) {
	f := gateFake("changed=false\n")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, NoRollback: true})
	err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--no-rollback") {
		t.Fatalf("--no-rollback must halt even with open gate: %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "releases/"+engineTestPreviousReleaseID+"/compose.yaml") {
		t.Fatal("--no-rollback must not roll back")
	}
}

func TestMigrateComposeJobGetsPrivateWritableBoundResultFile(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	found := false
	for _, c := range f.Commands {
		const (
			resultDir  = "/var/lib/ob/sample/releases/" + engineTestDeployReleaseID + "/.job-migrate-result"
			resultFile = resultDir + "/result"
		)
		privateDir := strings.Index(c, "install -d -m 700 '"+resultDir+"'")
		writableFile := strings.Index(c, "install -m 666 /dev/null '"+resultFile+"'")
		mount := strings.Index(c, "-v '"+resultFile+":/run/onebox/job-result:rw'")
		sealedFile := strings.Index(c, "chmod 600 '"+resultFile+"'")
		if strings.Contains(c, "rm -rf '"+resultDir+"'") &&
			strings.Contains(c, "run --rm --no-deps -e OB_RESULT_FILE=/run/onebox/job-result") &&
			privateDir >= 0 && privateDir < writableFile && writableFile < mount && mount < sealedFile {
			found = true
		}
	}
	if !found {
		t.Fatalf("migrate container must receive a privately staged, writable, subsequently sealed result file:\n%s", strings.Join(f.Commands, "\n"))
	}
}
