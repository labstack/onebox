package app

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The runtime view is what the execution engine consumes. A project describes
// intent; the engine needs the handful of resolved answers that intent implies
// — what to release, in what order, how long to wait, when to give up.
//
// These live here rather than in the engine because they are properties of the
// contract, not of one execution path. Two callers asking "how many replicas"
// must not be able to disagree.

// Compose service names are workload names. The old contract carried a separate
// `service` field so a declaration could point at a service in someone else's
// Compose file; generation now emits the workload's own name, and a
// Compose-referenced workload is merged in under that name too. One name.

// Mode is the rollout strategy: rolling replaces one replica at a time behind
// health, recreate stops the old container first. Jobs and daemons default to
// recreate because neither can be load-balanced through a transition.
func (w Workload) Mode() string {
	if w.Strategy != "" {
		return w.Strategy
	}
	// Rolling means: stand the newcomer up, wait for it to report healthy,
	// then retire an old one. A workload with no health check has nothing to
	// wait for — Docker reports its health as "none" forever — so defaulting
	// an application to rolling made the documented smallest project
	// undeployable. It gets recreate instead, and `ob canonical` shows the
	// choice with `# default` beside it.
	if w.Role == RoleApplication && w.Health != nil {
		return "rolling"
	}
	return "recreate"
}

// Count is the steady-state replica count. The schema defaults it to 1, so a
// zero here means the value reached us without passing through the loader.
func (w Workload) Count() int {
	if w.Replicas < 1 {
		return 1
	}
	return w.Replicas
}

// StopGraceSeconds is the SIGTERM→SIGKILL budget for `docker stop -t`, which
// takes whole seconds. A positive sub-second grace rounds UP: truncating 500ms
// to 0 would mean an immediate kill, the opposite of what was asked for.
func (w Workload) StopGraceSeconds() int {
	if w.Drain != nil && w.Drain.Grace != "" {
		if d, ok := ParseDuration(w.Drain.Grace); ok && d > 0 {
			return int(math.Ceil(d.Seconds()))
		}
	}
	return 30
}

// HealthRetries is the consecutive-failure count before the runtime flips a
// container unhealthy. It governs how fast a draining container is dropped by
// the proxy — the flip takes Retries × HealthInterval — so the drain budget is
// derived from it rather than guessed alongside it.
func (w Workload) HealthRetries() int {
	if w.Health != nil && w.Health.Retries > 0 {
		return w.Health.Retries
	}
	return defaultHealthRetries
}

// HealthInterval is the delay between the container's own health probes: the
// cadence the *runtime* runs the check at, not the cadence Onebox polls
// `docker inspect` at. The two are different jobs — one is a probe inside the
// container, the other a local query — and treating them as one number made
// the drain budget model a flip that could not happen in the time allowed.
//
// Both this and HealthRetries are written into the generated healthcheck, so
// the values a budget is computed from are the values the container was
// actually created with. Leaving either to the runtime's own default means
// budgeting against a number Onebox never chose and cannot see.
func (w Workload) HealthInterval() time.Duration {
	if w.Health != nil {
		if d, ok := ParseDuration(w.Health.Interval); ok && d > 0 {
			return d
		}
	}
	return defaultHealthInterval
}

// HealthStartPeriod is the grace a container gets before a failed probe counts
// against it. It is written into every generated healthcheck for the same
// reason the interval is: a five-second probe with no grace would call a
// perfectly healthy application unhealthy while it is still starting, and that
// verdict is visible to dependency conditions, restart watchers and alerting
// long before the rollout would notice it.
func (w Workload) HealthStartPeriod() time.Duration {
	if w.Health != nil {
		if d, ok := ParseDuration(w.Health.StartPeriod); ok && d > 0 {
			return d
		}
	}
	return defaultHealthStartPeriod
}

const (
	// A probe every five seconds costs twelve requests a minute per container
	// and lets a drained container leave rotation in fifteen — fast enough
	// that a rolling deploy is not dominated by waiting for the flip, cheap
	// enough to run against every replica forever.
	defaultHealthInterval = 5 * time.Second
	defaultHealthRetries  = 3
	// Thirty seconds is what a shorthand healthcheck effectively had before the
	// interval was written down: the runtime's own 30s interval meant the first
	// probe did not land until then. Keeping that as the grace means writing
	// the interval down costs a booting container nothing. The runtime leaves
	// the start period at the first success, so a fast application pays none of
	// it.
	defaultHealthStartPeriod = 30 * time.Second
)

