package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// Status prints recorded vs actual per role — divergence is the point
// of divergence. Recorded = the current symlink; actual = what each role's
// container says via its ob.release label and health.
//
// The host is high-latency and every command is a full SSH round trip (the
// docker work itself is negligible), so status is round-trip-bound. It fires
// every read concurrently in one wave — SSH multiplexes channels over the
// single connection, so their latencies overlap instead of summing — behind a
// spinner, since nothing streams until the whole table renders at the end.
func (e *Engine) Status(ctx context.Context) error {
	managed := e.Spec.Proxy.Managed
	var (
		recorded  string
		byService map[string][]svcContainer
		incS      journal.Summary
		incFound  bool
		px        proxyRaw
		schedules []StatusSchedule
	)

	reads := []func() error{
		func() (err error) { recorded, err = release.Current(ctx, e.T, e.names()); return },
		func() (err error) { byService, err = e.projectContainers(ctx); return },
		func() error {
			s, err := e.FindIncomplete(ctx)
			if errors.Is(err, ErrNoIncomplete) {
				return nil // a clean journal is not an error
			}
			if err != nil {
				return err // a read failure must NOT be mistaken for "clean"
			}
			incS, incFound = s, true
			return nil
		},
		func() (err error) { schedules, err = e.scheduleStatuses(ctx); return },
	}
	if managed {
		reads = append(reads, e.proxyReads(ctx, &px)...)
	}

	_, stop := e.ui.Busy("querying " + e.T.Host())
	err := gather(reads...)
	var expectedRevisions map[string]string
	if err == nil {
		recordedRelease := strings.TrimSpace(recorded)
		expectedRevisions, err = e.statusWorkloadRevisions(ctx, recordedRelease, byService)
	}
	stop()
	if err != nil {
		return err
	}

	recordedRelease := strings.TrimSpace(recorded)

	if recordedRelease == "" {
		recorded = "(none — never deployed)"
	} else {
		recorded = recordedRelease
	}
	fmt.Fprintf(e.Opts.Out, "app:      %s @ %s\n", e.Spec.Name, e.T.Host())
	fmt.Fprintf(e.Opts.Out, "recorded: %s\n", recorded)
	revision := e.Opts.Runner.VCSRevision
	if revision == "" {
		revision = "unknown"
	} else if len(revision) > 12 {
		revision = revision[:12]
	}
	dirty := ""
	if e.Opts.Runner.Dirty {
		dirty = "+dirty"
	}
	fmt.Fprintf(e.Opts.Out, "runner:   ob %s (%s%s)\n\n", e.Opts.Runner.Version, revision, dirty)
	// Sized to the identifiers actually present. A release identifier is
	// longer than any fixed width worth reading, and a column that overflows
	// pushes every later column out of line, which is how a status table stops
	// being scannable at exactly the moment someone is scanning it.
	release := len("ACTUAL RELEASE")
	for _, cs := range byService {
		for _, c := range cs {
			if len(c.release) > release {
				release = len(c.release)
			}
		}
	}
	row := fmt.Sprintf("%%-12s %%-10s %%-%ds %%-10s %%s", release)
	e.ui.Println(e.ui.Bold(fmt.Sprintf(row, "ROLE", "MODE", "ACTUAL RELEASE", "HEALTH", "STATE")))

	diverged := false
	for _, roleName := range e.Spec.ReleaseOrder() {
		role := e.Spec.Workloads[roleName]
		cs := byService[roleName]
		if len(cs) == 0 {
			diverged = true
			e.ui.Println(fmt.Sprintf(row, roleName, role.Mode(), "-", "-", e.ui.Warn("NOT RUNNING ⚠")))
			continue
		}
		for _, c := range cs {
			actual := c.release
			if actual == "" || actual == "<no value>" {
				actual = "(not ob-deployed)"
			}
			state := e.ui.OK("in sync")
			if !workloadReleaseMatches(c.release, c.revision, recordedRelease, expectedRevisions[roleName]) {
				state = e.ui.Warn("DIVERGED ⚠")
				diverged = true
			}
			if c.health != "healthy" && c.health != "none" {
				state += e.ui.Warn(" (" + c.health + ")")
				diverged = true
			}
			e.ui.Println(fmt.Sprintf(row, roleName, role.Mode(), actual, c.health, state))
		}
		// Running fewer replicas than the project declares is the shortfall a
		// human reads this to find. Counting only the containers that exist
		// meant a stopped replica vanished from the table and the report said
		// everything was in sync, while the structured output of the same
		// command called it divergence.
		if want := role.Count(); len(cs) != want {
			diverged = true
			e.ui.Println(fmt.Sprintf(row, roleName, role.Mode(), "-", "-",
				e.ui.Warn(fmt.Sprintf("REPLICAS %d/%d ⚠", len(cs), want))))
		}
	}
	for _, orphan := range e.statusOrphans(byService) {
		diverged = true
		for _, container := range orphan.Containers {
			actual := container.Release
			if actual == "" || actual == "<no value>" {
				actual = "(not ob-deployed)"
			}
			e.ui.Println(fmt.Sprintf(row, orphan.Service, "orphan", actual, container.Health, e.ui.Warn("UNDECLARED ⚠")))
		}
	}

	// services: running/health only — they converge separately, so an
	// unhealthy/starting one is shown but not a divergence. Absent (NOT RUNNING)
	// and present-but-not-serving ("down": crash-looping/paused) are both real
	// problems — a fully-exited service already diverged, so a crash-looping
	// one must too.
	fmt.Fprintln(e.Opts.Out)
	for _, acc := range e.Spec.ServiceNames() {
		cs := byService[acc]
		if len(cs) == 0 {
			e.ui.Println(fmt.Sprintf("service %-12s %s", acc, e.ui.Warn("NOT RUNNING ⚠")))
			diverged = true
			continue
		}
		if cs[0].health == "down" {
			e.ui.Println(fmt.Sprintf("service %-12s %s", acc, e.ui.Warn("down ⚠")))
			diverged = true
			continue
		}
		fmt.Fprintf(e.Opts.Out, "service %-12s %s\n", acc, cs[0].health)
	}
	for _, schedule := range schedules {
		if schedule.Diverged {
			diverged = true
			e.ui.Println(fmt.Sprintf("schedule %-11s %s", schedule.Name, e.ui.Warn(strings.Join(schedule.Issues, "; ")+" ⚠")))
			continue
		}
		result := schedule.LastResult
		if result == "" {
			result = "not run yet"
		}
		if schedule.Running {
			detail := fmt.Sprintf("running; policy: %s; timeout: %s", schedule.DeployLock, schedule.Timeout)
			if schedule.PinnedRelease != "" {
				detail += fmt.Sprintf("; release: %s; started: %s", schedule.PinnedRelease, schedule.StartedAt)
			}
			fmt.Fprintf(e.Opts.Out, "schedule %-11s %s\n", schedule.Name, detail)
			continue
		}
		fmt.Fprintf(e.Opts.Out, "schedule %-11s active; policy: %s; timeout: %s; last: %s\n",
			schedule.Name, schedule.DeployLock, schedule.Timeout, result)
	}

	if managed {
		fmt.Fprintln(e.Opts.Out)
		d, err := e.renderProxy(px)
		if err != nil {
			return err
		}
		diverged = diverged || d
	}

	// an unfinished deploy is the loudest divergence there is
	if incFound {
		diverged = true
		fmt.Fprintln(e.Opts.Out)
		e.warnf("INCOMPLETE deploy %s (started %s by %s) — `ob resume` or `ob abort`",
			incS.DeployID, incS.StartedAt, incS.Operator)
	}
	if diverged {
		return fmt.Errorf("status: divergence detected")
	}
	fmt.Fprintln(e.Opts.Out)
	e.ui.Successf("all in sync")
	return nil
}

