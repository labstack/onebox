package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Rendered is a generated Compose runtime and its content digest. The digest is
// what a plan binds: execution regenerates from the plan's own inputs and
// refuses a mismatch, so nobody has to believe the file executed matches the
// file reviewed.
type Rendered struct {
	Bytes  []byte
	Digest string

	// Services are the supporting services' own Compose documents, keyed by
	// service name. They are applied outside any release, which is what keeps
	// a rollback from taking a database with it.
	Services map[string][]byte

	// Unresolved names the build-sourced workloads that had no image. It is
	// empty on every runtime produced for execution — Render refuses those
	// outright — and populated only by RenderForInspection, whose output
	// describes a release rather than being one.
	Unresolved []string
}

// Runnable reports whether this runtime can be deployed. A rendering with an
// unresolved image describes what a release would look like once the image
// exists; running it would start a placeholder.
func (r *Rendered) Runnable() bool { return len(r.Unresolved) == 0 }

// UnresolvedImage is what an unresolved build stands in as. It is not a real
// reference and no registry serves it, so a runtime carrying it fails at the
// pull rather than starting something unintended.
const UnresolvedImage = "ob-unresolved-image:no-release"

// Images resolves a build-sourced workload to the image reference a release will
// run. Resolving a version from a tag is the release-pipeline change; until it
// lands the caller supplies this and generation fails closed without it.
type Images map[string]string

// Render generates the Compose runtime for one environment and release.
//
// It is a pure function of its inputs and opens no target connection: anything
// requiring the target — name collisions, account privileges — belongs to
// preflight, which runs after this and before any mutation.
func (p *Spec) Render(env, releaseID string, images Images) (*Rendered, error) {
	// Resolve first, always. Rendering an unresolved project would silently
	// ignore every environment override, which is the kind of bug that only
	// shows up as staging quietly running production's replica count.
	r, err := p.Resolve(env)
	if err != nil {
		return nil, err
	}
	return r.render(env, releaseID, images)
}

// Render on an already-resolved project renders it as-is rather than resolving
// a second time.
func (r *Resolved) Render(env, releaseID string, images Images) (*Rendered, error) {
	return r.render(env, releaseID, images)
}

func (r *Resolved) render(env, releaseID string, images Images) (*Rendered, error) {
	p := r.Spec
	n := p.NamesFor(env)

	services := map[string]any{}
	volumes := map[string]any{}
	var extraNetworks map[string]any

	for _, name := range sortedKeys(p.Workloads) {
		w := p.Workloads[name]
		svc, used, carried, err := p.renderWorkload(n, name, w, releaseID, images)
		if err != nil {
			return nil, err
		}
		services[name] = svc
		for _, v := range used {
			// Pin the name. Compose otherwise prefixes the project onto it, so
			// the volume Docker creates is not the volume the naming contract
			// promises — and preflight, which looks for the contract name,
			// would never see a collision that exists.
			volumes[v] = map[string]any{
				"name": v,
				// Ownership must be on the volume itself. Preflight reads
				// labels to tell a previous release from a stranger's resource,
				// and an unlabelled volume we created looks like a collision.
				"labels": map[string]any{"ob.app": p.Name},
			}
		}
		// Definitions a referenced service depends on: a segmented network, an
		// NFS-backed volume. Dropping them would change the runtime silently.
		for k, v := range carried.Volumes {
			volumes[k] = v
		}
		for k, v := range carried.Networks {
			if extraNetworks == nil {
				extraNetworks = map[string]any{}
			}
			extraNetworks[k] = v
		}
	}

	doc := map[string]any{
		"name":     n.ComposeProject(),
		"services": services,
	}
	if len(volumes) > 0 {
		doc["volumes"] = volumes
	}
	nets := map[string]any{}
	for k, v := range extraNetworks {
		nets[k] = v
	}
	if p.routesAnywhere() && p.Proxy.Kind != "none" {
		nets[p.Proxy.Network] = map[string]any{"external": true}
	}
	// The service network is external because the services on it outlive every
	// release. Compose would otherwise create it with the release and remove it
	// with the release, taking the database's reachability with it.
	if len(p.Services) > 0 {
		nets[n.ServiceNetwork()] = map[string]any{"external": true}
	}
	if len(nets) > 0 {
		doc["networks"] = nets
	}

	b, err := marshalDeterministic(doc)
	if err != nil {
		return nil, errf("render_failed", "", "", "cannot render runtime: %v", err)
	}

	// Each service is its own document and its own project. They are rendered
	// here, with the application, because they come from the same declaration
	// and must agree about names and networks — but they are applied
	// separately, and never by a release.
	rendered := &Rendered{Bytes: b}
	if len(p.Services) > 0 {
		rendered.Services = map[string][]byte{}
		for _, name := range sortedKeys(p.Services) {
			doc, err := p.renderService(n, name, p.Services[name])
			if err != nil {
				return nil, err
			}
			rendered.Services[name] = doc
		}
	}

	// The digest covers the services too. A changed Postgres version with an
	// unchanged application is still a different runtime, and a plan that did
	// not notice would be a plan that lied.
	sum := sha256.New()
	sum.Write(b)
	for _, name := range sortedKeys(rendered.Services) {
		sum.Write([]byte(name))
		sum.Write(rendered.Services[name])
	}
	rendered.Digest = hex.EncodeToString(sum.Sum(nil))
	return rendered, nil
}

