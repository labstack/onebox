package app

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Derived names are contract. Once a volume exists its name can never change
// without moving data, so every pattern here is fixed and pinned by a golden
// test. Runtime containers use the human-facing <app>-<component>-<replica>
// grammar; persistent and provider-internal names use the injective join below.
//
// Persistent and provider-internal identifiers are joined with underscores.
// Hyphens would be ambiguous there: `ob-<app>-<service>` maps both (a-b, c) and
// (a, b-c) to `ob-a-b-c`. Underscore is excluded from the identifier grammar and
// accepted in project and volume names, which makes that derivation injective.
// Runtime segments escape an authored hyphen as `--`, leaving a single hyphen as
// an unambiguous separator while ordinary names retain the simple form.
const (
	// ProxyProject and IngressNetwork are host-scoped.
	ProxyProject   = "onebox-proxy"
	IngressNetwork = "ob-ingress"

	// HostNamespace holds state shared by everything on the box.
	HostNamespace = "_host"

	// DefaultBasePath follows the platform convention for variable state owned
	// by a program that installs nothing of its own.
	DefaultBasePath = "/var/lib/ob"
)

// Names derives every generated name for one project and environment.
type Names struct {
	App      string
	BasePath string
}

// NamesFor resolves the base path for an environment, falling back to the
// project value and then to the documented default.
func (p *Spec) NamesFor(env string) Names {
	base := p.BasePath
	if e, ok := p.Environments[env]; ok && e.BasePath != "" {
		base = e.BasePath
	}
	if base == "" {
		base = DefaultBasePath
	}
	return Names{App: p.Name, BasePath: base}
}

// ComposeProject is the application's Compose project. It is the application
// identifier alone, which cannot collide with any derived name because
// identifiers contain no underscore and may not begin `ob-`.
func (n Names) ComposeProject() string { return n.App }

// ServiceProject is a supporting service's own Compose project, kept separate
// from the application's so a release or rollback cannot remove it.
func (n Names) ServiceProject(service string) string {
	return join("ob", n.App, service)
}

// ServiceContainer is a service's stable singleton slot. The explicit ordinal
// keeps every application-owned runtime name in one predictable grammar.
func (n Names) ServiceContainer(service string) string {
	return containerName(n.App, service, 1)
}

// ServiceNetwork joins the application to its services. It is one network per
// application rather than per service: workloads reach a service by its
// declared name, and a network per service would mean every workload joining
// several to say the same thing.
//
// It cannot collide with ServiceProject — that always carries a third segment —
// and it is created once, outside any release, because the services attached to
// it outlive every release.
func (n Names) ServiceNetwork() string { return join("ob", n.App) }

// ServiceDir holds what services need and releases must not touch: their
// generated Compose documents and their credentials.
func (n Names) ServiceDir() string { return path.Join(n.AppDir(), "services") }

// ServiceFile is a service's generated Compose document on the target.
func (n Names) ServiceFile(service string) string {
	return path.Join(n.ServiceDir(), service+".yaml")
}

// ProtectionRuntimeDir holds what a protected service needs at run time: the
// verified wal-g binary and the generated wrapper that puts its credentials in
// scope. The whole directory is mounted read-only into the container.
//
// A directory rather than two file mounts, and that is not tidiness. Onebox
// replaces generated files atomically, by writing a temporary file and renaming
// it over the target — which gives the path a new inode. A Docker bind-mount of
// a *file* is bound to the inode, so an atomically replaced file silently
// disappears from inside the running container: the mount still points at the
// inode that was unlinked. Mounting the directory keeps the mount stable.
//
// It is keyed by wal-g version, so upgrading the pinned version changes the
// mount path and the container is recreated onto the new binary rather than
// having it swapped underneath a running server.
func (n Names) ProtectionRuntimeDir(service string) string {
	return path.Join(n.AppDir(), "protection", "runtime", service, WalgVersion)
}

// ProtectionBinaryFile is the verified wal-g binary on the target.
func (n Names) ProtectionBinaryFile(service string) string {
	return path.Join(n.ProtectionRuntimeDir(service), "wal-g")
}

// ProtectionWrapperFile is the generated credential wrapper. It sits beside the
// binary and holds no secret: it names the credential entries and reads their
// values from the environment.
func (n Names) ProtectionWrapperFile(service string) string {
	return path.Join(n.ProtectionRuntimeDir(service), "ob-wal-g")
}

// ServiceSecretFile holds the credential Onebox generates on the target. It is
// written once and never travels: not in the project, not in the rendered
// runtime, not in the digest.
func (n Names) ServiceSecretFile(service string) string {
	return path.Join(n.ServiceDir(), service+".secret.env")
}

