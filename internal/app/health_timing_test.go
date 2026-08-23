package app

import (
	"strings"
	"testing"
	"time"
)

// The interval Docker probes at and the cadence Onebox polls `docker inspect`
// at are different things. Conflating them made the drain budget model a flip
// that took 3 x 30s against a budget of 5 x 2s.
func TestHealthIntervalIsTheProbeCadenceNotThePollCadence(t *testing.T) {
	shorthand := Workload{Health: &Health{HTTP: "/healthz"}}
	if got := shorthand.HealthInterval(); got != 5*time.Second {
		t.Fatalf("default health interval = %v, want 5s", got)
	}
	_, poll := shorthand.ReadyTiming()
	if poll != 2*time.Second {
		t.Fatalf("poll cadence = %v, want 2s — it is a local inspect, not a container probe", poll)
	}

	authored := Workload{Health: &Health{HTTP: "/healthz", Interval: "30s"}}
	if got := authored.HealthInterval(); got != 30*time.Second {
		t.Fatalf("authored health interval = %v, want 30s", got)
	}
}

// Every value the drain budget is computed from must also be the value the
// container was created with, or the budget models a flip that cannot happen.
func TestHealthcheckAlwaysCarriesTheIntervalAndRetriesTheBudgetAssumes(t *testing.T) {
	workload := Workload{Health: &Health{HTTP: "/healthz", Port: 8080}}
	check := healthcheck(workload.Health)
	if check == nil {
		t.Fatal("no healthcheck generated")
	}
	if got, want := check["interval"], workload.HealthInterval().String(); got != want {
		t.Fatalf("healthcheck interval = %v, want %v", got, want)
	}
	if got, want := check["retries"], workload.HealthRetries(); got != want {
		t.Fatalf("healthcheck retries = %v, want %v", got, want)
	}
}

// The authored duration is normalised, not echoed: `interval: 90s` renders as
// 1m30s. Same instruction to the runtime, different spelling.
func TestAuthoredHealthTimingReachesTheHealthcheck(t *testing.T) {
	workload := Workload{Health: &Health{HTTP: "/healthz", Port: 8080, Interval: "7s", Retries: 4}}
	check := healthcheck(workload.Health)
	if got := check["interval"]; got != "7s" {
		t.Fatalf("healthcheck interval = %v, want the authored 7s", got)
	}
	if got := check["retries"]; got != 4 {
		t.Fatalf("healthcheck retries = %v, want the authored 4", got)
	}
}

// Before the interval was written down, a shorthand healthcheck inherited the
// runtime's 30s probe interval, which gave a booting container roughly a
// minute and a half before it could be called unhealthy. Writing a 5s interval
// without also writing a start period would take that away and mark a
// slow-starting app unhealthy while it is still coming up — visible to
// dependency conditions, restart watchers, and alerting.
func TestHealthcheckGivesABootingContainerAStartPeriod(t *testing.T) {
	check := healthcheck(&Health{HTTP: "/healthz", Port: 8080})
	if got := check["start_period"]; got != defaultHealthStartPeriod.String() {
		t.Fatalf("start_period = %v, want %v", got, defaultHealthStartPeriod)
	}
}

// Onebox's duration grammar accepts days; Compose's does not know the unit at
// all. Every duration written into the healthcheck is therefore normalised to
// units Compose can parse, or the deploy dies at the compose step with
// `time: unknown unit "d"` — for a value that validated cleanly.
func TestAuthoredHealthDurationsAreNormalisedForCompose(t *testing.T) {
	check := healthcheck(&Health{HTTP: "/healthz", Port: 8080, StartPeriod: "90s", Interval: "7d"})
	if got := check["start_period"]; got != "1m30s" {
		t.Fatalf("start_period = %v, want the normalised 1m30s", got)
	}
	if got := check["interval"]; got != "168h0m0s" {
		t.Fatalf("interval = %v, want the normalised 168h0m0s", got)
	}
	for _, field := range []string{"interval", "start_period"} {
		if strings.ContainsAny(check[field].(string), "dwy") {
			t.Fatalf("%s = %v carries a unit Compose cannot parse", field, check[field])
		}
	}
}

// The readiness budget has to cover at least one full flip cycle, or a rollout
// gives up before the container's own healthcheck could have reported anything.
func TestReadyBudgetCoversAtLeastOneFlipCycle(t *testing.T) {
	slow := Workload{Health: &Health{HTTP: "/healthz", Interval: "3m"}}
	within, _ := slow.ReadyTiming()
	cycle := time.Duration(slow.HealthRetries()+1)*slow.HealthInterval() + slow.HealthStartPeriod()
	if within < cycle {
		t.Fatalf("within = %v, want at least one flip cycle (%v)", within, cycle)
	}

	ordinary := Workload{Health: &Health{HTTP: "/healthz"}}
	if within, _ := ordinary.ReadyTiming(); within != 120*time.Second {
		t.Fatalf("within = %v, want the 120s default for ordinary timings", within)
	}

	authored := Workload{Health: &Health{HTTP: "/healthz", Interval: "3m", Within: "45s"}}
	if within, _ := authored.ReadyTiming(); within != 45*time.Second {
		t.Fatalf("within = %v, want the authored 45s — an explicit budget is not second-guessed", within)
	}
}