// overlayFor is the enumerated set applied to a Compose-referenced workload.
func (p *Spec) overlayFor(n Names, name string, w Workload, releaseID string) overlay {
	ov := overlay{
		Labels: map[string]any{
			"ob.app":      p.Name,
			"ob.workload": name,
			"ob.release":  releaseID,
		},
		HasRoute:       len(w.NormalisedRoutes()) > 0,
		Health:         healthcheck(w.Health),
		EnvFiles:       p.projectedEnvFiles(n, name, w),
		ConnectionVars: p.connectionVars(name, w),
	}
	for k, v := range p.routeLabels(n, name, w) {
		ov.Labels[k] = v
	}
	if p.Proxy.Kind != "none" && ov.HasRoute {
		ov.Network = p.Proxy.Network
	}
	return ov
}

func (p *Spec) renderWorkload(n Names, name string, w Workload, releaseID string, images Images) (map[string]any, []string, definitions, error) {
	svc := map[string]any{}
	var namedVolumes []string
	var carried definitions

	switch {
	case w.Image != nil:
		// A resolved pin wins over the declared reference. Pinning turns a
		// mutable tag into a digest; refusing to apply it here would leave the
		// release on the tag while the plan reported the digest.
		svc["image"] = w.Image.Reference
		if ref := images[name]; ref != "" {
			svc["image"] = ref
		}
	case w.Build != nil:
		ref, ok := images[name]
		if !ok || ref == "" {
			if images[inspectionKey] == "" {
				return nil, nil, definitions{}, errf("image_unresolved", "workloads."+name,
					"ob deploy --image "+name+"=<reference>",
					"workload %q is built from source, and production never builds: pass the reference "+
						"whatever built it produced, as --image %s=<reference>", name, name)
			}
			ref = UnresolvedImage
		}
		svc["image"] = ref
	case w.Compose != "":
		merged, deps, err := mergeComposeRef(p.Dir, w.Compose, p.overlayFor(n, name, w, releaseID))
		if err != nil {
			return nil, nil, definitions{}, err
		}
		carried = deps
		// A referenced service is copied verbatim with the overlay already
		// applied. Nothing below may add to it: the declaration describes a
		// workload Onebox generates, and this one the user authored — except a
		// resolved pin, which replaces the tag the file names for the same
		// reason it replaces a declared one.
		if ref := images[name]; ref != "" && ref != UnresolvedImage {
			merged["image"] = ref
		}
		return merged, nil, carried, nil
	}

	if w.Command != nil {
		if c, ok := commandArgs(w.Command); ok {
			svc["command"] = c
		}
	}

	// Onebox owns container naming, so container_name is never emitted: a fixed
	// name forbids the two containers a rolling handover needs, and the rollout
	// assigns the derived name itself.
	labels := map[string]any{}
	// The user's labels go on first; Onebox's own follow and the schema reserves
	// its two namespaces, so nothing the user wrote can be overwritten here.
	for k, v := range w.Labels {
		labels[k] = v
	}
	labels["ob.app"] = p.Name
	labels["ob.workload"] = name
	labels["ob.release"] = releaseID
	// Onebox runs the replicas itself under derived slot names, so the count is
	// not a Compose concern — but it must still be part of the bound content.
	// Without it a scale change renders an identical runtime and the plan digest
	// never notices.
	labels["ob.replicas"] = fmt.Sprint(w.Replicas)
	for k, v := range p.routeLabels(n, name, w) {
		labels[k] = v
	}
	svc["labels"] = labels

	if env := stringMap(w.Env); len(env) > 0 {
		svc["environment"] = env
	}
	// A workload that needs a service reads how to reach it. The file is
	// written on the target from the credential generated there, so the
	// connection string never appears in the project, the rendered runtime or
	// the digest — and an application gets its database URL without anyone
	// copying a password into a repository.
	// Order is precedence: Compose applies each env_file over the ones before
	// it. Declared environment files come first, then the decrypted secrets,
	// then the managed-service connections — a generated credential is the
	// only thing that can actually open the service it describes, so nothing
	// authored is allowed to shadow it.
	var files []string
	for _, entry := range p.EnvFilesFor(w) {
		files = append(files, entry.StagedPath())
	}
	files = append(files, p.serviceClientFiles(n, name, w)...)
	if len(files) > 0 {
		svc["env_file"] = files
	}

	var mounts []string
	for _, v := range w.Volumes {
		if v.IsBind() {
			mounts = append(mounts, mount(v.Source, v.Target, v.Mode))
			continue
		}
		derived := n.WorkloadVolume(name, v.Name)
		namedVolumes = append(namedVolumes, derived)
		mounts = append(mounts, mount(derived, v.Path, v.Mode))
	}
	if len(mounts) > 0 {
		svc["volumes"] = mounts
	}

	if len(w.Ports) > 0 {
		var ports []string
		for _, pp := range w.Ports {
			s := fmt.Sprintf("%s:%d:%d", pp.Bind, pp.Host, pp.Container)
			if pp.Protocol == "udp" {
				s += "/udp"
			}
			ports = append(ports, s)
		}
		svc["ports"] = ports
	}

	if len(w.Needs) > 0 {
		// depends_on cannot cross Compose projects, and a service lives in its
		// own. Ordering against a service is enforced by applying services
		// before any release rather than by a key the runtime would reject.
		dep := map[string]any{}
		for _, need := range w.Needs {
			if _, isService := p.Services[need.Name]; isService {
				continue
			}
			dep[need.Name] = map[string]any{"condition": composeCondition(need.Condition)}
		}
		if len(dep) > 0 {
			svc["depends_on"] = dep
		}
	}

	if hc := healthcheck(w.Health); hc != nil {
		svc["healthcheck"] = hc
	}

	// Passthrough: declared verbatim, carrying no Onebox semantics.
	if w.Entrypoint != nil {
		svc["entrypoint"] = w.Entrypoint
	}
	for key, val := range map[string]string{
		"user": w.User, "hostname": w.Hostname, "working_dir": w.WorkingDir,
	} {
		if val != "" {
			svc[key] = val
		}
	}
	for key, val := range map[string]*bool{
		"init": w.Init, "tty": w.TTY, "stdin_open": w.StdinOpen,
	} {
		if val != nil {
			svc[key] = *val
		}
	}
	if len(w.ExtraHosts) > 0 {
		svc["extra_hosts"] = w.ExtraHosts
	}
	if w.Logging != nil {
		lg := map[string]any{}
		if w.Logging.Driver != "" {
			lg["driver"] = w.Logging.Driver
		}
		if len(w.Logging.Options) > 0 {
			lg["options"] = w.Logging.Options
		}
		if len(lg) > 0 {
			svc["logging"] = lg
		}
	}
	if w.Drain != nil && w.Drain.Grace != "" {
		svc["stop_grace_period"] = w.Drain.Grace
	}
	if w.Resources != nil {
		if w.Resources.Memory != "" {
			svc["mem_limit"] = w.Resources.Memory
		}
		if w.Resources.CPUs != "" {
			svc["cpus"] = w.Resources.CPUs
		}
	}
	nets := []string{"default"}
	if len(w.NormalisedRoutes()) > 0 && p.Proxy.Kind != "none" {
		nets = append(nets, p.Proxy.Network)
	}
	if len(p.serviceNeedsOf(w)) > 0 {
		nets = append(nets, n.ServiceNetwork())
	}
	if len(nets) > 1 {
		svc["networks"] = nets
	}
	// A job runs to completion. Restarting it forever is wrong, and `compose up`
	// must not start it at all: it runs at a release phase or on a schedule,
	// under Onebox's control. A profile keeps it out of the default set.
	if w.Role == RoleJob {
		svc["restart"] = "no"
		svc["profiles"] = []string{"job"}
	} else {
		svc["restart"] = "unless-stopped"
	}

	// Everything above came from the declaration, so it is Onebox's to escape.
	return escapeDollars(svc).(map[string]any), namedVolumes, carried, nil
}

