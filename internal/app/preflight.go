package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/transport"
)

// Preflight is the phase that needs the target. Generation is local and pure;
// everything that requires asking the host — privileges, name collisions —
// happens here, after generation and before the first mutating command.
//
// The split exists because the two failures need different answers. A
// generation failure means the project is wrong and someone must edit a file. A
// preflight failure means the server is not ready and someone must fix the
// host. Reporting them the same way makes an agent guess.
//
// Nothing here mutates. Every command is a read.

// Runner is the subset of the transport this phase needs.
type Runner interface {
	Run(ctx context.Context, cmd string) (transport.Result, error)
}

// Check is one question asked of the target.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Remedy string `json:"remedy,omitempty"`
}

// Report is the outcome of asking all of them.
type Report struct {
	Env    string  `json:"environment"`
	Target string  `json:"target,omitempty"`
	Checks []Check `json:"checks"`
}

// OK reports whether every check passed.
func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Failures returns the checks that did not pass, for a caller that wants to
// report only what needs fixing.
func (r *Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// Preflight asks the target whether this project can be deployed. It returns an
// error only when the target cannot be reached at all; anything the target
// answers is a Check, so a caller sees every problem at once rather than the
// first one.
func (r *Resolved) Preflight(ctx context.Context, run Runner) (*Report, error) {
	p := r.Spec
	n := p.NamesFor(r.Env)
	report := &Report{Env: r.Env}

	// 1. The container runtime. Everything else is meaningless without it, so a
	// failure here short-circuits rather than producing a cascade.
	res, err := run.Run(ctx, "docker version --format '{{.Server.Version}}'")
	if err != nil {
		return nil, errf("target_unreachable", "", "ob doctor",
			"cannot reach the target: %v", err)
	}
	if res.ExitCode != 0 {
		report.Checks = append(report.Checks, Check{
			Name:   "container runtime",
			Detail: strings.TrimSpace(firstLine(res.Stderr)),
			Remedy: "install Docker on the target, or grant this account permission to use it",
		})
		return report, nil
	}
	report.Checks = append(report.Checks, Check{
		Name: "container runtime", OK: true,
		Detail: "docker " + strings.TrimSpace(res.Stdout),
	})

	// 2. The base path. Checked without creating anything: preflight that
	// mutates is not preflight.
	report.Checks = append(report.Checks, basePathCheck(ctx, run, n.BasePath))

	// 3. Name collisions. One listing per resource kind rather than one command
	// per name — a project with twenty derived names should not cost twenty
	// round trips.
	owned, err := ownedNames(ctx, run, p.App)
	if err != nil {
		return nil, err
	}
	report.Checks = append(report.Checks, collisionChecks(p.All(r.Env), owned)...)

	// 4. The ingress network, which the proxy owns and this project only joins.
	if p.Proxy.Managed && p.Proxy.Kind != "none" && p.routesAnywhere() {
		report.Checks = append(report.Checks, networkCheck(ctx, run, p.Proxy.Network))
	}

	return report, nil
}

func basePathCheck(ctx context.Context, run Runner, base string) Check {
	// Walk up to the nearest existing ancestor and test that it is writable.
	cmd := fmt.Sprintf(
		`p=%q; while [ ! -e "$p" ] && [ "$p" != "/" ]; do p=$(dirname "$p"); done; `+
			`[ -w "$p" ] && echo "$p" || { echo "$p"; exit 1; }`, base)
	res, err := run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		where := strings.TrimSpace(res.Stdout)
		return Check{
			Name:   "base path",
			Detail: fmt.Sprintf("%s is not writable by this account", where),
			Remedy: fmt.Sprintf("grant write access to %s, or set base_path to a directory this account owns", where),
		}
	}
	return Check{Name: "base path", OK: true, Detail: base}
}

