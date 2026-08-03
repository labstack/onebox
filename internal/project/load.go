package project

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueerrors "cuelang.org/go/cue/errors"
	cueyaml "cuelang.org/go/encoding/yaml"
)

//go:embed schema.cue
var schemaSrc string

// APIVersion is the only authoring contract this package accepts.
const APIVersion = "onebox.run/v1"

// maxDerivedName is an Onebox limit chosen for headroom, not a container-runtime
// maximum. An over-long name is refused rather than truncated: truncation with a
// hash suffix is not injective, and a volume-name collision means one declared
// resource silently mounting another's data.
const maxDerivedName = 63

// Error is a typed loader failure. Every failure carries a code an agent can
// branch on and, where one exists, the command that resolves it.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Next    string `json:"next,omitempty"`
}

func (e *Error) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Path)
}

func errf(code, path, next, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Path: path, Next: next}
}

// Load reads and normalises a project file.
func Load(path string) (*Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errf("project_unreadable", path, "ob init", "cannot read project file: %v", err)
	}
	return LoadBytes(b, path)
}

// LoadBytes runs the fixed pipeline: parse, expand, validate, then apply the
// cross-field rules the schema cannot express.
func LoadBytes(b []byte, filename string) (*Project, error) {
	var raw map[string]any
	f, err := cueyaml.Extract(filename, b)
	if err != nil {
		return nil, errf("project_unparsable", filename, "", "invalid YAML: %v", rewordFirst(err))
	}
	ctx := cuecontext.New()
	v := ctx.BuildFile(f)
	if v.Err() != nil {
		return nil, errf("project_unparsable", filename, "", "invalid YAML: %v", rewordFirst(v.Err()))
	}
	if err := v.Decode(&raw); err != nil {
		return nil, errf("project_unparsable", filename, "", "project is not a mapping")
	}

	if err := checkAPIVersion(raw); err != nil {
		return nil, err
	}
	app, _ := raw["app"].(string)
	if err := expand(raw, app); err != nil {
		return nil, err
	}
	if err := validateSchema(ctx, raw, filename); err != nil {
		return nil, err
	}

	p, err := decode(ctx, raw)
	if err != nil {
		return nil, err
	}
	if err := crossFieldRules(p); err != nil {
		return nil, err
	}
	return p, nil
}

func checkAPIVersion(raw map[string]any) error {
	got, ok := raw["api_version"].(string)
	switch {
	case !ok || got == "":
		return errf("schema_identity_missing", "api_version", "ob init",
			"api_version is required; this binary accepts %q", APIVersion)
	case got != APIVersion:
		return errf("schema_identity_unsupported", "api_version", "",
			"unsupported api_version %q; this binary accepts %q", got, APIVersion)
	}
	if _, ok := raw["components"]; ok {
		return errf("withdrawn_components_block", "components", "",
			"the components block was withdrawn; declare workloads and services instead")
	}
	return nil
}

// shorthandKeys are the top-level fields that describe a single workload.
var shorthandKeys = []string{"build", "image", "compose", "port", "health", "domain", "routes"}

// expand rewrites shorthand into the normalised form the schema validates. It
// runs before validation because the schema requires discriminators — a role
// left absent would keep every branch of the workload disjunction alive.
func expand(raw map[string]any, app string) error {
	var present []string
	for _, k := range shorthandKeys {
		if _, ok := raw[k]; ok {
			present = append(present, k)
		}
	}
	wl, hasBlock := raw["workloads"]

	if len(present) > 0 && hasBlock {
		return errf("shorthand_and_workloads", "workloads", "",
			"top-level %s cannot be combined with a workloads block; move them into it",
			strings.Join(present, ", "))
	}
	if len(present) > 0 {
		if app == "" {
			return errf("app_required", "app", "", "app is required")
		}
		single := map[string]any{}
		for _, k := range present {
			single[k] = raw[k]
			delete(raw, k)
		}
		raw["workloads"] = map[string]any{app: single}
		wl = raw["workloads"]
	}

	workloads, _ := wl.(map[string]any)
	for name, w := range workloads {
		m, ok := w.(map[string]any)
		if !ok {
			return errf("workload_malformed", "workloads."+name, "", "workload must be a mapping")
		}
		if _, ok := m["role"]; !ok {
			m["role"] = "application"
		}
		expandWorkloadUnions(m)
	}
	expandTopLevelUnions(raw)
	return nil
}