// envFilesFor applies the workload's own list, falling back to the project-wide
// list for applications and workers only. A daemon never receives project-wide
// files: it is a database or a cron runner, not the application, and projecting
// the application's secrets into it was the failure this rule exists to prevent.
// envFilesFor resolves one workload's list.
//
// Most specific wins: the workload's own declaration, else the environment's
// default, else the project's. An environment override has already been applied
// to the workload by resolution, so it arrives here as the workload's own.
//
// A declared empty list is not an absent one. `nil` means the workload said
// nothing and takes the next default; a non-nil empty list means it declared
// that it receives none, which was previously inexpressible.
//
// The role gate governs only the default. It admits the workload roles that are
// the application's own and excludes a `daemon`, whose configuration is its
// own — but a daemon that declares a list receives exactly it, because the gate
// never reaches an explicit declaration.
//
// What it does not consult is the workload's source. An application adopted
// from a Compose file resolves what an application declared inline resolves;
// the source decides where the container comes from, not what it is told.
func (p *Spec) EnvFilesFor(w Workload) []EnvFile {
	if w.EnvFiles != nil {
		return w.EnvFiles
	}
	if !roleTakesTheDefault(w.Role) {
		return nil
	}
	if p.envDefault != nil {
		return p.envDefault
	}
	if p.Runtime == nil {
		return nil
	}
	return p.Runtime.EnvFiles
}

