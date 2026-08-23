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

// happyFake scripts an entire single-host deploy: rolling web, recreate
// worker, service postgres healthy, verify green, lock/fence/journal on
// fake defaults (exit 0).
// guardedHealthcheck is what generation emits for every shell-form check: the
// drain guard first, so a rollout can take the container out of rotation before
// it stops it. A rollout probes for this, and a fake that did not answer would
// exercise the unguardable path in every test.
const guardedHealthcheck = `["CMD-SHELL","[ -f /tmp/ob-drain ] \u0026\u0026 exit 1; curl -fsS 'http://127.0.0.1:80/'"]`

func seedStagedApplicationManifest(f *transport.Fake, releaseID string) {
	manifest, err := release.NewManifest(releaseID, release.KindApplication, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		panic(err)
	}
	command, input, err := release.ManifestWrite(testConfig().NamesFor("production"), manifest)
	if err != nil {
		panic(err)
	}
	if result, runErr := f.RunInput(context.Background(), command, input); runErr != nil || result.ExitCode != 0 {
		panic("seed staged application manifest")
	}
	f.Commands = nil
	f.Inputs = nil
}

func happyFake() *transport.Fake {
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "Config.Healthcheck.Test") {
			return transport.Result{Stdout: guardedHealthcheck + "\n"}, true
		}
		// server roll state, derived from history so the loop converges: NEW1
		// appears after a scale, OLD1 disappears once removed, names track renames.
		scaled, oldGone, drained := false, false, false
		scaleCount, recreateCount := 0, 0
		initialScaleCount, initialRecreateCount := 0, 0
		seenExactReleaseQuery := false
		newGone, workerGone := false, false
		name := map[string]string{"OLD1": "web"}
		for _, c := range f.Commands {
			if strings.Contains(c, "docker ps -aq") && strings.Contains(c, "label=ob.app=") && strings.Contains(c, "label=ob.release=") {
				seenExactReleaseQuery = true
			}
			if strings.Contains(c, "--scale web=") {
				scaled = true
				scaleCount++
				if !seenExactReleaseQuery {
					initialScaleCount++
				}
			}
			if strings.Contains(c, "--force-recreate --timeout 30 worker") {
				recreateCount++
				if !seenExactReleaseQuery {
					initialRecreateCount++
				}
			}
			if strings.Contains(c, "docker rm OLD1") {
				oldGone = true
			}
			if strings.Contains(c, "docker rm -f NEW1") {
				newGone = true
			}
			if strings.Contains(c, "docker rm -f W1") {
				workerGone = true
			}
			if strings.Contains(c, "ob-drain") {
				drained = true
			}
			if i := strings.Index(c, "docker rename "); i >= 0 {
				fs := strings.Fields(c[i+len("docker rename "):])
				if len(fs) >= 2 {
					name[fs[0]] = fs[1]
				}
			}
		}
		lastField := func(s string) string {
			fs := strings.Fields(s)
			return fs[len(fs)-1]
		}
		switch {
		case strings.Contains(cmd, "/_host/owner"):
			return transport.Result{Stdout: "sample\n"}, true
		case strings.Contains(cmd, "docker network inspect --format"):
			return transport.Result{Stdout: "abc123|sample|\n"}, true
		case strings.Contains(cmd, "docker version"):
			return transport.Result{Stdout: "27.0.3\n"}, true
		case strings.Contains(cmd, "compose version"):
			return transport.Result{Stdout: "2.29.1\n"}, true
		case strings.Contains(cmd, "df -Pk"):
			return transport.Result{Stdout: "4194304\n"}, true
		case strings.Contains(cmd, "service='postgres'"):
			return transport.Result{Stdout: "PG1\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "PG1"):
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service='web'") && strings.Contains(cmd, "ob.release="):
			if scaled && (!newGone || scaleCount > initialScaleCount) {
				return transport.Result{Stdout: "NEW1\n"}, true
			}
			return transport.Result{Stdout: ""}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service='web'"):
			var ids []string
			if !oldGone {
				ids = append(ids, "OLD1")
			}
			if scaled && (!newGone || scaleCount > initialScaleCount) {
				ids = append(ids, "NEW1")
			}
			return transport.Result{Stdout: strings.Join(ids, "\n") + "\n"}, true
		case strings.Contains(cmd, "{{.Name}}") && (strings.Contains(cmd, "OLD1") || strings.Contains(cmd, "NEW1")):
			n := name[lastField(cmd)]
			if n == "" {
				n = "sample-web-x"
			}
			return transport.Result{Stdout: "/" + n + "\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "NEW1") && strings.Contains(cmd, "Health"):
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "OLD1") && strings.Contains(cmd, "Health"):
			if drained {
				return transport.Result{Stdout: "unhealthy\n"}, true
			}
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "service='worker'") && strings.Contains(cmd, "ob.release="):
			if !workerGone || recreateCount > initialRecreateCount {
				return transport.Result{Stdout: "W1\n"}, true
			}
			return transport.Result{}, true
		case strings.Contains(cmd, "service='worker'"):
			if !workerGone || recreateCount > initialRecreateCount {
				return transport.Result{Stdout: "W1\n"}, true
			}
			return transport.Result{}, true
		case strings.Contains(cmd, "docker ps -aq") && strings.Contains(cmd, "label=ob.app=") && strings.Contains(cmd, "label=ob.release="):
			var ids []string
			if initialScaleCount > 0 && !newGone {
				ids = append(ids, "NEW1")
			}
			if initialRecreateCount > 0 && !workerGone {
				ids = append(ids, "W1")
			}
			return transport.Result{Stdout: strings.Join(ids, "\n")}, true
		case strings.Contains(cmd, "{{.State.Status}}"):
			return transport.Result{Stdout: "running\n"}, true
		case strings.Contains(cmd, "IPAddress"):
			return transport.Result{Stdout: "172.20.0.5 \n"}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: ""}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "20260101-000000-aaa111\n"}, true
		case strings.Contains(cmd, "ob.snapshot.yml"):
			return transport.Result{Stdout: engineProject}, true
		case strings.Contains(cmd, "/journal/"+engineTestPreviousReleaseID+".jsonl"):
			return transport.Result{Stdout: `{"deploy_id":"` + engineTestPreviousReleaseID + `","phase":"activation","event":"result","status":"ok","detail":"release=` + engineTestPreviousReleaseID + `"}` + "\n"}, true
		}
		return transport.Result{}, false
	}
	// Every test starts from the current release-store contract. Seed a serving
	// predecessor without recording setup as an operation under test.
	manifest, err := release.NewManifest(engineTestPreviousReleaseID, release.KindApplication, time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC))
	if err != nil {
		panic(err)
	}
	if err := manifest.Transition(release.StateVerified, time.Date(2025, 12, 31, 23, 59, 1, 0, time.UTC), ""); err != nil {
		panic(err)
	}
	if err := manifest.Transition(release.StateServing, time.Date(2025, 12, 31, 23, 59, 2, 0, time.UTC), ""); err != nil {
		panic(err)
	}
	command, input, err := release.ManifestWrite(testConfig().NamesFor("production"), manifest)
	if err != nil {
		panic(err)
	}
	if result, runErr := f.RunInput(context.Background(), command, input); runErr != nil || result.ExitCode != 0 {
		panic("seed serving manifest")
	}
	f.Commands = nil
	f.Inputs = nil
	return f
}