// ReadyTiming is the health gate's overall budget and the cadence Onebox polls
// `docker inspect` at while waiting. The poll cadence is deliberately not the
// container's probe interval: inspecting locally is cheap, so a slow probe
// interval should not also make Onebox notice the result slowly.
func (w Workload) ReadyTiming() (within, interval time.Duration) {
	interval = 2 * time.Second
	if w.Health != nil {
		if d, ok := ParseDuration(w.Health.Within); ok && d > 0 {
			return d, interval
		}
	}
	// The default budget stretches to cover one full flip cycle when the probe
	// timing is slow enough to need it. A rollout that gives up before the
	// container's healthcheck could have reported anything is not measuring the
	// application, and 120s is a figure chosen for ordinary probe timings, not
	// a statement that a three-minute interval should fail.
	within = 120 * time.Second
	if cycle := time.Duration(w.HealthRetries()+1)*w.HealthInterval() + w.HealthStartPeriod(); cycle > within {
		within = cycle
	}
	return within, interval
}

// DrainWait is how long to leave a container marked unhealthy before stopping
// it, so the proxy has time to notice and stop sending it traffic.
//
// Only an authored wait counts. The derived value this used to fall back to
// was unreachable — every caller checks `drain.wait` was written before asking
// — so it existed only to be printed by the plan, promising a pause no deploy
// ever took. Deriving one for real would add a sleep to every deploy that
// names a drain signal, which is a change to make deliberately, not by way of
// a fallback nothing calls.
func (w Workload) DrainWait() time.Duration {
	if w.Drain != nil && w.Drain.Wait != "" {
		if d, ok := ParseDuration(w.Drain.Wait); ok {
			return d
		}
	}
	return 0
}

// DrainSignal is the signal sent to begin a graceful stop.
func (w Workload) DrainSignal() string {
	if w.Drain != nil && w.Drain.Signal != "" {
		return w.Drain.Signal
	}
	return "TERM"
}

// IsJob reports a workload that runs to completion. Jobs are released before
// the rest and are never health-gated, because a job that stays up has failed.
func (w Workload) IsJob() bool { return w.Role == RoleJob }

// HoldsDurableData answers the question three separate places used to guess at.
//
// The contract publishes `persistence.mode` defaulting to durable, but the
// block is optional, so the default was unreachable unless it was written —
// and doctor, the migration-backup requirement and the backup gate each
// read an absent block as "not durable". A workload with a managed named
// volume holds data that outlives the release whether or not it says so.
//
// A bind mount is deliberately not durable here. Absolute sources are external
// host state Onebox does not own. Relative sources are read-only release
// content, so they cannot hold changing application data. Counting either
// would demand a backup report for every `./config` mount, and a warning that
// fires on everything is one nobody reads.
func (w Workload) HoldsDurableData() bool {
	if w.Persistence != nil {
		return w.Persistence.Mode == "durable"
	}
	for _, v := range w.Volumes {
		if !v.IsBind() {
			return true
		}
	}
	return false
}

// HasBindMounts reports whether any volume mounts a host path rather than a
// Onebox-managed named volume.
func (w Workload) HasBindMounts() bool {
	for _, v := range w.Volumes {
		if v.IsBind() {
			return true
		}
	}
	return false
}

// Role names. They are the schema's discriminator, so they are constants rather
// than string literals scattered across the execution path.
const (
	RoleApplication = "application"
	RoleWorker      = "worker"
	RoleDaemon      = "daemon"
	RoleJob         = "job"
)

// ReleaseOrder is the sequence long-running workloads are released in.
//
// An explicit deployment.order wins, and the loader has already checked that it
// names every workload. Otherwise the order comes from `needs`: a workload is
// released after everything it declares a prerequisite on. That is the same
// graph the runtime already uses to sequence startup, so an author who declared
// their dependencies once does not have to restate them as a deploy order.
func (p *Spec) ReleaseOrder() []string {
	var runnable []string
	for _, name := range sortedKeys(p.Workloads) {
		if !p.Workloads[name].IsJob() {
			runnable = append(runnable, name)
		}
	}
	if len(p.Deployment.Order) > 0 {
		return filterTo(p.Deployment.Order, runnable)
	}
	return p.topological(runnable)
}

// JobOrder is the stable dependency order for every declared job. It describes
// the release runtime, not which jobs a deploy executes; callers that execute a
// release phase must use JobOrderFor so manual jobs remain deploy-inert.
func (p *Spec) JobOrder() []string {
	var jobs []string
	for _, name := range sortedKeys(p.Workloads) {
		if p.Workloads[name].IsJob() {
			jobs = append(jobs, name)
		}
	}
	if len(p.Deployment.Order) > 0 {
		if ordered := filterTo(p.Deployment.Order, jobs); len(ordered) == len(jobs) {
			return ordered
		}
	}
	return p.topological(jobs)
}

// JobOrderFor returns only jobs assigned to one automatic release phase while
// preserving the dependency order of the complete job graph. In particular,
// when="manual" is never used by deployment execution.
func (p *Spec) JobOrderFor(when string) []string {
	ordered := p.JobOrder()
	out := make([]string, 0, len(ordered))
	for _, name := range ordered {
		if p.Workloads[name].When == when {
			out = append(out, name)
		}
	}
	return out
}

