package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
	"github.com/labstack/onebox/internal/ui"
)

// ob ls — host-wide overview. Unlike Status (one app, config-aware), this is a
// config-free inventory of every app on the host, derived from three concurrent
// host reads: a host-wide docker ps, the /<root>/<app>/current symlinks, and
// the proxy's app registry. It reads no per-app ob.yml, so it reports only what
// the host and Docker themselves know.

type appState int

const (
	stateInSync appState = iota
	stateNotRunning        // recorded a release, but nothing is running
	stateNeverActivated    // an app dir exists but was never activated (no current)
	stateRunningUnrecorded // containers run but there's no current symlink
	stateDiverged          // a running container is off the recorded release, or unhealthy
)

func (s appState) problem() bool {
	return s == stateNotRunning || s == stateDiverged || s == stateRunningUnrecorded
}

// AppRow is one app's line in the host overview.
type AppRow struct {
	App        string
	Recorded   string // recorded release id, or "" if never activated
	Running    int    // running container count
	Health     string // aggregate: healthy | "N unhealthy" | "N starting" | none | -
	Proxied    bool
	Incomplete bool // an unfinished deploy (only set with ListOptions.Incomplete)
	State      appState
}

func (r AppRow) Problem() bool { return r.State.problem() }

// StateLabel is the human-facing STATE cell.
func (r AppRow) StateLabel() string {
	switch r.State {
	case stateInSync:
		return "in sync"
	case stateNotRunning:
		return "NOT RUNNING"
	case stateNeverActivated:
		return "never activated"
	case stateRunningUnrecorded:
		return "running (unrecorded)"
	case stateDiverged:
		return "DIVERGED"
	default:
		return "?"
	}
}

// StateKey is the machine-facing STATE token (for --json).
func (r AppRow) StateKey() string {
	switch r.State {
	case stateInSync:
		return "in_sync"
	case stateNotRunning:
		return "not_running"
	case stateNeverActivated:
		return "never_activated"
	case stateRunningUnrecorded:
		return "running_unrecorded"
	case stateDiverged:
		return "diverged"
	default:
		return "unknown"
	}
}

// ProxySummary is the shared managed proxy's one-line health.
type ProxySummary struct {
	Managed bool // the ob-proxy project is present on the host
	Running bool
	Health  string
}

// HostOverview is the whole-host picture ob ls renders. Apps are alphabetical
// (stable for --json); the table renderer re-sorts problems-first.
type HostOverview struct {
	Proxy   ProxySummary
	Apps    []AppRow
	Foreign int // running compose projects ob doesn't manage
}

// HasProblems reports whether any app is not running or diverged (for --fail-on-drift).
func (o HostOverview) HasProblems() bool {
	for _, a := range o.Apps {
		if a.State.problem() {
			return true
		}
	}
	return false
}

type ListOptions struct {
	Incomplete bool
}

// HostList runs the host reads in one concurrent wave (behind a spinner) and
// derives the overview. It takes a bare transport, not an Engine, so both entry
// points (an ambient ob.yml or --host) can drive it without an app config.
func HostList(ctx context.Context, t transport.Transport, u *ui.UI, opts ListOptions) (HostOverview, error) {
	var (
		byProject  map[string][]svcContainer
		recorded   map[string]string
		proxied    map[string]bool
		incomplete map[string]bool
	)
	reads := []func() error{
		func() (err error) { byProject, err = hostContainers(ctx, t); return },
		func() (err error) { recorded, err = recordedReleases(ctx, t); return },
		func() (err error) { proxied, err = proxyRegisteredApps(ctx, t); return },
	}
	if opts.Incomplete {
		reads = append(reads, func() (err error) {
			incomplete, err = journal.HostIncomplete(ctx, t, release.Root())
			return
		})
	}

	_, stop := u.Busy("querying " + t.Host())
	err := gather(reads...)
	stop()
	if err != nil {
		return HostOverview{}, err
	}

	ov := HostOverview{}
	if pc := byProject[proxy.Project]; len(pc) > 0 {
		ov.Proxy = ProxySummary{Managed: true, Running: true, Health: pc[0].health}
	}
	// foreign = a running project that is neither an app (has a dir) nor the proxy
	for project := range byProject {
		if project == proxy.Project {
			continue
		}
		if _, isApp := recorded[project]; !isApp {
			ov.Foreign++
		}
	}
	// one row per app dir (the source of truth), alphabetical
	apps := make([]string, 0, len(recorded))
	for app := range recorded {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		ov.Apps = append(ov.Apps, buildRow(app, recorded[app], byProject[app], proxied[app], incomplete[app]))
	}
	return ov, nil
}

