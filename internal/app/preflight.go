package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"bytes"
	"github.com/labstack/onebox/internal/shellquote"
	"github.com/labstack/onebox/internal/transport"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// Preflight is the phase that needs the server. Generation is local and pure;
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

// Check is one question asked of the server.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Remedy string `json:"remedy,omitempty"`
}

// Report is the outcome of asking all of them.
type Report struct {
	Env    string  `json:"environment"`
	Server string  `json:"server,omitempty"`
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

// Preflight asks the server whether this project can be deployed. It returns an
// error only when the server cannot be reached at all; anything the server
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
		return nil, errf("server_unreachable", "", "ob doctor",
			"cannot reach the server: %v", err)
	}
	if res.ExitCode != 0 {
		report.Checks = append(report.Checks, Check{
			Name:   "container runtime",
			Detail: strings.TrimSpace(firstLine(res.Stderr)),
			Remedy: "install Docker on the server, or grant this account permission to use it",
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
	report.Checks = append(report.Checks, hostOwnerCheck(ctx, run, n.HostOwnerPath(), p.Name, r.Env))

	// 3. Name collisions. One listing per resource kind rather than one command
	// per name — a project with twenty derived names should not cost twenty
	// round trips.
	owned, err := ownedNames(ctx, run, p, r.Env)
	if err != nil {
		return nil, err
	}
	report.Checks = append(report.Checks, collisionChecks(p.Name, p.All(r.Env), owned)...)

	// 4. The ingress network, which the proxy owns and this project only joins.
	if p.Proxy.Kind != "none" && p.routesAnywhere() {
		report.Checks = append(report.Checks, networkCheck(ctx, run, p.Proxy.Network))
	}

	return report, nil
}

// hostOwnerCheck answers "can this application own this host", which is not the
// same question as "is it already bootstrapped". An unclaimed host passes:
// preflight exists to be run *before* the first bootstrap, and failing there
// would mean the only way to satisfy the check is the mutation it precedes.
//
// The read is exit-code aware rather than `|| true`, so an owner record that
// exists but cannot be read is never reported as an unclaimed host — the two
// answers lead to opposite actions, and the wrong one adopts a machine that
// already belongs to somebody else.
func hostOwnerCheck(ctx context.Context, run Runner, path, application, environment string) Check {
	// The same probe the engine's readHostOwner uses, including the
	// regular-file and symlink guards.
	res, err := run.Run(ctx, HostOwnerProbe(path))
	if err != nil {
		return Check{Name: "host owner", Detail: "could not read the host owner record", Remedy: "verify target access, then retry"}
	}
	if res.ExitCode == ProbeAbsent {
		return Check{Name: "host owner", OK: true, Detail: "unclaimed — ob bootstrap will claim it for " + application + "/" + environment}
	}
	// Absence that could not be established is not absence. Passing this as
	// unclaimed is how one application adopts a host that already has an owner.
	if res.ExitCode == ProbeUndetermined {
		return Check{
			Name:   "host owner",
			Detail: "the host state directory cannot be searched, so an owner record cannot be ruled out",
			Remedy: "verify access to the host state directory, then retry",
		}
	}
	if res.ExitCode == ProbeStatePathNotDirectory {
		return Check{
			Name:   "host owner",
			Detail: "the path that should hold the host owner record is not a directory",
			Remedy: "inspect the host state directory; no permission change fixes a file where the directory belongs",
		}
	}
	if res.ExitCode == ProbeNotRegular {
		return Check{
			Name:   "host owner",
			Detail: "the host owner record is not a regular file",
			Remedy: "inspect the host state directory; only a regular file is a valid owner record",
		}
	}
	// 2 is the probe's own refusal; 1 is cat failing on a record it could not
	// read for some other reason. Both are facts about the record.
	if res.ExitCode == ProbeUnreadable || res.ExitCode == 1 {
		return Check{
			Name:   "host owner",
			Detail: "the host owner record exists but could not be read",
			Remedy: "verify target access and the record's permissions, then retry",
		}
	}
	// Anything else is the command failing rather than a fact about the
	// record — no shell, a transport reporting a remote status without a Go
	// error — and reporting that as an unreadable record is a diagnosis
	// preflight never made.
	if res.ExitCode != 0 {
		return Check{
			Name:   "host owner",
			Detail: fmt.Sprintf("the host owner record could not be checked (exit %d)", res.ExitCode),
			Remedy: "verify target access and that a POSIX shell is available, then retry",
		}
	}
	record := strings.TrimSpace(res.Stdout)
	if record == "" {
		return Check{
			Name:   "host owner",
			Detail: "the host owner record is present but empty",
			Remedy: "inspect the host state directory; an empty record is not a valid claim",
		}
	}
	// The engine's parser, not a second reading of the same file. Preflight
	// exists to predict what the engine will do, so anything it accepts that the
	// engine rejects is a green light for a refused deploy.
	owner, ok := ParseHostOwnerRecord(record)
	if !ok {
		return Check{
			Name:   "host owner",
			Detail: "the host owner record is present but is not a record Onebox wrote",
			Remedy: "inspect it on the host; no ob command repairs an unparseable record, so remove it and run ob bootstrap",
		}
	}
	if owner.Application != application {
		return Check{Name: "host owner", Detail: fmt.Sprintf("host is owned by application %s", owner.Application), Remedy: "choose an unowned host; Onebox supports one application owner per host"}
	}
	if owner.Legacy() {
		return Check{Name: "host owner", OK: true, Detail: application + " (claimed before environments were recorded; ob bootstrap will complete it)"}
	}
	if owner.Environment != environment {
		// Every runtime name is application-scoped, so a second environment on
		// this host would reuse the first one's containers and volumes rather
		// than collide with them. Nothing downstream can see the difference.
		return Check{
			Name:   "host owner",
			Detail: fmt.Sprintf("host is claimed by the %s environment of %s", owner.Environment, application),
			Remedy: "choose a host for this environment; two environments on one host would share container and volume names",
		}
	}
	return Check{Name: "host owner", OK: true, Detail: application + "/" + environment}
}

func basePathCheck(ctx context.Context, run Runner, base string) Check {
	// Walk up to the nearest ancestor we can see and test that it is usable.
	//
	// -e follows symlinks, so the walk has to stop at a link it cannot
	// resolve as well as at a real directory: without -L a dangling
	// base_path is invisible, the walk climbs past it to a writable
	// /var/lib, and preflight answers "base path OK" for a path whose first
	// mkdir fails with "File exists" — the false absence this whole contract
	// exists to refuse.
	cmd := fmt.Sprintf(
		`p=%q; while [ ! -e "$p" ] && [ ! -L "$p" ] && [ "$p" != "/" ]; do p=$(dirname "$p"); done; `+
			`echo "$p"; `+
			`if [ -L "$p" ] && [ ! -e "$p" ]; then exit %d; fi; `+
			`if [ ! -d "$p" ]; then exit %d; fi; `+
			`( cd "$p" ) 2>/dev/null || exit %d; `+
			`[ -w "$p" ] || exit 1`,
		base, ProbeNotRegular, ProbeStatePathNotDirectory, ProbeUndetermined)
	res, err := run.Run(ctx, cmd)
	if err != nil {
		return Check{Name: "base path", Detail: "could not read the base path", Remedy: "verify target access, then retry"}
	}
	where := strings.TrimSpace(res.Stdout)
	switch res.ExitCode {
	case 0:
		return Check{Name: "base path", OK: true, Detail: base}
	case ProbeNotRegular:
		return Check{
			Name:   "base path",
			Detail: fmt.Sprintf("%s is a symlink whose target does not exist", where),
			Remedy: fmt.Sprintf("repair or remove %s; ob will not create a base path through a broken link", where),
		}
	case ProbeStatePathNotDirectory:
		return Check{
			Name:   "base path",
			Detail: fmt.Sprintf("%s is not a directory", where),
			Remedy: fmt.Sprintf("remove %s, or set base_path somewhere ob can create a directory", where),
		}
	case ProbeUndetermined:
		return Check{
			Name:   "base path",
			Detail: fmt.Sprintf("%s cannot be searched, so its contents could not be checked", where),
			Remedy: fmt.Sprintf("grant this account access to %s, then retry", where),
		}
	}
	if res.ExitCode == 1 {
		return Check{
			Name:   "base path",
			Detail: fmt.Sprintf("%s is not writable by this account", where),
			Remedy: fmt.Sprintf("grant write access to %s, or set base_path to a directory this account owns", where),
		}
	}
	// The probe emits 0, 1, 4, 5 and 6 and nothing else, so any other status
	// is the command itself failing — no shell, a transport reporting a remote
	// status without a Go error. Reusing the not-writable sentence there names
	// a cause preflight never observed, with an empty path where the offending
	// directory should be.
	return Check{
		Name:   "base path",
		Detail: fmt.Sprintf("the base path could not be checked (exit %d)", res.ExitCode),
		Remedy: "verify target access and that a POSIX shell is available, then retry",
	}
}

// ownedNames lists the container, volume and network names already on the host,
// with whichever application owns each. A name held by this application is the
// normal case — a previous release — and only a foreign holder is a collision.
func ownedNames(ctx context.Context, run Runner, project *Spec, environment string) (map[string]string, error) {
	owned := map[string]string{}
	application := project.Name
	n := project.NamesFor(environment)
	legacyServiceState := false
	if len(project.Services) > 0 {
		res, err := run.Run(ctx, "test -d "+shellquote.Quote(n.ServiceDir()))
		if err != nil {
			return nil, errf("server_unreachable", "", "", "cannot inspect legacy service-network ownership: %v", err)
		}
		legacyServiceState = res.ExitCode == 0
	}

	for _, q := range []struct {
		cmd, kind      string
		composeProject bool
	}{
		{`docker ps -a --format '{{.Names}}\t{{.Label "ob.app"}}'`, "container", false},
		{`docker volume ls --format '{{.Name}}\t{{.Label "ob.app"}}'`, "volume", false},
		{`docker network ls --format '{{.Name}}\t{{.Label "ob.app"}}\t{{.Label "com.docker.compose.project"}}'`, "network", true},
	} {
		res, err := run.Run(ctx, q.cmd)
		if err != nil {
			return nil, errf("server_unreachable", "", "",
				"cannot list %ss on the server: %v", q.kind, err)
		}
		if res.ExitCode != 0 {
			continue
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			fields := strings.SplitN(line, "\t", 3)
			name := strings.TrimSpace(fields[0])
			if name == "" {
				continue
			}
			owner := ""
			if len(fields) > 1 {
				owner = strings.TrimSpace(fields[1])
			}
			// Before Onebox labelled networks, Compose still labelled the
			// application default with its project. That is sufficient migration
			// evidence for this exact application, but not for a hand-created
			// network with only the derived name.
			if owner == "" && q.composeProject && name == n.ApplicationNetwork() && len(fields) > 2 && strings.TrimSpace(fields[2]) == application {
				owner = application
			}
			// Durable service state proves only an observed legacy service
			// network. Applying it after all resource kinds are merged would also
			// bless an unlabelled container or volume with the same name.
			if owner == "" && q.kind == "network" && name == n.ServiceNetwork() && legacyServiceState {
				owner = application
			}
			// Docker permits the same name in different resource kinds. Every
			// holder must belong to this application: one foreign or unlabelled
			// holder is a collision even if another kind is app-owned.
			if prev, seen := owned[name]; seen {
				if prev != application {
					continue
				}
				if owner != application {
					owned[name] = owner
				}
				continue
			}
			owned[name] = owner
		}
	}

	return owned, nil
}

func collisionChecks(application string, derived []string, owned map[string]string) []Check {
	var conflicts []string
	for _, name := range derived {
		owner, exists := owned[name]
		if !exists {
			continue
		}
		if owner == "" {
			conflicts = append(conflicts, fmt.Sprintf("%s (held by a resource Onebox does not own)", name))
		} else if owner != application {
			conflicts = append(conflicts, fmt.Sprintf("%s (owned by application %s)", name, owner))
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
	for _, check := range p.Runtime.EnvChecks {
		path := check.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s: cannot be read (%v)", check.File, err))
			continue
		}
		present, nonEmpty := envKeys(data, p.envContextBefore(dir, check.File))
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
	return errf("preflight_env_incomplete", "runtime.env_checks", "",
		"the environment is not ready:\n  %s", strings.Join(missing, "\n  "))
}

// envKeys scans dotenv-style bytes into the set of declared keys and the subset
// with a non-empty value. A line is `KEY=value`, tolerating leading whitespace
// and an `export ` prefix; anything else is ignored.
// envKeys reports what a file declares, resolved against the values the files
// declared before it already established.
//
// Without that context a value such as `API_TOKEN=${ROOT}` resolves to empty,
// and preflight rejects an environment the container runtime would have
// assembled correctly — the opposite failure to the one preflight exists to
// prevent.
func envKeys(data []byte, context map[string]string) (present, nonEmpty map[string]bool) {
	present, nonEmpty = map[string]bool{}, map[string]bool{}
	// Compose's parser, so preflight agrees with the runtime about what the
	// file declares. A hand-rolled scan counted `#API_TOKEN=x` as a declared
	// key and reported an environment ready that was not.
	values, err := dotenv.ParseWithLookup(bytes.NewReader(data), func(key string) (string, bool) {
		v, ok := context[key]
		return v, ok
	})
	if err != nil {
		return present, nonEmpty
	}
	for key, value := range values {
		present[key] = true
		if strings.TrimSpace(value) != "" {
			nonEmpty[key] = true
		}
	}
	return present, nonEmpty
}

// InterpolationEnv is the variable set available to `${VAR}` expressions in a
// Compose source the project references.
//
// Only the project-wide `runtime.env_files` feed it. Interpolation is a
// property of the document, not of a container: a per-workload file cannot
// coherently supply it, because one workload's file would then decide what
// another workload's copied service parses. Declared order wins, later over
// earlier, which is the same rule the files themselves follow when projected.
//
// The values never reach the generated runtime. A referenced source keeps its
// `${VAR}` verbatim — interpolation is that file's own contract — so what is
// resolved here is what the parser needs to read the document, not what is
// written into the artifact or its digest.
//
// The parsing is Compose's own. A hand-written dotenv scanner is close enough
// to look right and wrong in the places that matter — comments, quoting,
// escapes, and one variable expanding into another — and this contract is
// specifically that the same files mean the same thing here and when the
// container runtime reads them on the server. Same files, same parser.
func (p *Spec) InterpolationEnv() (map[string]string, error) {
	if len(p.documentScopeEntries()) == 0 {
		return nil, nil
	}
	// Plaintext entries only. An encrypted entry is ciphertext on disk, and
	// feeding it to a dotenv parser yields nothing useful — the guard exists on
	// the sibling helper below and was missing here, so a project mixing an
	// encrypted entry with a Compose reference interpolated from garbage.
	//
	// Plaintext entries only. Ciphertext handed to a dotenv parser puts
	// `ENC[AES256_GCM,…]` into the interpolation environment, and skipping an
	// entry silently resolves its variables empty. Neither is acceptable, so
	// what is unreadable here is reported by the caller when — and only when —
	// a variable actually goes unsupplied. Refusing eagerly was worse than
	// both: a project declaring an encrypted entry and referencing no Compose
	// file needs no interpolation at all, and stopped loading.
	paths := make([]string, 0, len(p.documentScopeEntries()))
	for _, entry := range p.documentScopeEntries() {
		if entry.Encrypted() {
			continue
		}
		paths = append(paths, filepath.Join(p.Dir, entry.File))
	}
	env, err := dotenv.GetEnvFromFile(map[string]string{}, paths)
	if err != nil {
		return nil, errf("env_file_unreadable", "runtime.env_files", "",
			"cannot read the environment files: %v", err)
	}
	return env, nil
}

// envContextBefore is the environment the declared files preceding this one
// establish, in declared order.
//
// A file that is not itself declared in runtime.env_files sees all of them:
// preflight is asserting that the environment as a whole is ready, and there
// is no position in the order from which to exclude anything.
func (p *Spec) envContextBefore(dir, file string) map[string]string {
	if len(p.documentScopeEntries()) == 0 {
		return nil
	}
	var preceding []string
	for _, entry := range p.documentScopeEntries() {
		if entry.File == file {
			break
		}
		preceding = append(preceding, filepath.Join(dir, entry.File))
	}
	if len(preceding) == 0 {
		return nil
	}
	env, err := dotenv.GetEnvFromFile(map[string]string{}, preceding)
	if err != nil {
		// An unreadable declared file is reported by the check that names it;
		// failing here would replace that with a worse message.
		return nil
	}
	return env
}

// documentScopeEntries is the list that feeds interpolation: the environment's
// if it declares one, otherwise the project's.
//
// Interpolation is a property of the document, so a workload's own list can
// never feed it — that would let one workload decide how another workload's
// copied service parses. The environment counts as document scope because the
// runtime is rendered per environment already, and any other reading has a
// container's values disagreeing with the document that parsed them.
func (p *Spec) documentScopeEntries() []EnvFile {
	if p.envDefault != nil {
		return p.envDefault
	}
	if p.Runtime == nil {
		return nil
	}
	return p.Runtime.EnvFiles
}

// EncryptedDocumentEntries names the document-scope entries this command cannot
// read, so a failure to interpolate can say where the value might have been.
//
// The contract requires an unsupplied variable to name the entry rather than
// resolve empty. It cannot be named at the point the values are gathered,
// because nothing there knows whether any variable needs it — only the parse
// knows that. So the fact travels, and the caller joins it to the failure.
func (p *Spec) EncryptedDocumentEntries() []string {
	var out []string
	for _, entry := range p.documentScopeEntries() {
		if entry.Encrypted() {
			out = append(out, entry.File)
		}
	}
	return out
}