// expandWorkloadUnions turns every scalar shorthand inside a workload into its
// object form, so that decoding never has to handle two shapes per field.
func expandWorkloadUnions(m map[string]any) {
	if s, ok := m["build"].(string); ok {
		m["build"] = map[string]any{"context": s}
	}
	if s, ok := m["image"].(string); ok {
		m["image"] = map[string]any{"reference": s}
	}
	if s, ok := m["health"].(string); ok {
		m["health"] = map[string]any{"http": s}
	}
	if s, ok := m["command"].(string); ok {
		m["command"] = map[string]any{"run": s}
	}
	if vs, ok := m["volumes"].([]any); ok {
		for i, v := range vs {
			if s, ok := v.(string); ok {
				vs[i] = map[string]any{"name": s}
			}
		}
	}
	if ns, ok := m["needs"].([]any); ok {
		for i, n := range ns {
			if s, ok := n.(string); ok {
				ns[i] = map[string]any{"name": s}
			}
		}
	}
}

func expandTopLevelUnions(raw map[string]any) {
	if envs, ok := raw["environments"].(map[string]any); ok {
		for _, e := range envs {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := em["server"].(string); ok {
				host, user := s, ""
				if at := strings.Index(s, "@"); at >= 0 {
					user, host = s[:at], s[at+1:]
				}
				em["server"] = map[string]any{"host": host}
				if user != "" {
					em["server"].(map[string]any)["user"] = user
				}
			}
		}
	}
	if secrets, ok := raw["secrets"].(map[string]any); ok {
		for k, s := range secrets {
			if str, ok := s.(string); ok {
				secrets[k] = map[string]any{"file": str}
			}
		}
	}
	if hooks, ok := raw["hooks"].(map[string]any); ok {
		for k, h := range hooks {
			if str, ok := h.(string); ok {
				hooks[k] = map[string]any{"run": str}
			}
		}
	}
	if services, ok := raw["services"].(map[string]any); ok {
		for k, s := range services {
			// Any non-mapping is the scalar version shorthand. Matching on
			// concrete numeric types missed whatever the YAML decoder chose.
			if _, isMap := s.(map[string]any); !isMap {
				services[k] = map[string]any{"version": s}
			}
		}
	}
}

func validateSchema(ctx *cue.Context, raw map[string]any, filename string) error {
	schema := ctx.CompileString(schemaSrc, cue.Filename("onebox-schema.cue"))
	if schema.Err() != nil {
		return errf("internal_schema_broken", "", "", "embedded schema is broken: %v", schema.Err())
	}
	def := schema.LookupPath(cue.ParsePath("#Config"))
	if def.Err() != nil {
		return errf("internal_schema_broken", "", "", "#Config missing from embedded schema")
	}
	u := def.Unify(ctx.Encode(raw))
	if err := u.Validate(); err != nil {
		return errf("project_invalid", filename, "", "%v", rewordFirst(err))
	}
	if _, err := u.MarshalJSON(); err != nil {
		return errf("project_invalid", filename, "", "%v", reword(err))
	}
	return nil
}

