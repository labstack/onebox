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
		case strings.Contains(cmd, "docker ps -aq") && strings.Contains(cmd, "onebox.release="):
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
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "onebox.release="):
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
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
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
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("resume roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "--scale") || strings.Contains(seq, "pull --quiet") {
		t.Fatalf("resume must not re-scale or re-pull:\n%s", seq)
	}
	if !strings.Contains(seq, "touch /tmp/onebox-drain") || !strings.Contains(seq, "docker stop -t 30 OLD1") {
		t.Fatalf("resume must continue drain+stop of old:\n%s", seq)
	}
}

func TestRollRoleCommandSequence(t *testing.T) {
	f := rollFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	ordered := []string{
		"docker compose -p sample --project-directory '/var/lib/onebox/app/releases/R1' -f '/var/lib/onebox/app/releases/R1/compose.yaml' pull --quiet web",
		"up -d --no-deps --no-recreate --scale web=2 web",
		"docker rename NEW1 sample-web-new",
		"docker exec OLD1 touch /tmp/onebox-drain",
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
	if strings.Index(seq, "onebox-drain") > strings.Index(seq, "docker stop") {
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
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
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

func TestRollProgressCompletesOnlyAfterRetirementAndSlots(t *testing.T) {
	f := rollFake()
	var out bytes.Buffer
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.HasPrefix(cmd, "docker stop ") || strings.HasPrefix(cmd, "docker rename ") {
			if strings.Contains(out.String(), "web rolling · 1/1") {
				t.Fatalf("progress completed before retirement/slot assignment: %s", out.String())
			}
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "web rolling · 1/1 · replicas converged") {
		t.Fatalf("successful roll must finish measured progress: %s", out.String())
	}
}

func TestRollProgressDoesNotCompleteOnFailedHealthcheck(t *testing.T) {
	f := rollFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "State.Health") && strings.Contains(cmd, "NEW1") {
			return transport.Result{Stdout: "starting\n"}, true
		}
		return base(cmd)
	}
	cfg := testConfig()
	cfg.Workloads["web"] = withinMillis(cfg.Workloads["web"], 1)
	var out bytes.Buffer
	e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "F"); err == nil {
		t.Fatal("expected failed healthcheck")
	}
	if strings.Contains(out.String(), "web rolling · 1/1") || strings.Contains(out.String(), "replicas converged") {
		t.Fatalf("failed roll must not show full progress: %s", out.String())
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
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
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

// The runner-side health poll defaults to 2s — matching the generated healthcheck
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
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
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
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
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
	err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml")
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

// routedRollFake publishes each endpoint from its requested document read onward.
func routedRollFake(base *transport.Fake, publishFrom map[string]int) *transport.Fake {
	previous := base.Dynamic
	reads := 0
	drained := false
	base.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.HasPrefix(cmd, "docker exec OLD1 touch ") {
			drained = true
		}
		if strings.Contains(cmd, ".NetworkSettings.Networks") {
			fields := strings.Fields(cmd)
			id := strings.Trim(fields[len(fields)-1], "'")
			if strings.HasPrefix(id, "OLD") {
				return transport.Result{Stdout: "10.0.0.100 "}, true
			}
			return transport.Result{Stdout: "10.0.0." + strings.TrimPrefix(id, "NEW") + " "}, true
		}
		if strings.HasPrefix(cmd, "cat ") && strings.Contains(cmd, "onebox.yml") {
			reads++
			body := "http:\n  services:\n    onebox_web:\n      loadBalancer:\n        servers:"
			servers := ""
			if !drained {
				servers += "\n          - url: http://10.0.0.100:3000"
			}
			for id, from := range publishFrom {
				if from > 0 && reads >= from {
					servers += "\n          - url: http://10.0.0." + strings.TrimPrefix(id, "NEW") + ":3000"
				}
			}
			if servers == "" {
				servers = " []"
			}
			return transport.Result{Stdout: body + servers + "\n"}, true
		}
		return previous(cmd)
	}
	return base
}

func routedRollConfig() *app.Resolved {
	cfg := testConfig()
	cfg.Proxy.Managed = true
	role := cfg.Workloads["web"]
	role.Routes = []app.Route{{Port: 3000}}
	cfg.Workloads["web"] = role
	return cfg
}

func TestRollRetiresOldOnlyAfterNewcomerPublished(t *testing.T) {
	f := routedRollFake(rollFake(), map[string]int{"NEW1": 3})
	clock := &virtualClock{}
	e := New(routedRollConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: clock.sleep, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, cmd := range f.Commands {
		if strings.HasPrefix(cmd, "cat ") && strings.Contains(cmd, "onebox.yml") {
			reads++
		}
		if strings.HasPrefix(cmd, "docker exec OLD1") && reads < 3 {
			t.Fatalf("retired after only %d route reads", reads)
		}
	}
	if reads < 3 {
		t.Fatalf("only %d route reads", reads)
	}
}

func TestRollRemovesNewcomerNeverPublished(t *testing.T) {
	f := routedRollFake(rollFake(), map[string]int{"NEW1": 0})
	clock := &virtualClock{}
	e := New(routedRollConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: clock.sleep, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
	err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "did not publish") {
		t.Fatalf("error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker rm -f NEW1") || strings.Contains(seq, "docker exec OLD1") {
		t.Fatal(seq)
	}
}

func TestRollResumeAdmitsAdoptedNewcomerBeforeRetiring(t *testing.T) {
	f := replicaFake(1, []string{"OLD1"}, map[string]string{"OLD1": "web"}, true)
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "State.Health") && strings.Contains(cmd, "NEW1") {
			return transport.Result{Stdout: "starting\n"}, true
		}
		return base(cmd)
	}
	cfg := testConfig()
	cfg.Workloads["web"] = withinMillis(cfg.Workloads["web"], 50)
	clock := &virtualClock{}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: clock.sleep, Now: clock.now})
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err == nil {
		t.Fatal("expected admission failure")
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "docker exec OLD1") || strings.Contains(seq, "docker rm -f NEW1") {
		t.Fatal(seq)
	}
}