// roleTakesTheDefault is the gate, in one place so it cannot drift between the
// paths that ask it.
func roleTakesTheDefault(role string) bool {
	return role == RoleApplication || role == RoleWorker || role == RoleJob
}

// routeLabels emits the exact routing keys the overlay contract enumerates.
func (p *Spec) routeLabels(n Names, name string, w Workload) map[string]any {
	routes := w.NormalisedRoutes()
	if len(routes) == 0 || p.Proxy.Kind == "none" {
		return nil
	}
	out := map[string]any{
		"traefik.enable":         "true",
		"traefik.docker.network": p.Proxy.Network,
	}
	for i, r := range routes {
		svcName := n.ProxyServiceFor(name, i)
		router := n.Router(name, i)
		kind := "http"
		rule := fmt.Sprintf("Host(`%s`)", r.Domain)
		if r.Protocol == "tcp" {
			kind = "tcp"
			rule = fmt.Sprintf("HostSNI(`%s`)", r.Domain)
		} else if r.Path != "" && r.Path != "/" {
			rule += fmt.Sprintf(" && PathPrefix(`%s`)", r.Path)
		}
		pre := fmt.Sprintf("traefik.%s.routers.%s.", kind, router)
		out[pre+"rule"] = rule
		out[pre+"entrypoints"] = r.Entrypoint
		if r.TLS == "terminate" || r.TLS == "passthrough" {
			out[pre+"tls"] = "true"
			// `tls=true` alone terminates. Passthrough is a separate key, and
			// without it the proxy decrypts traffic the author asked it to
			// forward untouched — which looks like it works, because the
			// backend still answers.
			if r.TLS == "passthrough" {
				out[pre+"tls.passthrough"] = "true"
			}
			// Without a resolver the router terminates TLS with no certificate
			// to terminate it with.
			if p.Proxy.CertResolver != "" && r.TLS == "terminate" {
				out[pre+"tls.certresolver"] = p.Proxy.CertResolver
			}
		}
		// Named explicitly: with more than one service defined on a container,
		// a router that does not say which one it means is not a router that
		// picks correctly.
		out[pre+"service"] = svcName
		sp := fmt.Sprintf("traefik.%s.services.%s.loadbalancer.server.", kind, svcName)
		out[sp+"port"] = fmt.Sprint(r.Port)
		if kind == "http" && r.Scheme != "" && r.Scheme != "http" {
			out[sp+"scheme"] = r.Scheme
		}
	}
	return out
}

func (p *Spec) routesAnywhere() bool {
	for _, w := range p.Workloads {
		if len(w.NormalisedRoutes()) > 0 {
			return true
		}
	}
	return false
}