// ServiceNames are the supporting services, sorted. They live in their own
// Compose projects and outlive any release.
func (p *Spec) ServiceNames() []string { return sortedKeys(p.Services) }

// topological orders the given workloads so every prerequisite precedes what
// needs it, breaking ties alphabetically so the result is stable. A cycle
// cannot reach here — the loader rejects one — but if it ever did, the
// remaining workloads are appended in name order rather than dropped.
func (p *Spec) topological(names []string) []string {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []string
	placed := map[string]bool{}
	for len(out) < len(names) {
		progressed := false
		for _, n := range names {
			if placed[n] {
				continue
			}
			ready := true
			for _, need := range p.Workloads[n].Needs {
				if want[need.Name] && !placed[need.Name] {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, n)
				placed[n] = true
				progressed = true
			}
		}
		if !progressed {
			for _, n := range names {
				if !placed[n] {
					out = append(out, n)
					placed[n] = true
				}
			}
		}
	}
	return out
}

// filterTo keeps the entries of order that appear in allowed, preserving the
// author's sequence and appending anything they left out.
func filterTo(order, allowed []string) []string {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range order {
		if ok[o] && !seen[o] {
			out = append(out, o)
			seen[o] = true
		}
	}
	for _, a := range allowed {
		if !seen[a] {
			out = append(out, a)
		}
	}
	return out
}

// ParseDuration accepts what the schema accepts, which is Go's syntax plus a
// whole-day suffix. Days exist because retention and backup windows are written
// in them and `168h` is a worse way to say a week.
func ParseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if days, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && strings.HasSuffix(s, "d") {
		// A day count that cannot be held in nanoseconds is not a long
		// duration, it is a wrapped one: it comes back negative, or — worse —
		// as a plausible positive that slips past every bound written as
		// `d > limit`. Refusing it is the only reading that cannot mislead.
		if days > maxDurationDays || days < -maxDurationDays {
			return 0, false
		}
		return time.Duration(days) * 24 * time.Hour, true
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d, true
}

// Environment looks up one environment by name, naming the alternatives when
// it is missing. A typo in `--env` is the single most common way to aim a
// deploy at nothing, and an error that lists what exists ends that in one
// round trip.
func (p *Spec) Environment(name string) (Environment, error) {
	e, ok := p.Environments[name]
	if !ok {
		return Environment{}, errf("unknown_environment", "environments."+name, "",
			"environment %q is not declared; the project declares: %s",
			name, strings.Join(sortedKeys(p.Environments), ", "))
	}
	return e, nil
}

// Target is the SSH destination for this environment, in the form the
// transport accepts.
func (e Environment) Destination() string {
	host := e.Server.Host
	// Bracketed so an IPv6 literal's own colons are not read as the port
	// separator. Without the port the transport silently uses 22, and a host
	// that only listens on 2222 reports itself unreachable.
	if e.Server.Port != 0 {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		host = fmt.Sprintf("%s:%d", host, e.Server.Port)
	}
	if e.Server.User != "" {
		return e.Server.User + "@" + host
	}
	return host
}

// ProbePort is the port an HTTP health check probes when it names none: the
// workload's own `port`, else the first route's, else the first published
// container port. A workload with several routes uses the first,
// which is the one the shorthand form would have described.
func (w Workload) ProbePort() int {
	if w.Port != 0 {
		return w.Port
	}
	for _, r := range w.NormalisedRoutes() {
		if r.Port != 0 {
			return r.Port
		}
	}
	for _, p := range w.PublishedPorts {
		if p.Container != 0 {
			return p.Container
		}
	}
	return 0
}

// ParsePostgresDuration reads the form PostgreSQL uses when it reports a
// setting — "15min", "1h", "300s", or a bare number meaning seconds — which is
// not the form ParseDuration accepts. It exists so a declared policy can be
// compared with what the server says it is doing.
func ParsePostgresDuration(value string) (time.Duration, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	digits := len(trimmed)
	for index, char := range trimmed {
		if char < '0' || char > '9' {
			digits = index
			break
		}
	}
	if digits == 0 {
		return 0, false
	}
	count, err := strconv.Atoi(trimmed[:digits])
	if err != nil || count < 0 {
		return 0, false
	}
	unit := strings.ToLower(strings.TrimSpace(trimmed[digits:]))
	switch unit {
	case "", "s":
		return time.Duration(count) * time.Second, true
	case "ms":
		return time.Duration(count) * time.Millisecond, true
	case "min":
		return time.Duration(count) * time.Minute, true
	case "h":
		return time.Duration(count) * time.Hour, true
	case "d":
		return time.Duration(count) * 24 * time.Hour, true
	}
	return 0, false
}

// maxDurationDays is the largest whole-day count that fits in int64
// nanoseconds: math.MaxInt64 / (24h in ns).
const maxDurationDays = 106751