func TestRollUnmanagedProxyReadsNoRoutes(t *testing.T) {
	f := rollFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range f.Commands {
		if strings.Contains(cmd, "onebox.yml") {
			t.Fatal(cmd)
		}
	}
}

func TestRetireWaitsForWithdrawalBeforeStop(t *testing.T) {
	testRetireWithdrawal(t, "delayed")
}

func TestRetireStopsWhenWithdrawalTimesOut(t *testing.T) {
	testRetireWithdrawal(t, "listed")
}

func TestRetireUnreadableRoutesAreNotWithdrawal(t *testing.T) {
	testRetireWithdrawal(t, "unreadable")
}

func TestRetireUnguardedHealthcheckSkipsWithdrawalGate(t *testing.T) {
	testRetireWithdrawal(t, "unguarded")
}

func testRetireWithdrawal(t *testing.T, mode string) {
	t.Helper()
	f := rollFake()
	base := f.Dynamic
	reads := 0
	stopped := false
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if mode == "unguarded" && strings.Contains(cmd, "Config.Healthcheck") {
			return transport.Result{Stdout: `{"Test":["CMD","/check"]}`}, true
		}
		if strings.Contains(cmd, ".NetworkSettings.Networks") {
			return transport.Result{Stdout: "10.0.0.9 "}, true
		}
		if strings.HasPrefix(cmd, "cat ") && strings.Contains(cmd, "onebox.yml") {
			reads++
			if mode == "unreadable" {
				return transport.Result{ExitCode: 1}, true
			}
			if mode == "listed" || reads <= 2 {
				return transport.Result{Stdout: "http:\n  services:\n    onebox_web:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.9:3000\n"}, true
			}
			return transport.Result{Stdout: "http: {}\n"}, true
		}
		if strings.HasPrefix(cmd, "docker stop ") {
			stopped = true
			if mode == "delayed" && reads != 3 {
				t.Errorf("stop after %d route reads, want 3", reads)
			}
		}
		return base(cmd)
	}
	clock := &virtualClock{}
	cfg := routedRollConfig()
	role := cfg.Workloads["web"]
	role.Drain = &app.Drain{Wait: "1s", Signal: "USR1"}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: clock.sleep, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
	err := e.retireContainer(context.Background(), "web", role, "OLD1", time.Second)
	seq := strings.Join(f.Commands, "\n")
	if mode == "listed" || mode == "unreadable" {
		if err == nil || !strings.Contains(err.Error(), "still routes") {
			t.Fatalf("error = %v, want still routes", err)
		}
		if stopped || strings.Contains(seq, "docker kill") || strings.Contains(seq, "docker rm") {
			t.Fatal(seq)
		}
		if !strings.Contains(seq, "touch "+app.DrainFile) {
			t.Fatal("container was not left draining")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if !stopped {
			t.Fatal("container was not stopped")
		}
		if mode == "unguarded" && (reads != 0 || clock.elapsed != 2*e.Opts.ConvergeBuffer) {
			t.Fatalf("reads=%d elapsed=%s", reads, clock.elapsed)
		}
	}
}

