package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/transport"
)

// happyFake scripts an entire single-host deploy: rolling web, recreate
// worker, accessory postgres healthy, verify green, lock/fence/journal on
// fake defaults (exit 0).
func happyFake() *transport.Fake {
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker version"):
			return transport.Result{Stdout: "27.0.3\n"}, true
		case strings.Contains(cmd, "compose version"):
			return transport.Result{Stdout: "2.29.1\n"}, true
		case strings.Contains(cmd, "df -Pk"):
			return transport.Result{Stdout: "4194304\n"}, true
		case strings.Contains(cmd, "service=postgres"):
			return transport.Result{Stdout: "PG1\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "PG1"):
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service=server") && strings.Contains(cmd, "yeet.release="):
			for _, c := range f.Commands {
				if strings.Contains(c, "--scale server=2") {
					return transport.Result{Stdout: "NEW1\n"}, true
				}
			}
			return transport.Result{Stdout: ""}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service=server"):
			return transport.Result{Stdout: "OLD1\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "NEW1") && strings.Contains(cmd, "Health"):
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "OLD1") && strings.Contains(cmd, "Health"):
			for _, c := range f.Commands {
				if strings.Contains(c, "yeet-drain") {
					return transport.Result{Stdout: "unhealthy\n"}, true
				}
			}
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "service=worker") && strings.Contains(cmd, "yeet.release="):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(cmd, "service=worker"):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(cmd, "{{.State.Status}}"):
			return transport.Result{Stdout: "running\n"}, true
		case strings.Contains(cmd, "IPAddress"):
			return transport.Result{Stdout: "172.20.0.5 \n"}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: ""}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "R1\n"}, true
		}
		return transport.Result{}, false
	}
	return f
}

func TestDeployJournalsAndFencesLifecycle(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "R1", t.TempDir()); err != nil {
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
	for _, mut := range []string{"--scale server=2", "touch /tmp/yeet-drain", "docker stop -t 30 OLD1", "--force-recreate worker", "ln -sfn"} {
		for _, c := range f.Commands {
			if strings.Contains(c, mut) && !strings.Contains(c, "yeet-fenced") {
				t.Fatalf("mutation not fence-guarded: %s", c)
			}
		}
	}
	// lock released at the end
	if !strings.Contains(seq, "rm -f '/var/lib/yeet/monk/lock'") {
		t.Fatal("lock never released")
	}
}

func TestDeployPhaseOrder(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Deploy(context.Background(), "R1", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v\ncommands:\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	phases := []string{
		"docker version",                                // preflight
		"run --rm --no-deps migrate",                    // pre-release hook (after upload)
		"--scale server=2 server",                       // release: web rolls first (order)
		"--force-recreate worker",                       // then worker recreates
		"curl -fsS -m 5 http://172.20.0.5:7500/healthz", // verify
		"ln -sfn 'releases/R1'",                         // finalize: activate
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
	if len(f.Uploads) != 1 || !strings.Contains(f.Uploads[0], "/var/lib/yeet/monk/releases/R1") {
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
	if err := e.Deploy(context.Background(), "R1", t.TempDir()); err == nil {
		t.Fatal("verify failure must fail the deploy")
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "ln -sfn") {
		t.Fatal("failed verify must not activate the release")
	}
}

func TestRollbackReplaysPreviousRelease(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/R2\n"}, true
		}
		if strings.Contains(cmd, "ls -1") {
			return transport.Result{Stdout: "R1\nR2\n"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "releases/R1/compose.yaml") {
		t.Fatalf("rollback must target previous release dir:\n%s", seq)
	}
	if !strings.Contains(seq, "ln -sfn 'releases/R1'") {
		t.Fatalf("rollback must re-activate previous:\n%s", seq)
	}
}