// healthcheck renders the declared probe. An HTTP probe becomes a shell probe
// because Compose has no HTTP form; it prefers curl and falls back to wget, so
// it works on the images people actually ship.
// DrainFile is how a rollout takes a container out of rotation before it stops
// it. The generated health check fails while this file exists, the runtime
// flips the container unhealthy, and the proxy stops routing to it — all
// before anything sends a signal. Without the guard the container reports
// healthy until the moment it dies, and the requests in flight at that moment
// are lost.
const DrainFile = "/tmp/ob-drain"

// drainGuarded prefixes a shell-form check with the drain test.
func drainGuarded(check string) string {
	return "[ -f " + DrainFile + " ] && exit 1; " + check
}

func healthcheck(h *Health) map[string]any {
	if h == nil {
		return nil
	}
	var test []string
	switch {
	case h.Exec != nil:
		switch v := h.Exec.(type) {
		case string:
			if v == "" {
				return nil
			}
			test = []string{"CMD-SHELL", drainGuarded(v)}
		case []any:
			// Executed directly, with no shell between the runtime and the
			// command. This is the only form an image without a shell can run,
			// and the one form that cannot carry the drain guard: there is no
			// shell to test the file in. The rollout detects that and says so
			// rather than waiting out a flip that can never happen.
			test = []string{"CMD"}
			for _, a := range v {
				test = append(test, fmt.Sprint(a))
			}
			if len(test) == 1 {
				return nil
			}
		default:
			return nil
		}
	case h.HTTP != "":
		url := fmt.Sprintf("http://127.0.0.1:%d%s", h.Port, h.HTTP)
		test = []string{"CMD-SHELL",
			drainGuarded(fmt.Sprintf("curl -fsS %s || wget -qO- %s || exit 1", url, url))}
	case h.TCP:
		test = []string{"CMD-SHELL", drainGuarded(fmt.Sprintf("nc -z 127.0.0.1 %d || exit 1", h.Port))}
	default:
		return nil
	}
	out := map[string]any{"test": test}
	if h.Interval != "" {
		out["interval"] = h.Interval
	}
	if h.StartPeriod != "" {
		out["start_period"] = h.StartPeriod
	}
	if h.Retries > 0 {
		out["retries"] = h.Retries
	}
	return out
}

func composeCondition(c string) string {
	switch c {
	case "started":
		return "service_started"
	case "completed":
		return "service_completed_successfully"
	default:
		return "service_healthy"
	}
}

func commandArgs(c any) (any, bool) {
	switch v := c.(type) {
	case []any:
		return v, true
	case map[string]any:
		if run, ok := v["run"].(string); ok {
			return run, true
		}
	case string:
		return v, true
	}
	return nil, false
}

func mount(source, target, mode string) string {
	s := source + ":" + target
	if mode == "ro" {
		s += ":ro"
	}
	return s
}

func stringMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// escapeDollars doubles every `$` in the values Onebox generates.
//
// Compose interpolates the file it reads. A `$VAR` that Onebox wrote — the
// shell expansion in a generated health check, a command the author declared
// meaning "expand this inside the container" — would otherwise be substituted
// on the host from an environment that does not have it, and silently become
// the empty string. That is how a Redis ends up running with `--requirepass ""`
// while the application holds a real password: everything reports healthy and
// nothing works.
//
// Content copied verbatim from a Compose file the author referenced is left
// alone. Interpolation is that file's own contract, and a project that writes
// `image: app:${TAG}` there means it.
func escapeDollars(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "$", "$$")
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = strings.ReplaceAll(s, "$", "$$")
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = escapeDollars(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = escapeDollars(e)
		}
		return out
	}
	return v
}

// marshalDeterministic renders with sorted keys at every level. Generation must
// be a pure function of its inputs, and map iteration order is the easiest way
// to accidentally make it otherwise.
func marshalDeterministic(v any) ([]byte, error) {
	node, err := toNode(v)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

func toNode(v any) (*yaml.Node, error) {
	switch t := v.(type) {
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range sortedKeys(t) {
			kn := &yaml.Node{Kind: yaml.ScalarNode, Value: k}
			vn, err := toNode(t[k])
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, kn, vn)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range t {
			in, err := toNode(item)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, in)
		}
		return n, nil
	case []string:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range t {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: item})
		}
		return n, nil
	default:
		n := &yaml.Node{}
		if err := n.Encode(v); err != nil {
			return nil, err
		}
		return n, nil
	}
}