func TestDeployJournalsAndFencesLifecycle(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	// journal lifecycle in order
	ordered := []string{
		`"event":"start"`,
		`"sub_step":"job:migrate","event":"result","status":"ok"`,
		`"role":"web","event":"result","status":"ok"`,
		`"role":"worker","event":"result","status":"ok"`,
		`"phase":"verify","event":"result","status":"ok"`,
		`"phase":"activation","event":"intent"`,
		`"phase":"activation","event":"result","status":"ok"`,
		`"event":"finish","status":"ok"`,
	}
	last := -1
	for _, want := range ordered {
		i := strings.Index(seq, want)
		if i < 0 {
			t.Fatalf("journal record missing %s:\n%s", want, seq)
		}
		if i < last {
			t.Fatalf("journal record out of order %s", want)
		}
		last = i
	}
	// every mutation is fence-guarded
	for _, mut := range []string{"--scale web=2", "touch /tmp/ob-drain", "docker stop -t 30 OLD1", "--force-recreate --timeout 30 worker", "ln -sfn"} {
		for _, c := range f.Commands {
			if strings.Contains(c, mut) && !strings.Contains(c, "ob-fenced") {
				t.Fatalf("mutation not fence-guarded: %s", c)
			}
		}
	}
	// lock released at the end
	if !strings.Contains(seq, "rm -f '/var/lib/ob/sample/lock'") {
		t.Fatal("lock never released")
	}
}