func TestRetireOldReplicaOffProxyNetworkIsWithdrawn(t *testing.T) {
	testRetireChangedRoutes(t, "off-network")
}
func TestRetireWaitsWhileOldReplicaServesARemovedRoute(t *testing.T) {
	testRetireChangedRoutes(t, "removed")
}
func TestRetireWaitsWhileOldReplicaServesAnotherProtocol(t *testing.T) {
	testRetireChangedRoutes(t, "tcp")
}
func testRetireChangedRoutes(t *testing.T, mode string) {
	t.Helper()
	f := rollFake()
	previous := f.Dynamic
	reads, stops := 0, 0
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, ".NetworkSettings.Networks") {
			if mode == "off-network" {
				return transport.Result{Stdout: " "}, true
			}
			return transport.Result{Stdout: "10.0.0.100 "}, true
		}
		if strings.HasPrefix(cmd, "cat ") && strings.Contains(cmd, "onebox.yml") {
			reads++
			if mode == "tcp" {
				return transport.Result{Stdout: "tcp:\n  services:\n    removed:\n      loadBalancer:\n        servers:\n          - address: 10.0.0.100:5432\n"}, true
			}
			if reads <= 2 {
				return transport.Result{Stdout: "http:\n  services:\n    onebox_web:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.100:3000\n"}, true
			}
			return transport.Result{Stdout: "{}"}, true
		}
		if strings.HasPrefix(cmd, "docker stop ") {
			stops++
			if mode == "off-network" && reads != 0 || mode == "removed" && reads != 3 {
				t.Errorf("stop after %d reads in %s", reads, mode)
			}
		}
		return previous(cmd)
	}
	cfg := routedRollConfig()
	role := cfg.Workloads["web"]
	if mode == "removed" {
		role.Routes = nil
	}
	clock := &virtualClock{}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: clock.sleep, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
	err := e.retireContainer(context.Background(), "web", role, "OLD1", time.Second)
	if mode == "tcp" {
		if err == nil || !strings.Contains(err.Error(), "still routes") || stops != 0 || clock.elapsed < routePublishBudget {
			t.Fatalf("err=%v stops=%d elapsed=%s", err, stops, clock.elapsed)
		}
	} else if err != nil || stops != 1 {
		t.Fatalf("err=%v stops=%d", err, stops)
	}
}
func TestRollResumeBlocksAdoptedHealthyUnpublishedNewcomer(t *testing.T) {
	f := routedRollFake(replicaFake(1, []string{"OLD1"}, map[string]string{"OLD1": "web"}, true), map[string]int{"NEW1": 0})
	clock := &virtualClock{}
	e := New(routedRollConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: clock.sleep, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
	err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "did not publish") {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "docker exec OLD1") {
		t.Fatal("drained old before admission")
	}
}
func TestRollWithdrawsOldReplicaInsideFullRoll(t *testing.T) {
	f := routedRollFake(rollFake(), map[string]int{"NEW1": 1})
	previous := f.Dynamic
	withdrawnRead := false
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		result, ok := previous(cmd)
		if strings.HasPrefix(cmd, "cat ") && strings.Contains(cmd, "onebox.yml") && !strings.Contains(result.Stdout, "10.0.0.100") {
			withdrawnRead = true
		}
		if strings.HasPrefix(cmd, "docker stop ") && strings.Contains(cmd, "OLD1") && !withdrawnRead {
			t.Error("stopped before a publication without OLD1")
		}
		return result, ok
	}
	clock := &virtualClock{}
	e := New(routedRollConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: clock.sleep, Now: clock.now, Wait: func(ctx context.Context, d time.Duration) error { clock.sleep(d); return ctx.Err() }})
	if err := e.RollRole(context.Background(), "web", "/var/lib/onebox/app/releases/R1/compose.yaml"); err != nil {
		t.Fatal(err)
	}
	if !withdrawnRead || !strings.Contains(strings.Join(f.Commands, "\n"), "docker stop -t 30 OLD1") {
		t.Fatal("missing withdrawal or stop")
	}
}
