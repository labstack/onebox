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
// the proxy — the flip takes Retries × Interval — so the drain budget is
// derived from it rather than guessed alongside it.
func (w Workload) HealthRetries() int {
	if w.Health != nil && w.Health.Retries > 0 {
		return w.Health.Retries
	}
	return 3
}

// ReadyTiming is the health gate's overall budget and its poll cadence.
func (w Workload) ReadyTiming() (within, interval time.Duration) {
	within, interval = 120*time.Second, 2*time.Second
	if w.Health == nil {
		return within, interval
	}
	if d, ok := ParseDuration(w.Health.Within); ok && d > 0 {
		within = d
	}
	if d, ok := ParseDuration(w.Health.Interval); ok && d > 0 {
		interval = d
	}
	return within, interval
}

// DrainWait is how long to leave a container marked unhealthy before stopping
// it, so the proxy has time to notice and stop sending it traffic. Without an
// explicit value it is derived from the health timing, which is the only way
// the two cannot drift apart.
func (w Workload) DrainWait() time.Duration {
	if w.Drain != nil && w.Drain.Wait != "" {
		if d, ok := ParseDuration(w.Drain.Wait); ok {
			return d
		}
	}
	_, interval := w.ReadyTiming()
	return time.Duration(w.HealthRetries()) * interval
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

// JobOrder is the sequence jobs run in, before any long-running workload is
// touched. Migrations that depend on each other are ordered by `needs` like
// everything else.
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
func (e Environment) Target() string {
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
	for _, p := range w.Ports {
		if p.Container != 0 {
			return p.Container
		}
	}
	return 0
}