func TestDeployStopsWhenRequiredJournalEvidenceCannotBeWritten(t *testing.T) {
	for _, tt := range []struct {
		name      string
		record    string
		forbidden string
		want      string
	}{
		{name: "transfer result", record: `"phase":"transfer","event":"result","status":"ok"`, forbidden: "ONEBOX_RESULT_FILE", want: "journal transfer result"},
		{name: "release intent", record: `"phase":"release","role":"web","event":"intent"`, forbidden: "--scale web=", want: "journal release web intent"},
		{name: "release result", record: `"phase":"release","role":"web","event":"result","status":"ok"`, forbidden: "--force-recreate --timeout 30 worker", want: "journal release web result"},
		{name: "verify result", record: `"phase":"verify","event":"result","status":"ok"`, forbidden: "ln -sfn 'releases/20260101-000000-aaa111'", want: "journal verify result"},
		{name: "deploy finish", record: `"phase":"deploy","event":"finish","status":"ok"`, want: "journal deploy finish"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := happyFake()
			base := f.Dynamic
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, tt.record) {
					return transport.Result{ExitCode: 74, Stderr: "journal is read-only"}, true
				}
				return base(cmd)
			}
			e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
			err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("deploy error = %v, want %q", err, tt.want)
			}
			seq := strings.Join(f.Commands, "\n")
			if tt.forbidden != "" && strings.Contains(seq, tt.forbidden) {
				t.Fatalf("deploy continued after %s journal failure:\n%s", tt.name, seq)
			}
			if tt.name == "deploy finish" && !strings.Contains(seq, "ln -sfn 'releases/20260101-000000-aaa111'") {
				t.Fatalf("finish failure must report the already-completed activation:\n%s", seq)
			}
		})
	}
}

func TestDeployEmitsVerificationAndActivationProgress(t *testing.T) {
	f := happyFake()
	var transitions []string
	e := New(testConfig(), testProject(t), f, Options{
		Out: &bytes.Buffer{}, Sleep: noSleep,
		Progress: func(phase, status, message string) {
			transitions = append(transitions, phase+":"+status+":"+message)
		},
	})
	if err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	want := []string{
		"verification:started:",
		"verification:succeeded:",
		"activation:started:",
		"activation:succeeded:",
		"cleanup:started:",
		"cleanup:succeeded:",
	}
	if strings.Join(transitions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("transitions = %#v, want %#v", transitions, want)
	}
}

func TestDeployPhaseOrder(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v\ncommands:\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	phases := []string{
		"docker version",                                  // preflight
		"run --rm --no-deps",                              // pre-release: the migrate job
		"--scale web=2 web",                               // release: web rolls first (order)
		"--force-recreate --timeout 30 worker",            // then worker recreates
		"curl -fsS -m 5 'http://172.20.0.5:7500/healthz'", // verify
		"ln -sfn 'releases/20260101-000000-aaa111'",       // finalize: activate
	}
	last := -1
	for _, p := range phases {
		i := strings.Index(seq, p)
		if i < 0 {
			t.Fatalf("phase step missing %q:\n%s", p, seq)
		}
		if i < last {
			t.Fatalf("phase %q out of order:\n%s", p, seq)
		}
		last = i
	}
	if len(f.Uploads) != 1 || !strings.Contains(f.Uploads[0], "/var/lib/ob/sample/releases/20260101-000000-aaa111") {
		t.Fatalf("transfer missing: %v", f.Uploads)
	}
}

func TestVerifyFailureBlocksActivation(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "curl -fsS") {
			return transport.Result{ExitCode: 22, Stderr: "404"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "20260101-000000-aaa111", t.TempDir()); err == nil {
		t.Fatal("verify failure must fail the deploy")
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "ln -sfn") {
		t.Fatal("failed verify must not activate the release")
	}
}

func TestRollbackReplaysPreviousRelease(t *testing.T) {
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
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "releases/20260101-000000-aaa111/compose.yaml") {
		t.Fatalf("rollback must target previous release dir:\n%s", seq)
	}
	if !strings.Contains(seq, "ln -sfn 'releases/20260101-000000-aaa111'") {
		t.Fatalf("rollback must re-activate previous:\n%s", seq)
	}
}