// SortedRouteKeys is exported for tests that assert label determinism.
func SortedRouteKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// inspectionKey marks a render as being for reading rather than running. It is
// a key no workload can have — identifiers may not contain a space — so an
// author cannot reach this mode by naming something unfortunately.
const inspectionKey = "ob inspection"

// RenderForInspection renders a runtime for describing a release rather than
// performing one. A build-sourced workload with no resolved image renders with
// a placeholder and is named in Unresolved, instead of failing the whole
// rendering.
//
// Reading and running are separated because they fail differently. `ob deploy`
// must refuse a project whose image nobody has built; `ob status` must still
// be able to say what that project consists of. Collapsing the two either
// makes status useless before the first release or lets a placeholder reach a
// host.
func (r *Resolved) RenderForInspection(env string, images Images) (*Rendered, error) {
	probe := Images{inspectionKey: "yes"}
	for k, v := range images {
		probe[k] = v
	}
	out, err := r.render(env, "", probe)
	if err != nil {
		return nil, err
	}
	for _, name := range sortedKeys(r.Workloads) {
		if w := r.Workloads[name]; w.Build != nil && images[name] == "" {
			out.Unresolved = append(out.Unresolved, name)
		}
	}
	return out, nil
}

// serviceNeedsOf lists the supporting services a workload declares a
// prerequisite on, in declaration order.
func (p *Spec) serviceNeedsOf(w Workload) []string {
	var out []string
	for _, need := range w.Needs {
		if _, ok := p.Services[need.Name]; ok {
			out = append(out, need.Name)
		}
	}
	return out
}

// serviceClientFiles are the target-side connection files for the services a
// workload needs: the canonical names always, and its own names where it asked
// for them. Its own come second so they win a collision, which is the only
// reading of "I want this variable to be the host" that is not a surprise.
func (p *Spec) serviceClientFiles(n Names, name string, w Workload) []string {
	var out []string
	for _, service := range p.serviceNeedsOf(w) {
		out = append(out, n.ServiceClientFile(service))
	}
	for _, need := range w.Needs {
		if _, ok := p.Services[need.Name]; ok && len(need.Env) > 0 {
			out = append(out, n.ServiceAliasFile(need.Name, name))
		}
	}
	return out
}

// RenderServices generates just the supporting services' documents.
//
// It is separate from Render because services have nothing to do with a
// release: they can be converged on a host whose application has never been
// built, and refusing to describe a database because an image is missing would
// make the two failures indistinguishable.
func (r *Resolved) RenderServices(env string) (map[string][]byte, error) {
	p := r.Spec
	if len(p.Services) == 0 {
		return nil, nil
	}
	n := p.NamesFor(env)
	out := make(map[string][]byte, len(p.Services))
	for _, name := range sortedKeys(p.Services) {
		doc, err := p.renderService(n, name, p.Services[name])
		if err != nil {
			return nil, err
		}
		out[name] = doc
	}
	return out, nil
}

// projectedEnvFiles is what a workload's role entitles it to, as the paths the
// generated document names.
func (p *Spec) projectedEnvFiles(n Names, name string, w Workload) []string {
	var out []string
	for _, entry := range p.EnvFilesFor(w) {
		out = append(out, entry.StagedPath())
	}
	return append(out, p.serviceClientFiles(n, name, w)...)
}

// connectionVars maps each variable a managed-service connection supplies to
// this workload to the service supplying it.
//
// A workload that names the parts it wants receives those names; one that names
// none receives the driver's canonical set. Either way these are names the
// author must not claim, because the value behind them is generated on the
// target and exists nowhere a project could reach.
func (p *Spec) connectionVars(name string, w Workload) map[string]string {
	out := map[string]string{}
	for _, need := range w.Needs {
		if _, ok := p.Services[need.Name]; !ok {
			continue
		}
		// Both sets, always. A workload that maps a part still receives the
		// canonical connection file beside its alias file, so stopping at the
		// aliases left every canonical name unguarded — a workload mapping one
		// part could author POSTGRES_PASSWORD and outrank the credential it was
		// given.
		for variable := range need.Env {
			out[variable] = need.Name
		}
		if client, ok := p.ClientEnvFor(need.Name); ok {
			for variable := range client.canonicalNames() {
				out[variable] = need.Name
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
