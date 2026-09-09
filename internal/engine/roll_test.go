package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// replicaFake models a rolling deploy for tests: `desired` new replicas replace
// the given old containers. Each `--scale` creates the next NEWk; NEWk is
// healthy; an OLD turns unhealthy once drained (touch); `docker rm` removes a
// container; renames are tracked so name queries reflect the latest name.
// resume=true means NEW1 is already running before any scale (adoption path).
func replicaFake(desired int, oldIDs []string, oldNames map[string]string, resume bool) *transport.Fake {
	return replicaFakeWithStopped(desired, oldIDs, oldNames, resume, nil)
}

// replicaFakeWithStopped adds replicas that exist but are not running. They are
// invisible to `docker ps -q` and visible to `docker ps -aq`, which is exactly
// the accounting Compose uses for --scale.
func replicaFakeWithStopped(desired int, oldIDs []string, oldNames map[string]string, resume bool, stoppedIDs []string) *transport.Fake {
	f := &transport.Fake{}
	lastField := func(s string) string {
		fs := strings.Fields(s)
		if len(fs) == 0 {
			return ""
		}
		return fs[len(fs)-1]
	}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		// Every generated shell-form check carries the drain guard; a rollout
		// probes for it before poisoning health.
		if strings.Contains(cmd, "Config.Healthcheck") {
			return transport.Result{Stdout: `{"Test":` + guardedHealthcheck + `,"Interval":5000000000,"Retries":3}` + "\n"}, true
		}
		scale := 0
		removed := map[string]bool{}
		drained := map[string]bool{}
		name := map[string]string{}
		for k, v := range oldNames {
			name[k] = v
		}
		for _, c := range f.Commands {
			if strings.Contains(c, "--scale web=") {
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
		var stopped []string
		for _, id := range stoppedIDs {
			if !removed[id] {
				stopped = append(stopped, id)
			}
		}
		lines := func(ids []string) transport.Result {
			return transport.Result{Stdout: strings.Join(ids, "\n") + "\n"}
		}
		switch {
		// -aq before -q: "docker ps -aq" does not contain "docker ps -q".
		case strings.Contains(cmd, "docker ps -aq") && strings.Contains(cmd, "status=exited"):
			return lines(stopped), true
		case strings.Contains(cmd, "docker ps -aq") && strings.Contains(cmd, "ob.release="):
			return lines(news), true
		case strings.Contains(cmd, "docker ps -aq") && strings.Contains(cmd, "service='web'"):
			return lines(append(append(append([]string{}, olds...), news...), stopped...)), true
		case strings.Contains(cmd, "State.Status"):
			id := lastField(cmd)
			for _, s := range stopped {
				if s == id {
					return transport.Result{Stdout: "exited\n"}, true
				}
			}
			return transport.Result{Stdout: "running\n"}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "ob.release="):
			return transport.Result{Stdout: strings.Join(news, "\n") + "\n"}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service='web'"):
			return transport.Result{Stdout: strings.Join(append(append([]string{}, olds...), news...), "\n") + "\n"}, true
		case strings.Contains(cmd, "{{.Name}}"):
			id := lastField(cmd)
			n := name[id]
			if n == "" {
				n = "ledger-web-should-not-appear-x" // compose default before any rename
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

// rollFake: the common single-replica happy path (one old named `sample-web-1`).
func rollFake() *transport.Fake {
	return replicaFake(1, []string{"OLD1"}, map[string]string{"OLD1": "web"}, false)
}

func noSleep(time.Duration) {}

// A single-replica roll ends with the survivor in stable slot `sample-web-1`, renamed
// only AFTER the old is gone (so the name is free), and never carries the
// Compose-generated prefix (renamed to `sample-web-new` the instant it's created).
func TestRollRoleRenamesSurvivorToService(t *testing.T) {
	f := rollFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	early, rm, final := -1, -1, -1
	for i, c := range f.Commands {
		switch {
		case strings.Contains(c, "docker rename NEW1 sample-web-new"):
			early = i
		case strings.Contains(c, "docker rename NEW1 sample-web-1"):
			final = i
		case strings.Contains(c, "docker rm OLD1"):
			rm = i
		}
	}
	if early < 0 {
		t.Fatalf("newcomer must be renamed off the sample- prefix immediately:\n%s", strings.Join(f.Commands, "\n"))
	}
	if final < 0 {
		t.Fatalf("survivor must take numbered stable slot sample-web-1:\n%s", strings.Join(f.Commands, "\n"))
	}
	if !(early < rm && rm < final) {
		t.Fatalf("want sample-web-new(%d) < rm OLD1(%d) < sample-web-1(%d):\n%s", early, rm, final, strings.Join(f.Commands, "\n"))
	}
}

func TestRollRoleResumeAdoptsExistingNewcomer(t *testing.T) {
	f := replicaFake(1, []string{"OLD1"}, map[string]string{"OLD1": "web"}, true)
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
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
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	ordered := []string{
		"docker compose -p sample --project-directory '/var/lib/ob/sample/releases/R1' -f '/var/lib/ob/sample/releases/R1/compose.yaml' pull --quiet web",
		"up -d --no-deps --no-recreate --scale web=2 web",
		"docker rename NEW1 sample-web-new",
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
	// Drain MUST precede stop so SIGTERM never races the proxy.
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
	cfg.Workloads["web"] = withinMillis(cfg.Workloads["web"], 50)
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
// sample-web-1 and sample-web-2 — no Compose-generated prefix, both olds retired.
func TestRollRoleTwoReplicasCleanSlots(t *testing.T) {
	f := replicaFake(2, []string{"OLD1", "OLD2"}, map[string]string{"OLD1": "sample-web-1", "OLD2": "sample-web-2"}, false)
	cfg := testConfig()
	r := cfg.Workloads["web"]
	r.Replicas = 2
	cfg.Workloads["web"] = r
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("2-replica roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	for _, want := range []string{
		"docker rm OLD1", "docker rm OLD2", // both olds retired
		"docker rename NEW1 sample-web-1", "docker rename NEW2 sample-web-2", // clean slots
	} {
		if !strings.Contains(seq, want) {
			t.Fatalf("2-replica roll missing %q:\n%s", want, seq)
		}
	}
	if strings.Contains(seq, "ledger-web-should-not-appear") {
		t.Fatalf("no sample- prefixed name should be committed:\n%s", seq)
	}
}

// drain.grace sets the docker stop -t timeout when retiring a drained container;
// absent it stays at the conservative 30s (asserted by the sequence tests).
func TestRollRoleDrainGraceConfigurable(t *testing.T) {
	f := rollFake()
	cfg := testConfig()
	r := cfg.Workloads["web"]
	r.Drain = &app.Drain{Grace: "8s"}
	cfg.Workloads["web"] = r
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
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

// The ob-side health poll defaults to 2s — matching the generated healthcheck
// cadence — so joins and drain flips are detected promptly; within stays 120s.
// Declared values still win (asserted by the sequence tests).
func TestReadyTimingDefaults(t *testing.T) {
	within, interval := app.Workload{}.ReadyTiming()
	if interval != 2*time.Second {
		t.Fatalf("default poll interval = %v, want 2s", interval)
	}
	if within != 120*time.Second {
		t.Fatalf("default within = %v, want 120s", within)
	}
}

func withinMillis(r app.Workload, ms int) app.Workload {
	h := *r.Health
	h.Within = fmt.Sprintf("%dms", ms)
	h.Interval = "1ms"
	r.Health = &h
	return r
}

// The reported wedge: every replica stopped, so `docker ps -q` reports none and
// the roll asks Compose for `--scale web=1`. Compose counts the stopped ones
// toward that target, creates nothing, and removes the surplus itself — which
// surfaced as "scale up produced no new container" and required manual
// `docker rm` before `ob resume` could make progress.
func TestRollRoleReplacesStoppedReplicas(t *testing.T) {
	stopped := []string{"STOP1", "STOP2", "STOP3"}
	f := replicaFakeWithStopped(3, nil, nil, false, stopped)
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll over stopped replicas: %v", err)
	}
	joined := strings.Join(f.Commands, "\n")
	for _, id := range stopped {
		if !strings.Contains(joined, "docker rm -f "+id) {
			t.Fatalf("stopped replica %s was left for compose to count:\n%s", id, joined)
		}
	}
	// Order is the whole point: a sweep after the scale would not prevent the
	// miscount that made the scale a no-op.
	firstScale := strings.Index(joined, "--scale web=")
	for _, id := range stopped {
		if at := strings.Index(joined, "docker rm -f "+id); at > firstScale {
			t.Fatalf("%s removed after the first --scale:\n%s", id, joined)
		}
	}
}

// A stopped replica alongside running ones must not be counted either, and the
// running ones must still be retired through the drain protocol rather than
// swept.
func TestRollRoleSweepsOnlyStoppedReplicas(t *testing.T) {
	f := replicaFakeWithStopped(1, []string{"OLD1"}, map[string]string{"OLD1": "web"}, false, []string{"STOP1"})
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v", err)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "docker rm -f STOP1") {
		t.Fatalf("stopped replica not swept:\n%s", joined)
	}
	// The running old is drained, not swept: it may still be carrying traffic.
	if !strings.Contains(joined, "docker exec OLD1") {
		t.Fatalf("running old was not drained:\n%s", joined)
	}
	if strings.Index(joined, "docker rm -f STOP1") > strings.Index(joined, "docker exec OLD1") {
		t.Fatalf("sweep must precede the roll, not follow it:\n%s", joined)
	}
}

// A newcomer that exits on start used to report "scale up produced no new
// container" — a claim about Compose that hid the real cause and left the dead
// container behind for the next scale-up to miscount.
func TestRollRoleReportsNewcomerThatExited(t *testing.T) {
	f := replicaFakeWithStopped(1, []string{"OLD1"}, map[string]string{"OLD1": "web"}, false, nil)
	inner := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "State.Status") && strings.Contains(cmd, "NEW1") {
			return transport.Result{Stdout: "exited\n"}, true
		}
		if strings.Contains(cmd, "State.Health") && strings.Contains(cmd, "NEW1") {
			return transport.Result{Stdout: "starting\n"}, true
		}
		return inner(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "exited before becoming healthy") {
		t.Fatalf("roll error = %v, want the newcomer's own exit", err)
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "docker rm -f NEW1") {
		t.Fatalf("dead newcomer was left behind:\n%s", strings.Join(f.Commands, "\n"))
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "docker rm -f OLD1") {
		t.Fatalf("the old replica must keep serving:\n%s", strings.Join(f.Commands, "\n"))
	}
}