func decode(ctx *cue.Context, raw map[string]any) (*Project, error) {
	schema := ctx.CompileString(schemaSrc, cue.Filename("onebox-schema.cue"))
	def := schema.LookupPath(cue.ParsePath("#Config"))
	b, err := def.Unify(ctx.Encode(raw)).MarshalJSON()
	if err != nil {
		return nil, errf("project_invalid", "", "", "%v", reword(err))
	}
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, errf("internal_decode_failed", "", "", "cannot decode normalised project: %v", err)
	}
	return &p, nil
}

// crossFieldRules applies what the schema cannot express: cardinality, source
// and routing exclusivity, identifier uniqueness, and derived-name length.
func crossFieldRules(p *Project) error {
	if len(p.Environments) == 0 {
		return errf("no_environment", "environments", "", "at least one environment is required")
	}
	if len(p.Workloads) == 0 {
		return errf("no_workload", "workloads", "",
			"at least one workload is required; declare one or use the top-level shorthand")
	}

	for _, name := range sortedKeys(p.Workloads) {
		w := p.Workloads[name]
		path := "workloads." + name

		sources := 0
		for _, has := range []bool{w.Build != nil, w.Image != nil, w.Compose != ""} {
			if has {
				sources++
			}
		}
		if sources != 1 {
			return errf("workload_source", path, "",
				"workload %q must declare exactly one of build, image, or compose (found %d)", name, sources)
		}

		hasScalar := w.Domain != "" || w.Port != 0
		if hasScalar && len(w.Routes) > 0 {
			return errf("routing_exclusive", path, "",
				"workload %q declares both the domain/port shorthand and routes; use one", name)
		}
		if (w.Domain == "") != (w.Port == 0) {
			return errf("routing_incomplete", path, "",
				"workload %q must declare domain and port together", name)
		}

		for _, n := range w.Needs {
			if _, ok := p.Workloads[n.Name]; ok {
				continue
			}
			if _, ok := p.Services[n.Name]; ok {
				continue
			}
			return errf("unknown_prerequisite", path+".needs", "",
				"workload %q needs %q, which is neither a workload nor a service", name, n.Name)
		}
	}

	for _, name := range sortedKeys(p.Services) {
		if _, clash := p.Workloads[name]; clash {
			return errf("identifier_collision", "services."+name, "",
				"%q names both a workload and a service; their derived volume names would collide", name)
		}
	}

	return checkDerivedNames(p)
}

// checkDerivedNames refuses an over-long generated name rather than truncating.
func checkDerivedNames(p *Project) error {
	check := func(kind, name string) error {
		if len(name) <= maxDerivedName {
			return nil
		}
		return errf("derived_name_too_long", name, "",
			"derived %s name %q is %d characters, over the %d-character limit; shorten the identifiers",
			kind, name, len(name), maxDerivedName)
	}
	for _, w := range sortedKeys(p.Workloads) {
		if err := check("container", p.App+"_"+w); err != nil {
			return err
		}
		for _, v := range p.Workloads[w].Volumes {
			if v.IsBind() {
				continue
			}
			if err := check("volume", "ob_"+p.App+"_"+w+"_"+v.Name); err != nil {
				return err
			}
		}
	}
	for _, s := range sortedKeys(p.Services) {
		if err := check("service project", "ob_"+p.App+"_"+s); err != nil {
			return err
		}
		for _, v := range p.Services[s].Volumes {
			if err := check("volume", "ob_"+p.App+"_"+s+"_"+v); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reword keeps the validation language out of user-facing errors.
func reword(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "#Config.", "")
	s = strings.ReplaceAll(s, "cue: marshal error: ", "")
	if i := strings.Index(s, "cannot convert incomplete value"); i >= 0 {
		return strings.TrimSpace(s[:i]) + "is incomplete or ambiguous"
	}
	return firstLine(s)
}

func rewordFirst(err error) string {
	list := cueerrors.Errors(err)
	if len(list) == 0 {
		return firstLine(err.Error())
	}
	return reword(list[0])
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// Dir returns the directory a repository path resolves against.
func Dir(projectFile string) string { return filepath.Dir(projectFile) }
