package project

import (
	"encoding/json"
	"sort"
)

// Resolution applies one environment's overrides to the project. Everything
// downstream — generation, planning, `ob config` — works from a resolved
// project, so nothing has to ask which environment a value came from.
//
// An override may change how much of something runs. It may not change what
// runs, where it came from, or what it does to data: a staging environment that
// can swap the image or the data effect is a different application wearing the
// same name.

// overridableWorkload and overridableService are the closed sets. They are
// enforced here as well as in the schema, because this is the layer that would
// silently apply an unlisted key.
var (
	overridableWorkload = map[string]bool{
		"replicas": true, "resources": true, "env": true, "strategy": true, "routes": true,
	}
	overridableService = map[string]bool{
		"resources": true, "settings": true, "backup": true,
	}
)

// Origin records where a resolved value came from.
type Origin string

const (
	OriginOverride Origin = "override"
	OriginExplicit Origin = "explicit"
	OriginDefault  Origin = "default"
)

// Resolved is a project with one environment's overrides applied.
type Resolved struct {
	*Project

	// Env is the environment this was resolved for.
	Env string

	// Origins records the paths an override changed, so a reader can tell a
	// staging value from a project-level one without diffing two files.
	Origins map[string]Origin
}

// Resolve applies the named environment's overrides and returns a copy. The
// receiver is not modified: resolving twice for two environments must not let
// one leak into the other.
func (p *Project) Resolve(env string) (*Resolved, error) {
	e, ok := p.Environments[env]
	if !ok {
		return nil, errf("unknown_environment", "environments."+env, "",
			"environment %q is not declared", env)
	}

	clone, err := p.deepCopy()
	if err != nil {
		return nil, err
	}
	out := &Resolved{Project: clone, Env: env, Origins: map[string]Origin{}}
	if e.Overrides == nil {
		return out, nil
	}

	for _, name := range sortedKeys(e.Overrides.Workloads) {
		patch := e.Overrides.Workloads[name]
		w, ok := clone.Workloads[name]
		if !ok {
			return nil, errf("override_unknown_workload",
				"environments."+env+".overrides.workloads."+name, "",
				"environment %q overrides workload %q, which the project does not declare", env, name)
		}
		merged, err := applyPatch("workloads."+name, w, patch, overridableWorkload, out.Origins)
		if err != nil {
			return nil, err
		}
		clone.Workloads[name] = merged
	}

	for _, name := range sortedKeys(e.Overrides.Services) {
		patch := e.Overrides.Services[name]
		s, ok := clone.Services[name]
		if !ok {
			return nil, errf("override_unknown_service",
				"environments."+env+".overrides.services."+name, "",
				"environment %q overrides service %q, which the project does not declare", env, name)
		}
		merged, err := applyPatch("services."+name, s, patch, overridableService, out.Origins)
		if err != nil {
			return nil, err
		}
		clone.Services[name] = merged
	}

	return out, nil
}

// applyPatch merges an override into one workload or service.
//
// A scalar or list replaces wholesale; a mapping merges key by key so a single
// setting can change without restating the rest; a null removes a key. Anything
// outside the closed set is refused by name, with the permitted set listed —
// guessing is what turns an override system into a second configuration
// language.
func applyPatch[T any](path string, target T, patch map[string]any, allowed map[string]bool, origins map[string]Origin) (T, error) {
	var zero T

	raw, err := toGeneric(target)
	if err != nil {
		return zero, err
	}

	for _, key := range sortedKeys(patch) {
		if !allowed[key] {
			return zero, errf("override_not_permitted", path+"."+key, "",
				"%q may not be overridden per environment; permitted here: %s",
				key, joinSorted(allowed))
		}
		value := patch[key]
		switch {
		case value == nil:
			delete(raw, key)
		case isMapping(value) && isMapping(raw[key]):
			raw[key] = mergeMapping(raw[key].(map[string]any), value.(map[string]any))
		default:
			raw[key] = value
		}
		origins[path+"."+key] = OriginOverride
	}

	var out T
	if err := fromGeneric(raw, &out); err != nil {
		return zero, errf("override_invalid", path, "",
			"override produced a value the contract does not accept: %v", firstLine(err.Error()))
	}
	return out, nil
}

// mergeMapping merges one level, with a null member removing that member. One
// level is deliberate: deeper merging cannot be predicted while reading, which
// is the failure every layered configuration system eventually produces.
func mergeMapping(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

func isMapping(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func (p *Project) deepCopy() (*Project, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, errf("internal_copy_failed", "", "", "cannot copy project: %v", err)
	}
	var out Project
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, errf("internal_copy_failed", "", "", "cannot copy project: %v", err)
	}
	out.Dir = p.Dir
	// Without these a resolved project has no memory of what was authored, and
	// would report every value as a default.
	out.rawExpanded = p.rawExpanded
	out.derivedPaths = p.derivedPaths
	return &out, nil
}

func toGeneric(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errf("internal_copy_failed", "", "", "%v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, errf("internal_copy_failed", "", "", "%v", err)
	}
	return out, nil
}

func fromGeneric(m map[string]any, out any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func joinSorted(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}