// A retries count large enough to overflow the drain budget's arithmetic turns
// it negative, which expires instantly — the failure the budget exists to
// prevent, reached by a route validation could have closed.
func TestAbsurdRetriesIsRejected(t *testing.T) {
	_, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: ledger\n"+
		"environments: {production: {server: root@10.0.0.1}}\n"+
		"image: nginx\ndomain: d.example.com\nport: 8080\n"+
		"health: {http: /healthz, retries: 100000000000}\n"), "ob.yml")
	if err == nil {
		t.Fatal("a retries count that overflows the drain budget was accepted")
	}
	if !strings.Contains(err.Error(), "retries") {
		t.Fatalf("error does not name retries: %v", err)
	}
}

// Bounding retries alone does not close the overflow: the readiness budget
// multiplies it by the interval, so an absurd interval wraps the arithmetic
// just as well. A wrapped budget is an effectively infinite one, and a
// crash-looping newcomer that should abort the rollout hangs instead.
func TestAbsurdHealthDurationsAreRejected(t *testing.T) {
	for name, health := range map[string]string{
		"interval":     "{http: /healthz, interval: 100000d}",
		"start_period": "{http: /healthz, start_period: 100000d}",
		"within":       "{http: /healthz, within: 100000d}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: ledger\n"+
				"environments: {production: {server: root@10.0.0.1}}\n"+
				"image: nginx\ndomain: d.example.com\nport: 8080\n"+
				"health: "+health+"\n"), "ob.yml")
			if err == nil {
				t.Fatalf("health %s was accepted", health)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error does not name %s: %v", name, err)
			}
		})
	}
}

// The budget stays inside int64 nanoseconds at every accepted extreme.
func TestReadyBudgetStaysPositiveAtTheAcceptedExtremes(t *testing.T) {
	extreme := Workload{
		Health: &Health{
			HTTP: "/healthz", Interval: maxLifecycleDuration.String(),
			StartPeriod: maxLifecycleDuration.String(), Retries: maxHealthRetries,
		},
		// An authored wait, so the drain assertion below is about the
		// arithmetic rather than about the zero returned when none was written.
		Drain: &Drain{Wait: maxLifecycleDuration.String()},
	}
	within, _ := extreme.ReadyTiming()
	if within <= 0 {
		t.Fatalf("within = %v — the budget overflowed", within)
	}
	if drain := extreme.DrainWait(); drain <= 0 {
		t.Fatalf("drain wait = %v — the budget overflowed", drain)
	}
}

// `start_period: 0s` means "no grace" to the runtime, and an author who writes
// it means it. Routing emission through a defaulted accessor turned it into
// the 30s default with no warning.
func TestAuthoredZeroStartPeriodIsKept(t *testing.T) {
	check := healthcheck(&Health{HTTP: "/healthz", Port: 8080, StartPeriod: "0s"})
	if got := check["start_period"]; got != "0s" {
		t.Fatalf("start_period = %v, want the authored 0s", got)
	}
}

// Every duration Compose will parse has to be normalised, not only the ones in
// the healthcheck. stop_grace_period is authored with the same grammar.
func TestStopGracePeriodIsNormalisedForCompose(t *testing.T) {
	workload := Workload{Role: RoleApplication, Drain: &Drain{Grace: "1d"}}
	svc := map[string]any{}
	applyStopGrace(svc, workload)
	if got := svc["stop_grace_period"]; got != "24h0m0s" {
		t.Fatalf("stop_grace_period = %v, want the normalised 24h0m0s", got)
	}
}

// A day count large enough to overflow int64 nanoseconds must not parse as a
// valid duration: it wraps to a negative — or worse, to a plausible positive —
// and slips past every bound expressed as `d > limit`.
func TestParseDurationRejectsOverflowingDayCounts(t *testing.T) {
	for _, raw := range []string{"1000000d", "213504d", "9223372036854775807d"} {
		if d, ok := ParseDuration(raw); ok {
			t.Fatalf("ParseDuration(%q) = %v, want rejection", raw, d)
		}
	}
	if d, ok := ParseDuration("14d"); !ok || d != 14*24*time.Hour {
		t.Fatalf("ParseDuration(\"14d\") = %v, %v — an ordinary day count must still parse", d, ok)
	}
}

// A day count that wraps int64 must be rejected by validation too, not merely
// by the parser: the two together are what make the bound mean something.
func TestOverflowingDayCountIsRejectedAtLoad(t *testing.T) {
	_, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: ledger\n"+
		"environments: {production: {server: root@10.0.0.1}}\n"+
		"image: nginx\ndomain: d.example.com\nport: 8080\n"+
		"health: {http: /healthz, interval: 1000000d}\n"), "ob.yml")
	if err == nil {
		t.Fatal("an interval that overflows int64 nanoseconds was accepted")
	}
	if !strings.Contains(err.Error(), "interval") {
		t.Fatalf("error does not name interval: %v", err)
	}
}

func TestAbsurdDrainDurationsAreRejected(t *testing.T) {
	for name, drain := range map[string]string{
		"wait":  "{wait: 100000d}",
		"grace": "{grace: 100000d}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: ledger\n"+
				"environments: {production: {server: root@10.0.0.1}}\n"+
				"workloads: {web: {image: nginx, domain: d.example.com, port: 8080, drain: "+drain+"}}\n"), "ob.yml")
			if err == nil {
				t.Fatalf("drain %s was accepted", drain)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error does not name %s: %v", name, err)
			}
		})
	}
}
