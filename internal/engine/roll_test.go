package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/transport"
)

// replicaFake models a rolling deploy for tests: `desired` new replicas replace
// the given old containers. Each `--scale` creates the next NEWk; NEWk is
// healthy; an OLD turns unhealthy once drained (touch); `docker rm` removes a
// container; renames are tracked so name queries reflect the latest name.
// resume=true means NEW1 is already running before any scale (adoption path).
func replicaFake(desired int, oldIDs []string, oldNames map[string]string, resume bool) *transport.Fake {
	f := &transport.Fake{}
	lastField := func(s string) string {
		fs := strings.Fields(s)
		if len(fs) == 0 {
			return ""
		}
		return fs[len(fs)-1]
	}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		scale := 0
		removed := map[string]bool{}
		drained := map[string]bool{}
		name := map[string]string{}
		for k, v := range oldNames {
			name[k] = v
		}
		for _, c := range f.Commands {
			if strings.Contains(c, "--scale server=") {
				scale++
			}
			if i := strings.Index(c, "docker rm -f "); i >= 0 {
				removed[strings.Fields(c[i+len("docker rm -f "):])[0]] = true
			} else if i := strings.Index(c, "docker rm "); i >= 0 {
				removed[strings.Fields(c[i+len("docker rm "):])[0]] = true
			}
			if i := strings.Index(c, "docker exec "); i >= 0 && strings.Contains(c, "touch") {
				drained[strings.Fields(c[i+len("docker exec "):])[0]] = true
			}
			if i := strings.Index(c, "docker rename "); i >= 0 {
				fs := strings.Fields(c[i+len("docker rename "):])
				if len(fs) >= 2 {
					name[fs[0]] = fs[1]
				}
			}
		}
		created := scale
		if resume && created == 0 {
			created = 1 // NEW1 pre-exists
		}
		var news []string
		for k := 1; k <= created; k++ {
			id := fmt.Sprintf("NEW%d", k)
			if !removed[id] {
				news = append(news, id)
			}
		}
		var olds []string
		for _, id := range oldIDs {
			if !removed[id] {
				olds = append(olds, id)
			}
		}
		switch {
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "ob.release="):
			return transport.Result{Stdout: strings.Join(news, "\n") + "\n"}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service='server'"):
			return transport.Result{Stdout: strings.Join(append(append([]string{}, olds...), news...), "\n") + "\n"}, true
		case strings.Contains(cmd, "{{.Name}}"):
			id := lastField(cmd)
			n := name[id]
			if n == "" {
				n = "monk-server-x" // compose default before any rename
			}
			return transport.Result{Stdout: "/" + n + "\n"}, true
		case strings.Contains(cmd, "State.Health"):
			id := lastField(cmd)
			if strings.HasPrefix(id, "NEW") {
				return transport.Result{Stdout: "healthy\n"}, true
			}
			if drained[id] {
				return transport.Result{Stdout: "unhealthy\n"}, true
			}
			return transport.Result{Stdout: "healthy\n"}, true
		}
		return transport.Result{}, false
	}
	return f
}

// rollFake: the common single-replica happy path (one old named `server`).
func rollFake() *transport.Fake {
	return replicaFake(1, []string{"OLD1"}, map[string]string{"OLD1": "server"}, false)
}

func noSleep(time.Duration) {}

// A single-replica roll ends with the survivor named plainly `server`, renamed
// only AFTER the old is gone (so the name is free), and never carries the
// monk- prefix (renamed to a transient server-new the instant it's created).
func TestRollRoleRenamesSurvivorToService(t *testing.T) {
	f := rollFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	early, rm, final := -1, -1, -1
	for i, c := range f.Commands {
		switch {
		case strings.Contains(c, "docker rename NEW1 server-new"):
			early = i
		case strings.Contains(c, "docker rename NEW1 server"):
			final = i
		case strings.Contains(c, "docker rm OLD1"):
			rm = i
		}
	}
	if early < 0 {
		t.Fatalf("newcomer must be renamed off the monk- prefix immediately:\n%s", strings.Join(f.Commands, "\n"))
	}
	if final < 0 {
		t.Fatalf("survivor must take the plain service name:\n%s", strings.Join(f.Commands, "\n"))
	}
	if !(early < rm && rm < final) {
		t.Fatalf("want server-new(%d) < rm OLD1(%d) < server(%d):\n%s", early, rm, final, strings.Join(f.Commands, "\n"))
	}
}

func TestRollRoleResumeAdoptsExistingNewcomer(t *testing.T) {
	f := replicaFake(1, []string{"OLD1"}, map[string]string{"OLD1": "server"}, true)
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("resume roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "--scale") || strings.Contains(seq, "pull --quiet") {
		t.Fatalf("resume must not re-scale or re-pull:\n%s", seq)
	}
	if !strings.Contains(seq, "touch /tmp/ob-drain") || !strings.Contains(seq, "docker stop -t 30 OLD1") {
		t.Fatalf("resume must continue drain+stop of old:\n%s", seq)
	}
}

func TestRollRoleCommandSequence(t *testing.T) {
	f := rollFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	ordered := []string{
		"docker compose -p monk -f '/var/lib/ob/monk/releases/R1/compose.yaml' pull --quiet server",
		"up -d --no-deps --no-recreate --scale server=2 server",
		"docker rename NEW1 server-new",
		"docker exec OLD1 touch /tmp/ob-drain",
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
	if strings.Index(seq, "ob-drain") > strings.Index(seq, "docker stop") {
		t.Fatal("drain must happen before stop")
	}
}

func TestRollRoleAbortsOnUnhealthyNew(t *testing.T) {
	f := rollFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "State.Health") && strings.Contains(cmd, "NEW1") {
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

// A 2-replica roll surges each new one in turn and ends with both named
// server-1 and server-2 — no monk- prefix, both olds retired.
func TestRollRoleTwoReplicasCleanSlots(t *testing.T) {
	f := replicaFake(2, []string{"OLD1", "OLD2"}, map[string]string{"OLD1": "server-1", "OLD2": "server-2"}, false)
	cfg := testConfig()
	r := cfg.Roles["web"]
	r.Replicas = 2
	cfg.Roles["web"] = r
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("2-replica roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	for _, want := range []string{
		"docker rm OLD1", "docker rm OLD2", // both olds retired
		"docker rename NEW1 server-1", "docker rename NEW2 server-2", // clean slots
	} {
		if !strings.Contains(seq, want) {
			t.Fatalf("2-replica roll missing %q:\n%s", want, seq)
		}
	}
	if strings.Contains(seq, "monk-server") {
		t.Fatalf("no monk- prefixed name should be committed:\n%s", seq)
	}
}

// drain.grace sets the docker stop -t timeout when retiring a drained container;
// absent it stays at the conservative 30s (asserted by the sequence tests).
func TestRollRoleDrainGraceConfigurable(t *testing.T) {
	f := rollFake()
	cfg := testConfig()
	r := cfg.Roles["web"]
	r.Drain = &config.Drain{Grace: config.Duration(8 * time.Second)}
	cfg.Roles["web"] = r
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker stop -t 8 OLD1") {
		t.Fatalf("drain.grace: want `docker stop -t 8 OLD1`:\n%s", seq)
	}
	if strings.Contains(seq, "docker stop -t 30") {
		t.Fatalf("drain.grace not applied — still using default 30:\n%s", seq)
	}
}

func withinMillis(r config.Role, ms int) config.Role {
	rd := *r.Ready
	rd.Within = config.Duration(time.Duration(ms) * time.Millisecond)
	rd.Interval = config.Duration(time.Millisecond)
	r.Ready = &rd
	return r
}
