package project

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
}

// Images resolves a build-sourced workload to the image reference a release will
// run. Resolving a version from a tag is the release-pipeline change; until it
// lands the caller supplies this and generation fails closed without it.
type Images map[string]string

// Render generates the Compose runtime for one environment and release.
//
// It is a pure function of its inputs and opens no target connection: anything
// requiring the target — name collisions, account privileges — belongs to
// preflight, which runs after this and before any mutation.
func (p *Project) Render(env, releaseID string, images Images) (*Rendered, error) {
	if _, ok := p.Environments[env]; !ok {
		return nil, errf("unknown_environment", "environments."+env, "",
			"environment %q is not declared", env)
	}
	n := p.NamesFor(env)

	services := map[string]any{}
	volumes := map[string]any{}

	for _, name := range sortedKeys(p.Workloads) {
		w := p.Workloads[name]
		svc, used, err := p.renderWorkload(n, name, w, releaseID, images)
		if err != nil {
			return nil, err
		}
		services[name] = svc
		for _, v := range used {
			volumes[v] = map[string]any{}
		}
	}

	doc := map[string]any{
		"name":     n.ComposeProject(),
		"services": services,
	}
	if len(volumes) > 0 {
		doc["volumes"] = volumes
	}
	if p.routesAnywhere() && p.Proxy.Managed && p.Proxy.Kind != "none" {
		doc["networks"] = map[string]any{
			p.Proxy.Network: map[string]any{"external": true},
		}
	}

	b, err := marshalDeterministic(doc)
	if err != nil {
		return nil, errf("render_failed", "", "", "cannot render runtime: %v", err)
	}
	sum := sha256.Sum256(b)
	return &Rendered{Bytes: b, Digest: hex.EncodeToString(sum[:])}, nil
}

// overlayFor is the enumerated set applied to a Compose-referenced workload.
func (p *Project) overlayFor(n Names, name string, w Workload, releaseID string) overlay {
	ov := overlay{
		Labels: map[string]any{
			"ob.app":      p.App,
			"ob.workload": name,
			"ob.release":  releaseID,
		},
		HasRoute: len(w.NormalisedRoutes()) > 0,
	}
	for k, v := range p.routeLabels(n, name, w) {
		ov.Labels[k] = v
	}
	if p.Proxy.Managed && p.Proxy.Kind != "none" && ov.HasRoute {
		ov.Network = p.Proxy.Network
	}
	return ov
}

func (p *Project) renderWorkload(n Names, name string, w Workload, releaseID string, images Images) (map[string]any, []string, error) {
	svc := map[string]any{}
	var namedVolumes []string

	switch {
	case w.Image != nil:
		svc["image"] = w.Image.Reference
	case w.Build != nil:
		ref, ok := images[name]
		if !ok || ref == "" {
			return nil, nil, errf("image_unresolved", "workloads."+name, "ob release",
				"workload %q is built from source and has no resolved image for this release", name)
		}
		svc["image"] = ref
	case w.Compose != "":
		merged, err := mergeComposeRef(p.Dir, w.Compose, p.overlayFor(n, name, w, releaseID))
		if err != nil {
			return nil, nil, err
		}
		// A referenced service is copied verbatim with the overlay already
		// applied. Nothing below may add to it: the declaration describes a
		// workload Onebox generates, and this one the user authored.
		return merged, nil, nil
	}

	if w.Command != nil {
		if c, ok := commandArgs(w.Command); ok {
			svc["command"] = c
		}
	}

	// Onebox owns container naming, so container_name is never emitted: a fixed
	// name forbids the two containers a rolling handover needs, and the rollout
	// assigns the derived name itself.
	labels := map[string]any{
		"ob.app":      p.App,
		"ob.workload": name,
		"ob.release":  releaseID,
	}
	for k, v := range p.routeLabels(n, name, w) {
		labels[k] = v
	}
	svc["labels"] = labels

	if env := stringMap(w.Env); len(env) > 0 {
		svc["environment"] = env
	}
	if files := p.envFilesFor(w); len(files) > 0 {
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
		dep := map[string]any{}
		for _, need := range w.Needs {
			dep[need.Name] = map[string]any{"condition": composeCondition(need.Condition)}
		}
		svc["depends_on"] = dep
	}

	if hc := healthcheck(w.Health); hc != nil {
		svc["healthcheck"] = hc
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
	if len(w.NormalisedRoutes()) > 0 && p.Proxy.Managed && p.Proxy.Kind != "none" {
		svc["networks"] = []string{"default", p.Proxy.Network}
	}
	// A job runs to completion. Restarting it forever is wrong, and `compose up`
	// must not start it at all: it runs at a release phase or on a schedule,
	// under Onebox's control. A profile keeps it out of the default set.
	if w.Role == "job" {
		svc["restart"] = "no"
		svc["profiles"] = []string{"job"}
	} else {
		svc["restart"] = "unless-stopped"
	}

	return svc, namedVolumes, nil
}

// envFilesFor applies the workload's own list, falling back to the project-wide
// list for applications and workers only. A daemon never receives project-wide
// files: it is a database or a cron runner, not the application, and projecting
// the application's secrets into it was the failure this rule exists to prevent.
func (p *Project) envFilesFor(w Workload) []string {
	if len(w.EnvFiles) > 0 {
		return w.EnvFiles
	}
	if w.Role != "application" && w.Role != "worker" {
		return nil
	}
	if p.Runtime == nil {
		return nil
	}
	return p.Runtime.EnvFiles
}

// routeLabels emits the exact routing keys the overlay contract enumerates.
func (p *Project) routeLabels(n Names, name string, w Workload) map[string]any {
	routes := w.NormalisedRoutes()
	if len(routes) == 0 || !p.Proxy.Managed || p.Proxy.Kind == "none" {
		return nil
	}
	out := map[string]any{
		"traefik.enable":         "true",
		"traefik.docker.network": p.Proxy.Network,
	}
	svcName := n.ProxyService(name)
	for i, r := range routes {
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
			// Without a resolver the router terminates TLS with no certificate
			// to terminate it with.
			if p.Proxy.CertResolver != "" && r.TLS == "terminate" {
				out[pre+"tls.certresolver"] = p.Proxy.CertResolver
			}
		}
		sp := fmt.Sprintf("traefik.%s.services.%s.loadbalancer.server.", kind, svcName)
		out[sp+"port"] = fmt.Sprint(r.Port)
		if kind == "http" && r.Scheme != "" && r.Scheme != "http" {
			out[sp+"scheme"] = r.Scheme
		}
	}
	return out
}

func (p *Project) routesAnywhere() bool {
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
func healthcheck(h *Health) map[string]any {
	if h == nil {
		return nil
	}
	var test []string
	switch {
	case h.Exec != "":
		test = []string{"CMD-SHELL", h.Exec}
	case h.HTTP != "":
		url := fmt.Sprintf("http://127.0.0.1:%d%s", h.Port, h.HTTP)
		test = []string{"CMD-SHELL",
			fmt.Sprintf("curl -fsS %s || wget -qO- %s || exit 1", url, url)}
	case h.TCP:
		test = []string{"CMD-SHELL", fmt.Sprintf("nc -z 127.0.0.1 %d || exit 1", h.Port)}
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
