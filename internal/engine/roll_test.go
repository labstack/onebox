package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/transport"
)

// rollFake scripts docker for a happy roll: the newcomer (label query for
// yeet.release) appears once `up --scale` ran; NEW is healthy immediately;
// OLD turns unhealthy once the drain poison lands.
func rollFake() *transport.Fake {
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "yeet.release=") {
			for _, c := range f.Commands {
				if strings.Contains(c, "--scale server=2") {
					return transport.Result{Stdout: "NEW1\n"}, true
				}
			}
			return transport.Result{Stdout: ""}, true
		}
		if strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service=server") {
			return transport.Result{Stdout: "OLD1\n"}, true
		}
		if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "NEW1") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "OLD1") {
			for _, c := range f.Commands {
				if strings.Contains(c, "touch /tmp/yeet-drain") {
					return transport.Result{Stdout: "unhealthy\n"}, true
				}
			}
			return transport.Result{Stdout: "healthy\n"}, true
		}
		return transport.Result{}, false
	}
	return f
}

// The newcomer is renamed to <service>-<release> so `docker ps` is readable
// (compose's --scale index is cosmetic; yeet tracks by label).
func TestRollRoleTagsNewcomerWithRelease(t *testing.T) {
	f := rollFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/yeet/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker rename NEW1 server-R1") {
		t.Fatalf("newcomer must be renamed to <service>-<release> (no app prefix):\n%s", seq)
	}
}

func TestRollRoleResumeAdoptsExistingNewcomer(t *testing.T) {
	f := rollFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		// newcomer already exists BEFORE any up --scale (resume scenario)
		if strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "yeet.release=") {
			return transport.Result{Stdout: "NEW1\n"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/yeet/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("resume roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "--scale") || strings.Contains(seq, "pull --quiet") {
		t.Fatalf("resume must not re-scale or re-pull:\n%s", seq)
	}
	if !strings.Contains(seq, "touch /tmp/yeet-drain") || !strings.Contains(seq, "docker stop -t 30 OLD1") {
		t.Fatalf("resume must continue drain+stop of old:\n%s", seq)
	}
}

func noSleep(time.Duration) {}

func TestRollRoleCommandSequence(t *testing.T) {
	f := rollFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/yeet/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	ordered := []string{
		"docker compose -p monk -f '/var/lib/yeet/monk/releases/R1/compose.yaml' pull --quiet server",
		"up -d --no-deps --no-recreate --scale server=2 server",
		"docker exec OLD1 touch /tmp/yeet-drain",
		"docker stop -t 30 OLD1",
		"docker rm OLD1",
	}
	last := -1
	for _, want := range ordered {
		i := strings.Index(seq, want)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", want, seq)
		}
		if i < last {
			t.Fatalf("%q out of order in:\n%s", want, seq)
		}
		last = i
	}
	// drain MUST precede stop: SIGTERM never races the proxy (rev 5)
	if strings.Index(seq, "yeet-drain") > strings.Index(seq, "docker stop") {
		t.Fatal("drain must happen before stop")
	}
}

func TestRollRoleAbortsOnUnhealthyNew(t *testing.T) {
	f := rollFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "NEW1") {
			return transport.Result{Stdout: "starting\n"}, true
		}
		return base(cmd)
	}
	cfg := testConfig()
	cfg.Roles["web"] = withinMillis(cfg.Roles["web"], 50)
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.RollRole(context.Background(), "web", "F")
	if err == nil {
		t.Fatal("expected join failure")
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker rm -f NEW1") {
		t.Fatalf("failed join must remove the new container:\n%s", seq)
	}
	if strings.Contains(seq, "docker stop -t 30 OLD1") {
		t.Fatalf("old container must be left serving on failure:\n%s", seq)
	}
}

func withinMillis(r config.Role, ms int) config.Role {
	rd := *r.Ready
	rd.Within = config.Duration(time.Duration(ms) * time.Millisecond)
	rd.Interval = config.Duration(time.Millisecond)
	r.Ready = &rd
	return r
}
