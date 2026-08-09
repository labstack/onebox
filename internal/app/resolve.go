package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Resolution applies one environment's overrides to the project. Everything
// downstream — generation, planning, `ob canonical` — works from a resolved
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
		// Which files a workload reads cannot change which artifact runs or what
		// it does to data. Without it a workload declaring its own list — which
		// every daemon holding a credential must — is pinned to one
		// environment's values across all of them.
		"env_files": true,
	}
	overridableService = map[string]bool{
		"resources": true, "settings": true, "protection": true,
	}
)

// Origin records where a resolved value came from.
type Origin string

const (
	OriginAuthored            Origin = "authored"
	OriginDefault             Origin = "default"
	OriginEnvironmentOverride Origin = "environment-override"
	OriginObserved            Origin = "observed"
	OriginDerived             Origin = "derived"

	// Compatibility names retain the Go API while the public values use the
	// vocabulary exposed by canonical output.
	OriginExplicit = OriginAuthored
	OriginOverride = OriginEnvironmentOverride
)

// Resolved is a project with one environment's overrides applied.
type Resolved struct {
	*Spec

	// Env is the environment this was resolved for.
	Env string

	// Origins records the paths an override changed, so a reader can tell a
	// staging value from a project-level one without diffing two files.
	Origins map[string]Origin

	canonicalFacts *CanonicalFacts
	serviceRuntime map[string]ServiceRuntimeState
}

// Resolve applies the named environment's overrides and returns a copy. The
// receiver is not modified: resolving twice for two environments must not let
// one leak into the other.
func (p *Spec) Resolve(env string) (*Resolved, error) {
	e, ok := p.Environments[env]
	if !ok {
		return nil, errf("unknown_environment", "environments."+env, "",
			"environment %q is not declared", env)
	}

	clone, err := p.deepCopy()
	if err != nil {
		return nil, err
	}
	// The selected environment's default travels with the clone, so generation
	// resolves a workload's list without having to be told which environment it
	// is rendering.
	clone.envDefault = e.EnvFiles
	out := &Resolved{Spec: clone, Env: env, Origins: map[string]Origin{}}
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
		authoredPatch := patch
		s, ok := clone.Services[name]
		if !ok {
			return nil, errf("override_unknown_service",
				"environments."+env+".overrides.services."+name, "",
				"environment %q overrides service %q, which the project does not declare", env, name)
		}
		patch, err = prepareServiceOverride("environments."+env+".overrides.services."+name, s, patch)
		if err != nil {
			return nil, err
		}
		merged, err := applyPatch("services."+name, s, patch, overridableService, out.Origins)
		if err != nil {
			return nil, err
		}
		if _, protected := authoredPatch["protection"]; protected {
			prefix := "services." + name + ".protection"
			for originPath := range out.Origins {
				if originPath == prefix || strings.HasPrefix(originPath, prefix+".") {
					delete(out.Origins, originPath)
				}
			}
			markOverrideValue(prefix, authoredPatch["protection"], out.Origins)
		}
		clone.Services[name] = merged
	}

	// The resolved project is what every downstream consumer sees, so it has
	// to satisfy the same contract the authored one does. Merging alone only
	// decodes into the struct: an override could otherwise produce
	// `replicas: 0`, an unknown strategy, or a route that collides with
	// another workload's — each of which is refused when written directly in
	// the project, and each of which reached generation unexamined.
	if err := validateSpec(clone); err != nil {
		return nil, overrideError(env, err)
	}
	if err := crossFieldRules(clone); err != nil {
		return nil, overrideError(env, err)
	}

	return out, nil
}

// overrideError says where the offending value came from. A refusal naming
// `workloads.web.replicas` sends the reader to a line that is correct; the
// value they need to change is in the environment's overrides.
func overrideError(env string, err error) error {
	var e *Error
	if errors.As(err, &e) {
		return errf(e.Code, "environments."+env+".overrides."+e.Path, e.Next,
			"%s (the project is valid; this comes from %q's overrides)", e.Message, env)
	}
	return err
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
	// `omitempty` erases a declared empty list here too, so overriding any
	// other field of a workload silently converted "receives nothing" back into
	// "did not say". The patch may legitimately declare its own empty list, so
	// this only restores what the target had and lets the patch overwrite it
	// below.
	if w, ok := any(target).(Workload); ok && w.EnvFiles != nil && len(w.EnvFiles) == 0 {
		raw["env_files"] = []any{}
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
		markOverrideValue(path+"."+key, value, origins)
	}

	var out T
	if err := fromGeneric(raw, &out); err != nil {
		return zero, errf("override_invalid", path, "",
			"override produced a value the contract does not accept: %v", firstLine(err.Error()))
	}
	return out, nil
}

// markOverrideValue records the leaves an override actually set.
//
// Marking the block instead, and expanding that to every leaf beneath it when
// printing, annotated values the override never touched: an override of
// `resources: {memory: 1GB}` made `ob canonical` label a sibling `cpus` the
// project declared as an environment override. The one command whose job is to
// say where a value came from must not guess.
func markOverrideValue(path string, value any, origins map[string]Origin) {
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			origins[path] = OriginEnvironmentOverride
			return
		}
		for _, key := range sortedKeys(v) {
			markOverrideValue(path+"."+key, v[key], origins)
		}
	case []any:
		if len(v) == 0 {
			origins[path] = OriginEnvironmentOverride
			return
		}
		for i, item := range v {
			markOverrideValue(fmt.Sprintf("%s[%d]", path, i), item, origins)
		}
	default:
		origins[path] = OriginEnvironmentOverride
	}
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

// deepCopy round-trips through JSON, which is exact for everything except an
// explicitly empty list: `omitempty` drops it on the way out and it returns as
// absent. Those two states mean different things here — "receives none" versus
// "did not say" — so the empties are recorded before the copy and restored
// after it.
func (p *Spec) deepCopy() (*Spec, error) {
	declaredEmpty := map[string]bool{}
	for name, w := range p.Workloads {
		if w.EnvFiles != nil && len(w.EnvFiles) == 0 {
			declaredEmpty[name] = true
		}
	}
	// Every scope that can declare a list can declare an empty one, and every
	// one of them loses it to `omitempty`. Restoring only workloads meant a
	// project or environment declaring "the default is none" silently became
	// "did not say" — the same defect, in the two scopes nobody checked.
	runtimeEmpty := p.Runtime != nil && p.Runtime.EnvFiles != nil && len(p.Runtime.EnvFiles) == 0
	envEmpty := map[string]bool{}
	for name, e := range p.Environments {
		if e.EnvFiles != nil && len(e.EnvFiles) == 0 {
			envEmpty[name] = true
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, errf("internal_copy_failed", "", "", "cannot copy project: %v", err)
	}
	var out Spec
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, errf("internal_copy_failed", "", "", "cannot copy project: %v", err)
	}
	for name := range declaredEmpty {
		w := out.Workloads[name]
		w.EnvFiles = []EnvFile{}
		out.Workloads[name] = w
	}
	if runtimeEmpty && out.Runtime != nil {
		out.Runtime.EnvFiles = []EnvFile{}
	}
	for name := range envEmpty {
		e := out.Environments[name]
		e.EnvFiles = []EnvFile{}
		out.Environments[name] = e
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

// fromGeneric decodes the merged document back into the typed value, refusing a
// key the contract does not define.
//
// Plain Unmarshal discards unknown fields, and the permitted-key check above
// only guards the top level of a patch. A nested typo — `resources: {memroy:
// 4GB}` — passed `ob validate` and resolved to the project's own value, so the
// environment silently ran unoverridden.
func fromGeneric(m map[string]any, out any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
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
