package app

import (
	"fmt"
	"path"
	"sort"
)

// Derived names are contract. Once a volume exists its name can never change
// without moving data, so every pattern here is fixed and pinned by a golden
// test.
//
// Underscore joins identifiers. Hyphen cannot: identifiers may themselves
// contain hyphens, so `ob-<app>-<service>` maps both (a-b, c) and (a, b-c) to
// `ob-a-b-c`. Underscore is excluded from the identifier grammar and accepted by
// the container runtime in project and volume names, which makes the derivation
// injective. Application identifiers additionally may not begin `ob-`, which
// reserves the two pre-existing hyphenated host-scoped names.
const (
	// ProxyProject and IngressNetwork are host-scoped and predate this contract.
	ProxyProject   = "ob-proxy"
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

// ServiceContainer is a service's container name. It is fixed — unlike a
// workload, a service is never replaced by a second copy running beside it, so
// there is no handover that a fixed name would forbid.
func (n Names) ServiceContainer(service string) string {
	return join(n.App, service)
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

// ServiceSecretFile holds the credential Onebox generates on the target. It is
// written once and never travels: not in the project, not in the rendered
// runtime, not in the digest.
func (n Names) ServiceSecretFile(service string) string {
	return path.Join(n.ServiceDir(), service+".secret.env")
}

// ServiceClientFile holds the connection details workloads read. It is derived
// from the credential file on the target, for the same reason.
func (n Names) ServiceClientFile(service string) string {
	return path.Join(n.ServiceDir(), service+".client.env")
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

// Container is the stable name of a workload's container. Container names are
// host-global in the container runtime, so every one carries the application.
func (n Names) Container(workload string, replica int) string {
	if replica <= 1 {
		return join(n.App, workload)
	}
	return join(n.App, workload, fmt.Sprint(replica))
}

// TransientContainer is the name a rollout gives a new container before it takes
// a stable slot. It is application-scoped like every other name, and belongs in
// the preflight collision check.
func (n Names) TransientContainer(workload string) string {
	return join(n.App, workload, "new")
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

// AppDir, ReleasesDir, ReleaseDir, CurrentLink and HostDir are the remote layout.
func (n Names) AppDir() string      { return path.Join(n.BasePath, n.App) }
func (n Names) ReleasesDir() string { return path.Join(n.AppDir(), "releases") }
func (n Names) ReleaseDir(id string) string {
	return path.Join(n.ReleasesDir(), id)
}
func (n Names) CurrentLink() string { return path.Join(n.AppDir(), "current") }
func (n Names) HostDir() string     { return path.Join(n.BasePath, HostNamespace) }

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