// statusWorkloadRevisions reads the active release's own runtime contract only
// when an older release label could be a retained workload. The active Compose
// is the authority: a local inspection render can contain an authored tag or an
// unresolved build placeholder, while the deployed revision contains the image
// digest the plan pinned.
func (e *Engine) statusWorkloadRevisions(ctx context.Context, recorded string, byService map[string][]svcContainer) (map[string]string, error) {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" || !e.hasRetainedReleaseCandidate(byService, recorded) {
		return nil, nil
	}
	composePath := release.PathsFor(e.names()).Releases + "/" + recorded + "/compose.yaml"
	return e.releaseWorkloadRevisions(ctx, composePath)
}

func (e *Engine) hasRetainedReleaseCandidate(byService map[string][]svcContainer, recorded string) bool {
	for _, name := range e.Spec.ReleaseOrder() {
		for _, container := range byService[name] {
			if container.release != "" && container.release != recorded && container.revision != "" {
				return true
			}
		}
	}
	return false
}

func workloadReleaseMatches(containerRelease, containerRevision, recorded, expectedRevision string) bool {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" || containerRelease == "" {
		return false
	}
	return containerRelease == recorded || expectedRevision != "" && containerRevision == expectedRevision
}

// gather runs order-independent reads concurrently over the transport (SSH
// multiplexes many channels on one connection) and joins their errors. Wall
// time collapses from the sum of the round trips to the slowest single one.
func gather(fns ...func() error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(fns))
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			errs[i] = fn()
		}(i, fn)
	}
	wg.Wait()
	return errors.Join(errs...)
}