// ProtectionSecretDir contains lifecycle-only mode-0600 credential files on
// the target. Plans and units refer to these paths and named entries only.
func (n Names) ProtectionSecretDir() string {
	return path.Join(n.AppDir(), "protection", "secrets")
}

func (n Names) ProtectionCredentialFile(service, target string) string {
	return path.Join(n.ProtectionSecretDir(), service+"-"+target+".env")
}

func (n Names) ActiveVolumeFile(service string) string {
	return path.Join(n.AppDir(), "protection", "state", service+".active-volume.json")
}

// ProtectionLifecycleStateFile is the durable target-side source used before
// rendering a managed service. It is separate from active-volume selection:
// one binds lifecycle/image policy, the other binds the physical data volume.
func (n Names) ProtectionLifecycleStateFile(service string) string {
	return path.Join(n.AppDir(), "protection", "state", service+".lifecycle.json")
}

// ServiceVersionFile records the version that last ran successfully. The
// running container's image is not the same fact: after a refused or failed
// upgrade the image may be a version that never opened the data directory, and
// treating it as authoritative traps the operator on the way back.
func (n Names) ServiceVersionFile(service string) string {
	return path.Join(n.ServiceDir(), service+".version")
}

// ServiceClientFile holds the connection details workloads read, under the
// names Onebox chose. It is derived from the credential file on the target,
// for the same reason.
func (n Names) ServiceClientFile(service string) string {
	return path.Join(n.ServiceDir(), service+".client.env")
}

// ServiceAliasFile holds the same connection under the names one workload
// asked for. It is per workload because two workloads may want different
// names for the same database, and per service because a workload may need
// several.
func (n Names) ServiceAliasFile(service, workload string) string {
	return path.Join(n.ServiceDir(), service+"."+workload+".env")
}

// WorkloadVolume and ServiceVolume share a pattern. That is safe only because
// workload and service identifiers are unique across both blocks, which the
// loader enforces; without that rule these would collide.
func (n Names) WorkloadVolume(workload, volume string) string {
	return join("ob", n.App, workload, volume)
}

func (n Names) ServiceVolume(service, volume string) string {
	return join("ob", n.App, service, volume)
}

func (n Names) ProtectionRestoreProject(service string) string {
	return join("ob", n.App, service, "restore")
}

func (n Names) ProtectionRestoreContainer(service string) string {
	return runtimeName(n.App, service, "restore", "1")
}

func (n Names) ProtectionRestoreNetwork(service string) string {
	return join("ob", n.App, service, "restore-net")
}

func (n Names) ProtectionRestoreVolume(service string) string {
	return join("ob", n.App, service, "restore-stage")
}

// ProtectionTimerForEnvironment names a protection timer.
//
// The "ob-protection-" prefix keeps it out of the namespace SyncSchedules owns.
// That is not cosmetic: the job scheduler treats every unit named "ob-<app>-*"
// as its own and removes the ones no longer declared, so protection timers named
// that way were deleted by the next deploy — every scheduled backup silently
// stopped, and the only trace was a line in the deploy output saying the
// schedule was "no longer declared".
func (n Names) ProtectionTimerForEnvironment(environment, service, operation string) string {
	return n.ProtectionUnitForEnvironment(environment, service, operation) + ".timer"
}

// ProtectionUnitForEnvironment is the systemd unit name without its suffix, so
// the .service and .timer that pair together cannot be spelled differently.
func (n Names) ProtectionUnitForEnvironment(environment, service, operation string) string {
	return ProtectionUnitPrefix + n.App + "-" + environment + "-" + service + "-" + operation
}

// ProtectionUnitPrefix is the systemd namespace protection owns outright.
const ProtectionUnitPrefix = "ob-protection-"

// Container is a workload's stable runtime slot. Container names are
// host-global, so every one carries the application, component, and a
// one-based replica ordinal — including singleton workloads.
func (n Names) Container(workload string, replica int) string {
	return containerName(n.App, workload, replica)
}

// TransientContainer is the name a rollout gives a new container before it takes
// a stable slot. It is application-scoped like every other name, and belongs in
// the preflight collision check.
func (n Names) TransientContainer(workload string) string {
	return runtimeName(n.App, workload, "new")
}

// Router and ProxyService name the proxy's routing objects. They are part of the
// contract rather than an implementation detail, because they become permanent
// the first time a release ships.
//
// A router carries an `r` prefix on its index. Without it, router 2 of a
// workload derives the same string as that workload's second replica container —
// harmless while the two live in different namespaces, and a trap the moment
// anyone reads one list and assumes the other.
func (n Names) Router(workload string, route int) string {
	return join(n.App, workload, fmt.Sprintf("r%d", route))
}

