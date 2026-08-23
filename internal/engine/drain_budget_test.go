package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// virtualClock advances only when the engine sleeps, so a test can model a
// container that takes real seconds to flip without spending them.
type virtualClock struct {
	mu      sync.Mutex
	elapsed time.Duration
}

func (c *virtualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC).Add(c.elapsed)
}

func (c *virtualClock) sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.elapsed += d
}

func (c *virtualClock) since(mark time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.elapsed - mark
}

// flippingFake models what Docker actually does: a drained container keeps
// reporting healthy until `retries` consecutive probes have failed, one probe
// every `interval`. The old fake flipped the instant it was drained, which is
// why a budget too short for the real flip never failed a test.
func flippingFake(clock *virtualClock, flipAfter time.Duration, baked string) *transport.Fake {
	f := &transport.Fake{}
	var drainedAt sync.Map
	lastField := func(s string) string {
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return ""
		}
		return fields[len(fields)-1]
	}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "Config.Healthcheck") {
			return transport.Result{Stdout: baked + "\n"}, true
		}
		removed := map[string]bool{}
		scale := 0
		for _, c := range f.Commands {
			if strings.Contains(c, "--scale web=") {
				scale++
			}
			if i := strings.Index(c, "docker rm "); i >= 0 {
				removed[strings.Fields(strings.TrimPrefix(c[i+len("docker rm "):], "-f "))[0]] = true
			}
			if i := strings.Index(c, "docker exec "); i >= 0 && strings.Contains(c, "touch") {
				id := strings.Fields(c[i+len("docker exec "):])[0]
				clock.mu.Lock()
				elapsed := clock.elapsed
				clock.mu.Unlock()
				drainedAt.LoadOrStore(id, elapsed)
			}
		}
		var news []string
		for k := 1; k <= scale; k++ {
			if id := fmt.Sprintf("NEW%d", k); !removed[id] {
				news = append(news, id)
			}
		}
		var olds []string
		if !removed["OLD1"] {
			olds = append(olds, "OLD1")
		}
		switch {
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "ob.release="):
			return transport.Result{Stdout: strings.Join(news, "\n") + "\n"}, true
		case strings.Contains(cmd, "docker ps -q") && strings.Contains(cmd, "service='web'"):
			return transport.Result{Stdout: strings.Join(append(append([]string{}, olds...), news...), "\n") + "\n"}, true
		case strings.Contains(cmd, "{{.Name}}"):
			return transport.Result{Stdout: "/web\n"}, true
		case strings.Contains(cmd, "State.Health"):
			id := lastField(cmd)
			if strings.HasPrefix(id, "NEW") {
				return transport.Result{Stdout: "healthy\n"}, true
			}
			mark, ok := drainedAt.Load(id)
			if !ok {
				return transport.Result{Stdout: "healthy\n"}, true
			}
			if clock.since(mark.(time.Duration)) < flipAfter {
				return transport.Result{Stdout: "healthy\n"}, true
			}
			return transport.Result{Stdout: "unhealthy\n"}, true
		}
		return transport.Result{}, false
	}
	return f
}

// The budget exists so a container is never stopped while the proxy may still
// be routing to it. With the shorthand `health: /path`, the container flips
// after retries × the generated interval; a budget derived from anything else
// expires first and stops it anyway — the exact outcome the budget prevents.
func TestDrainBudgetCoversTheFlipTheGeneratedHealthcheckProduces(t *testing.T) {
	clock := &virtualClock{}
	config := testConfig()
	workload := config.Workloads["web"]
	workload.Health = &app.Health{HTTP: "/healthz", Port: 8080}
	workload.Drain = nil
	config.Workloads["web"] = workload

	// Worst case, not best: a real probe cycle is not aligned to the moment the
	// drain file appears, so the flip can take up to one extra interval. A
	// budget of exactly retries x interval would pass a best-case test and
	// still strand containers in the field.
	flip := time.Duration(workload.HealthRetries()+1)*workload.HealthInterval() - time.Millisecond
	fake := flippingFake(clock, flip, bakedHealthcheckJSON("5s", 3))
	out := &bytes.Buffer{}
	e := New(config, testProject(t), fake, Options{Out: out, Sleep: clock.sleep, Now: clock.now})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v", err)
	}
	assertDrained(t, out.String())
	if strings.Contains(out.String(), "never reported unhealthy") {
		t.Fatalf("the drain budget expired before the container could flip:\n%s", out.String())
	}
}

// assertDrained fails a test that would otherwise pass vacuously: a rollout
// that decides the health check cannot be drain-guarded skips the wait
// entirely, so it never warns no matter how wrong the budget is.
func assertDrained(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, "cannot be drain-guarded") {
		t.Fatalf("the rollout skipped the drain wait, so the budget was never exercised:\n%s", output)
	}
}

// bakedHealthcheckJSON is what `docker inspect .Config.Healthcheck` returns for
// a container: durations in nanoseconds, and an omitted key meaning "the
// runtime's own default" rather than zero.
func bakedHealthcheckJSON(interval string, retries int) string {
	fields := `"Test":` + guardedHealthcheck
	if interval != "" {
		d, err := time.ParseDuration(interval)
		if err != nil {
			panic(err)
		}
		fields += fmt.Sprintf(`,"Interval":%d`, d.Nanoseconds())
	}
	if retries > 0 {
		fields += fmt.Sprintf(`,"Retries":%d`, retries)
	}
	return "{" + fields + "}"
}

// The containers a deploy drains were created by the PREVIOUS deploy, so they
// carry the previous healthcheck. Budgeting from the spec being deployed means
// the first rollout after any change to the probe timing — including an upgrade
// that changes Onebox's own default — budgets for a flip the running container
// cannot perform. That is the reported failure, one release later.
func TestDrainBudgetCoversAContainerBakedBeforeTheChange(t *testing.T) {
	clock := &virtualClock{}
	config := testConfig()
	workload := config.Workloads["web"]
	workload.Health = &app.Health{HTTP: "/healthz", Port: 8080}
	workload.Drain = nil
	config.Workloads["web"] = workload

	// No Interval and no Retries: exactly what Onebox emitted before this fix,
	// which Docker runs at its own 30s default.
	const dockerDefaultInterval = 30 * time.Second
	fake := flippingFake(clock, 4*dockerDefaultInterval-time.Millisecond, bakedHealthcheckJSON("", 0))
	out := &bytes.Buffer{}
	e := New(config, testProject(t), fake, Options{Out: out, Sleep: clock.sleep, Now: clock.now})
	if err := e.RollRole(context.Background(), "web", "/var/lib/ob/sample/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v", err)
	}
	assertDrained(t, out.String())
	if strings.Contains(out.String(), "never reported unhealthy") {
		t.Fatalf("the budget ignored the healthcheck the draining container actually carries:\n%s", out.String())
	}
}