func buildRow(app, recorded string, cs []svcContainer, proxied, incomplete bool) AppRow {
	row := AppRow{App: app, Recorded: recorded, Running: len(cs), Proxied: proxied, Incomplete: incomplete, Health: healthSummary(cs)}
	switch {
	case recorded == "" && len(cs) == 0:
		row.State = stateNeverActivated
	case recorded == "":
		row.State = stateRunningUnrecorded
	case len(cs) == 0:
		row.State = stateNotRunning
	default:
		row.State = stateInSync
		for _, c := range cs {
			// Release drift applies only to release-labeled (role) containers —
			// accessories and foreign containers legitimately carry no ob.release,
			// so an empty label is "running", not drift. An unhealthy container of
			// any kind is worth surfacing on a host overview. (ob ls can't know a
			// role is *expected* but missing — that needs the config; use status.)
			if (c.release != "" && c.release != recorded) || (c.health != "healthy" && c.health != "none") {
				row.State = stateDiverged
				break
			}
		}
	}
	return row
}

// healthSummary collapses a service's replicas to one word, worst-first:
// unhealthy > starting > healthy > none.
func healthSummary(cs []svcContainer) string {
	if len(cs) == 0 {
		return "-"
	}
	var unhealthy, starting, healthy int
	for _, c := range cs {
		switch c.health {
		case "unhealthy":
			unhealthy++
		case "starting":
			starting++
		case "healthy":
			healthy++
		}
	}
	switch {
	case unhealthy > 0:
		return fmt.Sprintf("%d unhealthy", unhealthy)
	case starting > 0:
		return fmt.Sprintf("%d starting", starting)
	case healthy > 0:
		return "healthy"
	default:
		return "none"
	}
}

// hostContainers is read 1: every running container on the host, keyed by
// compose project (the ob-proxy project is kept — it feeds the proxy summary).
func hostContainers(ctx context.Context, t transport.Transport) (map[string][]svcContainer, error) {
	res, err := t.Run(ctx,
		"docker ps --format '{{.ID}}|{{.Label \"com.docker.compose.project\"}}|{{.Label \"com.docker.compose.service\"}}|{{.Label \"ob.release\"}}|{{.Status}}'")
	if err != nil {
		return nil, err
	}
	byProject := map[string][]svcContainer{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 5)
		if len(parts) != 5 || parts[1] == "" {
			continue // no compose project → not an ob-managed container
		}
		id := parts[0]
		if !validID.MatchString(id) {
			return nil, fmt.Errorf("suspicious container id %q from docker ps — refusing to reuse in a command", id)
		}
		byProject[parts[1]] = append(byProject[parts[1]], svcContainer{id: id, release: parts[3], health: healthFromStatus(parts[4])})
	}
	return byProject, nil
}

// recordedReleases is read 2: app -> recorded release, from each <root>/<app>/
// current symlink. Source of truth for which apps exist. POSIX-safe (no
// nullglob) and empty-host-safe. The base root honors OB_BASE_DIR.
func recordedReleases(ctx context.Context, t transport.Transport) (map[string]string, error) {
	root := release.Root()
	cmd := "cd " + q(root) + " 2>/dev/null && for a in $(ls -1 2>/dev/null); do " +
		"[ \"$a\" = _host ] && continue; [ -d \"$a\" ] || continue; " +
		"printf '%s|%s\\n' \"$a\" \"$(readlink \"$a/current\" 2>/dev/null)\"; done || true"
	res, err := t.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		app, target, _ := strings.Cut(line, "|")
		if !appNameRe.MatchString(app) {
			continue // reject a hostile / non-app directory name
		}
		rec := strings.TrimSpace(target) // "releases/<id>" or ""
		if i := strings.LastIndex(rec, "/"); i >= 0 {
			rec = rec[i+1:]
		}
		out[app] = rec
	}
	return out, nil
}

// proxyRegisteredApps is read 3: the apps registered behind the managed proxy.
func proxyRegisteredApps(ctx context.Context, t transport.Transport) (map[string]bool, error) {
	res, err := t.Run(ctx, "ls -1 "+q(proxy.HostPaths().Apps)+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, f := range strings.Fields(res.Stdout) {
		if appNameRe.MatchString(f) {
			set[f] = true
		}
	}
	return set, nil
}