func (n Names) ProxyService(workload string) string {
	return join(n.App, workload)
}

// ProxyServiceFor is the Traefik backend for one route.
//
// A workload may declare several routes on different ports, and a backend
// carries the port — so one backend per workload cannot describe them. Every
// route after the first would overwrite the port of the ones before it, and
// the whole workload would answer on whichever port happened to be declared
// last. Route zero keeps the workload's own name so a single-routed workload's
// labels do not move.
func (n Names) ProxyServiceFor(workload string, route int) string {
	if route == 0 {
		return n.ProxyService(workload)
	}
	return join(n.App, workload, fmt.Sprintf("r%d", route))
}

// AppDir, ReleasesDir, ReleaseDir, CurrentLink and HostDir are the remote layout.
func (n Names) AppDir() string      { return path.Join(n.BasePath, n.App) }
func (n Names) ReleasesDir() string { return path.Join(n.AppDir(), "releases") }
func (n Names) ReleaseDir(id string) string {
	return path.Join(n.ReleasesDir(), id)
}
func (n Names) CurrentLink() string { return path.Join(n.AppDir(), "current") }
func (n Names) HostDir() string     { return path.Join(n.BasePath, HostNamespace) }

// HostOwnerPath is where the host owner record lives. Preflight and the engine
// both probe it, and a preflight that reads a different path than the mutation
// it precedes would report an owned host as unclaimed.
func (n Names) HostOwnerPath() string { return n.HostDir() + "/owner" }

// All returns every container-runtime name a project derives, sorted. Preflight
// checks each against foreign resources on the target, and the golden test pins
// the set so a change that would rename an existing volume fails loudly.
//
// Routers and proxy services are deliberately absent: they are labels, not
// runtime objects, so they cannot collide with a container and including them
// would make preflight report conflicts that do not exist.
func (p *Spec) All(env string) []string {
	n := p.NamesFor(env)
	out := []string{n.ComposeProject()}
	for _, w := range sortedKeys(p.Workloads) {
		wl := p.Workloads[w]
		out = append(out, n.Container(w, 1), n.TransientContainer(w))
		for i := 2; i <= wl.Replicas; i++ {
			out = append(out, n.Container(w, i))
		}
		for _, v := range wl.Volumes {
			if !v.IsBind() {
				out = append(out, n.WorkloadVolume(w, v.Name))
			}
		}
	}
	for _, s := range sortedKeys(p.Services) {
		out = append(out, n.ServiceProject(s), n.ServiceContainer(s))
		for _, v := range p.Services[s].Volumes {
			out = append(out, n.ServiceVolume(s, v))
		}
		if p.Services[s].Backup != nil {
			out = append(out,
				n.ProtectionRestoreProject(s),
				n.ProtectionRestoreContainer(s),
				n.ProtectionRestoreNetwork(s),
				n.ProtectionRestoreVolume(s),
			)
		}
	}
	sort.Strings(out)
	return out
}

// routesOf normalises the scalar domain/port shorthand into the route list, so
// callers never have to check which form was authored.
func routesOf(w Workload) []Route {
	if len(w.Routes) > 0 {
		return w.Routes
	}
	if w.Domain == "" {
		return nil
	}
	return []Route{{
		Domain: w.Domain, Port: w.Port, Path: "/",
		Entrypoint: "websecure", Protocol: "http", Scheme: "http", TLS: "terminate",
	}}
}

// NormalisedRoutes returns the workload's routes with the scalar shorthand
// expanded, so callers never handle two shapes.
func (w Workload) NormalisedRoutes() []Route { return routesOf(w) }

// Join is the injective separator rule above, exported for derived identifiers
// that live outside this file — a backup repository prefix among them.
func Join(parts ...string) string { return join(parts...) }

func join(parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "_"
		}
		out += p
	}
	return out
}

func containerName(app, component string, replica int) string {
	if replica < 1 {
		panic("container replica ordinal must be positive")
	}
	return runtimeName(app, component, fmt.Sprint(replica))
}

func runtimeName(parts ...string) string {
	escaped := make([]string, len(parts))
	for i, part := range parts {
		escaped[i] = strings.ReplaceAll(part, "-", "--")
	}
	return strings.Join(escaped, "-")
}

// ProtectionRunLock is the mutex over actual repository work for one service.
//
// It exists because Onebox's protection lock is a value written to a file and
// verified by comparing it — a protocol a systemd unit's shell cannot join. So
// scheduled units and the engine's own wal-g invocations both take this flock
// instead, which makes it the one thing serialising a timer against an operator
// running `ob backup` at the same moment.
func (n Names) ProtectionRunLock(service string) string {
	return path.Join(n.AppDir(), "protection", "run-"+service+".lock")
}