// ownedNames lists the container, volume and network names already on the host,
// with whichever application owns each. A name held by this application is the
// normal case — a previous release — and only a foreign holder is a collision.
func ownedNames(ctx context.Context, run Runner, app string) (map[string]string, error) {
	owned := map[string]string{}

	for _, q := range []struct{ cmd, kind string }{
		{`docker ps -a --format '{{.Names}}\t{{.Label "ob.app"}}'`, "container"},
		{`docker volume ls --format '{{.Name}}\t{{.Label "ob.app"}}'`, "volume"},
		{`docker network ls --format '{{.Name}}\t{{.Label "ob.app"}}'`, "network"},
	} {
		res, err := run.Run(ctx, q.cmd)
		if err != nil {
			return nil, errf("target_unreachable", "", "",
				"cannot list %ss on the target: %v", q.kind, err)
		}
		if res.ExitCode != 0 {
			continue
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			name, owner, _ := strings.Cut(line, "\t")
			if name == "" {
				continue
			}
			// Later kinds must not clobber an earlier owner record.
			if prev, seen := owned[name]; seen && prev != "" {
				continue
			}
			owned[name] = strings.TrimSpace(owner)
		}
	}
	return owned, nil
}

func collisionChecks(derived []string, owned map[string]string) []Check {
	var conflicts []string
	for _, name := range derived {
		owner, exists := owned[name]
		if !exists {
			continue
		}
		if owner == "" {
			conflicts = append(conflicts, fmt.Sprintf("%s (held by a resource Onebox does not own)", name))
		}
		// A name held by this application is a previous release, not a conflict.
	}
	sort.Strings(conflicts)

	if len(conflicts) == 0 {
		return []Check{{Name: "name collisions", OK: true,
			Detail: fmt.Sprintf("%d derived names, none held by anything else", len(derived))}}
	}
	return []Check{{
		Name:   "name collisions",
		Detail: strings.Join(conflicts, ", "),
		Remedy: "rename or remove the existing resource, or change the identifier it collides with; Onebox will not adopt a resource it did not create",
	}}
}

func networkCheck(ctx context.Context, run Runner, network string) Check {
	res, err := run.Run(ctx, fmt.Sprintf("docker network inspect %s >/dev/null 2>&1", network))
	if err != nil || res.ExitCode != 0 {
		return Check{
			Name:   "ingress network",
			Detail: fmt.Sprintf("%s does not exist", network),
			Remedy: "run the proxy bootstrap so the ingress network exists before a workload joins it",
		}
	}
	return Check{Name: "ingress network", OK: true, Detail: network}
}

// RunPreflight checks every declared env-file assertion against the working
// tree. It runs before anything is transferred, because "the deploy succeeded
// and the app crash-looped on a missing key" is a worse way to learn the same
// fact.
//
// It reports every failure rather than the first. An operator fixing a missing
// key one round trip at a time is the reason people stop declaring them.
func (p *Spec) RunPreflight(dir string) error {
	if p.Runtime == nil {
		return nil
	}
	var missing []string
	for _, check := range p.Runtime.Preflight {
		path := check.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s: cannot be read (%v)", check.File, err))
			continue
		}
		present, nonEmpty := envKeys(data)
		for _, k := range check.Require {
			switch {
			case !present[k]:
				missing = append(missing, fmt.Sprintf("%s: %s is not declared", check.File, k))
			case !nonEmpty[k]:
				missing = append(missing, fmt.Sprintf("%s: %s is empty", check.File, k))
			}
		}
		// A `present` key may legitimately be empty — an optional feature
		// toggle declared and left off. Only its absence is a failure.
		for _, k := range check.Present {
			if !present[k] {
				missing = append(missing, fmt.Sprintf("%s: %s is not declared", check.File, k))
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return errf("preflight_env_incomplete", "runtime.preflight", "",
		"the environment is not ready:\n  %s", strings.Join(missing, "\n  "))
}

// envKeys scans dotenv-style bytes into the set of declared keys and the subset
// with a non-empty value. A line is `KEY=value`, tolerating leading whitespace
// and an `export ` prefix; anything else is ignored.
func envKeys(data []byte) (present, nonEmpty map[string]bool) {
	present, nonEmpty = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		present[strings.TrimSpace(line[:eq])] = true
		if strings.TrimSpace(line[eq+1:]) != "" {
			nonEmpty[strings.TrimSpace(line[:eq])] = true
		}
	}
	return present, nonEmpty
}
