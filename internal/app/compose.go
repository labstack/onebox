package app

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The Compose reference is the bounded escape hatch: a workload the declaration
// cannot express is authored in Compose and referenced as `file#service`. Onebox
// copies that service verbatim and adds an enumerated set of keys — nothing
// else — so a reader can predict exactly what changes.
//
// The file is parsed as plain YAML rather than through the Compose loader.
// Loading would interpolate variables, follow `extends` and `include`, and
// validate the whole file, so a project would fail on an unrelated service's
// missing variable. Referencing one service should depend only on that service.

// overlayKeys is the closed set. Anything outside it is copied untouched.
type overlay struct {
	Network  string         // ingress network to append, empty when the proxy is off
	Labels   map[string]any // ob.* identity and traefik.* routing
	HasRoute bool           // routes were declared, so traefik.* is ours
}

// mergeComposeRef reads the referenced service and applies the overlay.
func mergeComposeRef(dir, ref string, ov overlay) (map[string]any, definitions, error) {
	file, service, ok := strings.Cut(ref, "#")
	if !ok {
		return nil, definitions{}, errf("compose_ref_malformed", ref, "",
			"compose reference %q must be file#service", ref)
	}

	path, err := resolveRepoPath(dir, file)
	if err != nil {
		return nil, definitions{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, definitions{}, errf("compose_file_unreadable", file, "",
			"cannot read referenced compose file %q: %v", file, err)
	}

	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
		Networks map[string]any            `yaml:"networks"`
		Volumes  map[string]any            `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, definitions{}, errf("compose_file_unparsable", file, "",
			"referenced compose file %q is not valid YAML: %v", file, firstLine(err.Error()))
	}
	svc, ok := doc.Services[service]
	if !ok {
		return nil, definitions{}, errf("compose_service_missing", ref, "",
			"compose file %q declares no service %q (it has: %s)",
			file, service, strings.Join(sortedKeys(doc.Services), ", "))
	}

	if err := refuseConflicts(ref, svc, ov); err != nil {
		return nil, definitions{}, err
	}

	merged := applyOverlay(svc, ov)
	deps := carriedDefinitions(merged, doc.Networks, doc.Volumes, ov.Network)
	return merged, deps, nil
}

// carriedDefinitions collects the top-level network and volume definitions the
// merged service depends on.
//
// Without this the generated runtime references a network or volume it never
// defines. A survey of real projects found 85 declaring non-default network
// topology and 35 with volume driver options — segmentation and NFS mounts that
// would have been dropped or, at best, surfaced as a confusing runtime error.
func carriedDefinitions(svc map[string]any, networks, volumes map[string]any, ingress string) definitions {
	d := definitions{Networks: map[string]any{}, Volumes: map[string]any{}}

	for _, n := range networkNames(svc["networks"]) {
		if n == ingress || n == "default" {
			continue
		}
		if spec, ok := networks[n]; ok {
			d.Networks[n] = orEmpty(spec)
		} else {
			d.Networks[n] = map[string]any{}
		}
	}

	for _, name := range mountedVolumeNames(svc["volumes"]) {
		if spec, ok := volumes[name]; ok {
			d.Volumes[name] = orEmpty(spec)
		} else {
			d.Volumes[name] = map[string]any{}
		}
	}
	return d
}

// definitions are the top-level entries a merged service needs.
type definitions struct {
	Networks map[string]any
	Volumes  map[string]any
}

func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// mountedVolumeNames are the named volumes a service mounts, in both the short
// and the long form.
//
// Compose accepts `data:/var/lib/data` and the equivalent mapping
// `{type: volume, source: data, target: /var/lib/data}`. Reading only strings
// dropped the mapping form silently: the service was copied with a mount whose
// volume was never defined, which Compose then refuses — naming the volume,
// and nothing about where it went.
func mountedVolumeNames(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range items {
		switch m := item.(type) {
		case string:
			name, _, ok := strings.Cut(m, ":")
			// A bind mount defines nothing; an anonymous volume names nothing.
			if ok && !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, ".") {
				out = append(out, name)
			}
		case map[string]any:
			// Only `type: volume` carries a definition. A bind or tmpfs source
			// is a host path or nothing at all.
			if kind, _ := m["type"].(string); kind != "volume" {
				continue
			}
			if source, _ := m["source"].(string); source != "" {
				out = append(out, source)
			}
		}
	}
	return out
}

// refuseConflicts names the key and the file rather than overwriting. Onebox
// never silently discards something the user wrote: a conflict means the
// declaration and the Compose service disagree, and only the author can say
// which is right.
func refuseConflicts(ref string, svc map[string]any, ov overlay) error {
	// extends is not followed: the file is read as plain YAML so that
	// referencing one service cannot fail on another service's missing
	// variable. Silently rendering a service without what it inherits would be
	// worse than refusing, so refuse.
	if _, ok := svc["extends"]; ok {
		return errf("compose_extends", ref, "",
			"referenced service in %q uses extends, which Onebox does not follow; "+
				"inline the inherited settings or declare the workload directly", ref)
	}
	if _, ok := svc["container_name"]; ok {
		return errf("compose_container_name", ref, "",
			"referenced service in %q sets container_name; Onebox owns container naming, so remove it", ref)
	}
	// network_mode and networks cannot coexist in the container runtime. With the
	// proxy off no network is attached, so the key is harmless and preserved.
	if _, ok := svc["network_mode"]; ok && ov.Network != "" {
		return errf("compose_network_mode", ref, "",
			"referenced service in %q sets network_mode, which cannot coexist with the ingress network", ref)
	}
	if nets, ok := svc["networks"]; ok && ov.Network != "" {
		for _, n := range networkNames(nets) {
			if n == ov.Network {
				return errf("compose_ingress_attached", ref, "",
					"referenced service in %q already attaches %q; Onebox adds it", ref, ov.Network)
			}
		}
	}
	for _, k := range sortedKeys(labelMap(svc["labels"])) {
		if strings.HasPrefix(k, "ob.") {
			return errf("compose_ob_label", ref, "",
				"referenced service in %q declares %q; the ob. namespace is Onebox's", ref, k)
		}
		if ov.HasRoute && strings.HasPrefix(k, "traefik.") {
			return errf("compose_traefik_label", ref, "",
				"referenced service in %q declares %q while the workload also declares a route; "+
					"remove the label or the route", ref, k)
		}
	}
	return nil
}

// applyOverlay copies the service and adds exactly the enumerated keys.
func applyOverlay(svc map[string]any, ov overlay) map[string]any {
	out := make(map[string]any, len(svc)+2)
	for k, v := range svc {
		out[k] = v
	}

	if len(ov.Labels) > 0 {
		labels := labelMap(out["labels"])
		merged := make(map[string]any, len(labels)+len(ov.Labels))
		for k, v := range labels {
			merged[k] = v
		}
		for k, v := range ov.Labels {
			merged[k] = v
		}
		out["labels"] = merged
	}

	if ov.Network != "" {
		nets := networkNames(out["networks"])
		if len(nets) == 0 {
			nets = []string{"default"}
		}
		out["networks"] = append(nets, ov.Network)
	}
	return out
}

// labelMap accepts both Compose label forms: a mapping, or a list of `k=v`.
func labelMap(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case []any:
		out := map[string]any{}
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				continue
			}
			k, val, _ := strings.Cut(s, "=")
			out[k] = val
		}
		return out
	}
	return nil
}

func networkNames(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		out := sortedKeys(t)
		return out
	}
	return nil
}

// resolveRepoPath resolves a repository path against the project file's
// directory and refuses one that escapes the repository, including through a
// symbolic link. The lexical check is not enough: `a/../../etc` is legal to
// write and resolves outside.
func resolveRepoPath(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", errf("path_absolute", rel, "",
			"%q must be a repository-relative path", rel)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", errf("path_unresolvable", rel, "", "cannot resolve %q: %v", rel, err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	joined := filepath.Join(root, rel)
	if resolved, err := filepath.EvalSymlinks(joined); err == nil {
		joined = resolved
	}
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", errf("path_escapes_repository", rel, "",
			"%q resolves outside the project directory", rel)
	}
	return joined, nil
}

// ComposeRefsOf lists every referenced file, so callers can stage them.
func (p *Spec) ComposeRefsOf() []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range sortedKeys(p.Workloads) {
		ref := p.Workloads[name].Compose
		if ref == "" {
			continue
		}
		file, _, _ := strings.Cut(ref, "#")
		if !seen[file] {
			seen[file] = true
			out = append(out, file)
		}
	}
	sort.Strings(out)
	return out
}
