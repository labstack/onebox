package app

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
	"gopkg.in/yaml.v3"
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
func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errf("project_unreadable", path, "ob init", "cannot read project file: %v", err)
	}
	return LoadBytes(b, path)
}

// LoadBytes runs the fixed pipeline: parse, expand, validate, then apply the
// cross-field rules the schema cannot express.
func LoadBytes(b []byte, filename string) (*Spec, error) {
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

	// Line numbers come from the document as authored: the only form whose
	// lines mean anything to the person reading the error.
	var doc yaml.Node
	lines := map[string]int{}
	if yaml.Unmarshal(b, &doc) == nil {
		lines = lineIndex(&doc)
	}

	if err := checkAPIVersion(raw); err != nil {
		return nil, err
	}
	app, _ := raw["app"].(string)
	derived, err := expand(raw, app)
	if err != nil {
		return nil, err
	}
	if err := checkShape(raw, lines); err != nil {
		return nil, err
	}
	if err := validateSchema(ctx, raw, filename); err != nil {
		return nil, err
	}

	p, err := decode(ctx, raw)
	if err != nil {
		return nil, err
	}
	p.Dir = filepath.Dir(filename)
	defaultProxyManagement(p, raw, derived)
	p.captureRaw(raw, derived)
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
func expand(raw map[string]any, app string) (map[string]Origin, error) {
	var present []string
	for _, k := range shorthandKeys {
		if _, ok := raw[k]; ok {
			present = append(present, k)
		}
	}
	wl, hasBlock := raw["workloads"]

	// Paths whose value the author did not write where it now appears.
	derived := map[string]Origin{}

	if len(present) > 0 && hasBlock {
		return nil, errf("shorthand_and_workloads", "workloads", "",
			"top-level %s cannot be combined with a workloads block; move them into it",
			strings.Join(present, ", "))
	}
	if len(present) > 0 {
		if app == "" {
			return nil, errf("app_required", "app", "", "app is required")
		}
		single := map[string]any{}
		for _, k := range present {
			single[k] = raw[k]
			delete(raw, k)
			derived["workloads."+app+"."+k] = OriginShorthand
		}
		raw["workloads"] = map[string]any{app: single}
		wl = raw["workloads"]
	}

	workloads, _ := wl.(map[string]any)
	for name, w := range workloads {
		m, ok := w.(map[string]any)
		if !ok {
			return nil, errf("workload_malformed", "workloads."+name, "", "workload must be a mapping")
		}
		if _, ok := m["role"]; !ok {
			// Injected so the schema can discriminate. Recorded as a default so
			// `ob canonical` does not claim the author chose it.
			m["role"] = RoleApplication
			derived["workloads."+name+".role"] = OriginDefault
		}
		expandWorkloadUnions(m)
	}
	expandTopLevelUnions(raw)
	return derived, nil
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
	// Each workload against the one shape its role names, before the document
	// as a whole. A workload is a disjunction over four roles, and when a value
	// fails every branch the validator reports whichever branch it tried first
	// — so a typo in a worker came back as "role: conflicting values
	// \"application\" and \"worker\"", which sends the author to fix the one
	// field that was right.
	if err := validateWorkloadShapes(ctx, schema, raw, filename); err != nil {
		return err
	}
	def := schema.LookupPath(cue.ParsePath("#Config"))
	if def.Err() != nil {
		return errf("internal_schema_broken", "", "", "#Config missing from embedded schema")
	}
	u := def.Unify(ctx.Encode(raw))
	if err := u.Validate(); err != nil {
		return errf("project_invalid", where(err, filename), "", "%v", rewordFirst(err))
	}
	if _, err := u.MarshalJSON(); err != nil {
		return errf("project_invalid", incompletePath(err, filename), "", "%v", reword(err))
	}
	return nil
}

func decode(ctx *cue.Context, raw map[string]any) (*Spec, error) {
	schema := ctx.CompileString(schemaSrc, cue.Filename("onebox-schema.cue"))
	def := schema.LookupPath(cue.ParsePath("#Config"))
	b, err := def.Unify(ctx.Encode(raw)).MarshalJSON()
	if err != nil {
		return nil, errf("project_invalid", "", "", "%v", reword(err))
	}
	var p Spec
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, errf("internal_decode_failed", "", "", "cannot decode normalised project: %v", err)
	}
	return &p, nil
}

// defaultProxyManagement decides whether Onebox runs a proxy when the author
// did not say.
//
// `kind` and `managed` answer different questions. `kind` is what routes —
// which decides whether routing labels and a proxy network are generated at
// all. `managed` is who runs it: Onebox, or an operator with their own Traefik
// already reading Docker labels. Conflating them meant `managed: false` threw
// the routes away, and a project that declared a domain deployed something
// nothing could reach, silently.
//
// The schema cannot express this: it depends on whether anything is routed,
// which is a different part of the document. Defaulting to managed
// unconditionally would demand a running Traefik before a project that
// publishes nothing over HTTP — a worker and a database, say — could deploy at
// all, and the operator would be bootstrapping a proxy to route zero requests.
// An explicit `proxy.managed` always wins in either direction.
func defaultProxyManagement(p *Spec, raw map[string]any, derived map[string]Origin) {
	if proxy, ok := raw["proxy"].(map[string]any); ok {
		if _, stated := proxy["managed"]; stated {
			return
		}
	}
	p.Proxy.Managed = p.routesAnywhere()
	derived["proxy.managed"] = OriginDefault
}

// validateWorkloadShapes checks each workload against the definition its
// declared role selects, so the failure names the field that is wrong.
func validateWorkloadShapes(ctx *cue.Context, schema cue.Value, raw map[string]any, filename string) error {
	workloads, ok := raw["workloads"].(map[string]any)
	if !ok {
		return nil
	}
	byRole := map[string]string{
		RoleApplication: "#WorkloadApplication",
		RoleWorker:      "#WorkloadWorker",
		RoleDaemon:      "#WorkloadDaemon",
		RoleJob:         "#WorkloadJob",
	}
	for _, name := range sortedKeys(workloads) {
		body, ok := workloads[name].(map[string]any)
		if !ok {
			continue
		}
		role, _ := body["role"].(string)
		defName, known := byRole[role]
		if !known {
			// An unknown role is the document-level validator's to report; it
			// names the roles that exist.
			continue
		}
		def := schema.LookupPath(cue.ParsePath(defName))
		if def.Err() != nil {
			return errf("internal_schema_broken", "", "", "%s missing from embedded schema", defName)
		}
		u := def.Unify(ctx.Encode(body))
		if err := u.Validate(); err != nil {
			msg := rewordFirst(err)
			// The validator names the definition it was checking against;
			// nobody writing a project knows what a workloadWorker is.
			for _, leak := range []string{"workloadApplication.", "workloadWorker.", "workloadDaemon.", "workloadJob."} {
				msg = strings.ReplaceAll(msg, leak, "workloads."+name+".")
			}
			if hint := nearMiss(msg, def); hint != "" {
				msg += "; did you mean " + hint + "?"
			}
			return errf("project_invalid", "workloads."+name, "", "%s", msg)
		}
	}
	return nil
}

// crossFieldRules applies what the schema cannot express: cardinality, source
// and routing exclusivity, identifier uniqueness, and derived-name length.
func crossFieldRules(p *Spec) error {
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

		for i, n := range w.Needs {
			dep, isWorkload := p.Workloads[n.Name]
			if !isWorkload {
				if _, isService := p.Services[n.Name]; !isService {
					return errf("unknown_prerequisite", path+".needs", "",
						"workload %q needs %q, which is neither a workload nor a service", name, n.Name)
				}
			}

			// Resolve the condition against what the dependency can actually
			// offer. Asking to wait for health from something with no health
			// check is not a stricter guarantee, it is an unstartable runtime.
			//
			// A managed service is not opaque: Onebox wrote its health check
			// and knows whether the driver has one, so a wait on a driver that
			// cannot report health — nats carries no shell to run one — is
			// refused here rather than hanging on the target.
			hasHealth := isWorkload && dep.Health != nil
			if !isWorkload {
				hasHealth = p.serviceHasHealth(n.Name)
			}
			// A Compose-referenced dependency may declare a health check in the
			// file it references, which the declaration cannot see. Trusting the
			// author there is the only honest option; refusing would reject
			// correct projects on missing information.
			opaque := isWorkload && dep.Compose != ""

			switch n.Condition {
			case "":
				if hasHealth || opaque {
					w.Needs[i].Condition = "healthy"
				} else {
					w.Needs[i].Condition = "started"
				}
			case "healthy":
				if !hasHealth && !opaque {
					return errf("prerequisite_has_no_health", path+".needs", "",
						"workload %q waits for %q to become healthy, but %q declares no health check; "+
							"declare one, or wait for it to have started instead",
						name, n.Name, n.Name)
				}
			}
		}
	}

	if err := checkRouteCollisions(p); err != nil {
		return err
	}
	if p.Proxy.Kind == "none" {
		for _, name := range sortedKeys(p.Workloads) {
			if len(p.Workloads[name].NormalisedRoutes()) > 0 {
				return errf("route_without_proxy", "workloads."+name+".routes", "",
					"workload %q declares a route but proxy.kind is \"none\", so nothing would route it; "+
						"remove the route, or name the proxy that serves it", name)
			}
		}
	}

	for _, name := range sortedKeys(p.Services) {
		if _, clash := p.Workloads[name]; clash {
			return errf("identifier_collision", "services."+name, "",
				"%q names both a workload and a service; their derived volume names would collide", name)
		}
		svc := p.Services[name]
		key, d, known := driverOf(name, svc)
		if !known {
			return errf("unknown_service_driver", "services."+name, "",
				"no managed driver named %q; Onebox runs these: %s.\n"+
					"To run something else, declare it as a daemon workload — you own the image and the settings then.",
				key, strings.Join(DriverNames(), ", "))
		}
		// Materialise the durable volume in the project rather than only in the
		// generated runtime, so the canonical form, the preflight collision
		// check and the renderer all name the same volume.
		if d.dataPath != "" && len(svc.Volumes) == 0 {
			svc.Volumes = []string{"data"}
			p.Services[name] = svc
			p.derivedPaths["services."+name+".volumes"] = OriginDefault
		}
	}

	return checkDerivedNames(p)
}

// checkRouteCollisions refuses two workloads claiming the same address.
//
// The proxy would accept both and route to one of them, chosen by a rule the
// author never wrote. That failure is invisible: every container is healthy,
// every check passes, and half the traffic goes somewhere unintended. Naming
// both workloads at load time costs one error message and saves an outage
// nobody can explain.
//
// The address is entrypoint, protocol, domain and path together, because two
// routes differing in any of them are genuinely distinct — the same host on
// two listeners is how a project serves HTTP and gRPC side by side.
func checkRouteCollisions(p *Spec) error {
	type claim struct{ workload string }
	seen := map[string]claim{}
	for _, name := range sortedKeys(p.Workloads) {
		for _, r := range p.Workloads[name].NormalisedRoutes() {
			key := r.Entrypoint + " " + r.Protocol + " " + r.Domain + r.Path
			if prev, taken := seen[key]; taken {
				return errf("route_collision", "workloads."+name+".routes", "",
					"workloads %q and %q both claim %s on the %q entrypoint; "+
						"the proxy would route to one of them and nothing would say which",
					prev.workload, name, r.Domain+r.Path, r.Entrypoint)
			}
			seen[key] = claim{workload: name}
		}
	}
	return nil
}

// checkDerivedNames refuses an over-long generated name rather than truncating.
func checkDerivedNames(p *Spec) error {
	check := func(kind, name string) error {
		if len(name) <= maxDerivedName {
			return nil
		}
		return errf("derived_name_too_long", name, "",
			"derived %s name %q is %d characters, over the %d-character limit; shorten the identifiers",
			kind, name, len(name), maxDerivedName)
	}
	for _, w := range sortedKeys(p.Workloads) {
		if err := check("container", p.Name+"_"+w); err != nil {
			return err
		}
		for _, v := range p.Workloads[w].Volumes {
			if v.IsBind() {
				continue
			}
			if err := check("volume", "ob_"+p.Name+"_"+w+"_"+v.Name); err != nil {
				return err
			}
		}
	}
	for _, s := range sortedKeys(p.Services) {
		if err := check("service project", "ob_"+p.Name+"_"+s); err != nil {
			return err
		}
		for _, v := range p.Services[s].Volumes {
			if err := check("volume", "ob_"+p.Name+"_"+s+"_"+v); err != nil {
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

// where reports the field a validation error is about. Pointing at the file is
// useless in a project of any size; the validator knows the path, so use it.
func where(err error, fallback string) string {
	list := cueerrors.Errors(err)
	if len(list) == 0 {
		return fallback
	}
	if e := mostSpecific(list); e != nil {
		if path := e.Path(); len(path) > 0 {
			return strings.TrimPrefix(strings.Join(path, "."), "#Config.")
		}
	}
	if pos := list[0].Position(); pos.Filename() != "" && pos.Line() > 0 {
		return fmt.Sprintf("%s:%d", pos.Filename(), pos.Line())
	}
	return fallback
}

// incompletePath pulls the field out of a marshal failure, so the error points
// at the declaration rather than at the file.
func incompletePath(err error, fallback string) string {
	s := strings.ReplaceAll(err.Error(), "#Config.", "")
	s = strings.ReplaceAll(s, "cue: marshal error: ", "")
	if i := strings.Index(s, ": cannot convert incomplete value"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return fallback
}

// reword keeps the validation language out of user-facing errors.
// nearMiss suggests the field an author probably meant. A rejected name is
// most often a typo or a plural away from a real one, and the difference
// between "field not allowed" and "did you mean replicas?" is one round trip
// against a schema nobody has memorised.
func nearMiss(msg string, def cue.Value) string {
	const marker = ": field not allowed"
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	path := msg[:i]
	typo := path[strings.LastIndex(path, ".")+1:]
	typo = strings.Trim(typo, `"`)
	if typo == "" {
		return ""
	}
	best, bestDist := "", 3 // farther than this is a different word, not a typo
	iter, err := def.Fields(cue.Optional(true), cue.Definitions(false))
	if err != nil {
		return ""
	}
	for iter.Next() {
		candidate := iter.Selector().Unquoted()
		if d := editDistance(typo, candidate); d < bestDist {
			best, bestDist = candidate, d
		}
	}
	return best
}

// editDistance is Levenshtein, bounded by the caller's threshold rather than
// here: the field sets are small enough that the simple form is the honest one.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			best := cur[j-1] + 1
			if prev[j]+1 < best {
				best = prev[j] + 1
			}
			if prev[j-1]+cost < best {
				best = prev[j-1] + cost
			}
			cur[j] = best
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func reword(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "#Config.", "")
	s = strings.ReplaceAll(s, "#Workload", "workload")
	s = strings.ReplaceAll(s, "#Config", "project")
	s = strings.ReplaceAll(s, "cue: marshal error: ", "")
	// An incomplete value at a known path is almost always a required field
	// nobody filled in. Say that, rather than reporting how the validator felt.
	if i := strings.Index(s, "cannot convert incomplete value"); i >= 0 {
		if path := strings.TrimSuffix(strings.TrimSpace(s[:i]), ":"); path != "" {
			return fmt.Sprintf("%s is required, or its value does not match any accepted form", path)
		}
		return "a required value is missing"
	}
	return firstLine(s)
}

func rewordFirst(err error) string {
	list := cueerrors.Errors(err)
	if len(list) == 0 {
		return firstLine(err.Error())
	}
	if e := mostSpecific(list); e != nil {
		return reword(e)
	}
	return reword(list[0])
}

// mostSpecific picks the error a person can act on.
//
// A failed disjunction reports a summary — "6 errors in empty disjunction" —
// followed by the real reasons. The summary is the validator explaining itself,
// not the project explaining what is wrong, so prefer the deepest concrete
// error underneath it.
func mostSpecific(list []cueerrors.Error) cueerrors.Error {
	var best cueerrors.Error
	bestDepth := -1
	for _, e := range list {
		if strings.Contains(e.Error(), "disjunction") {
			continue
		}
		if d := len(e.Path()); d > bestDepth {
			best, bestDepth = e, d
		}
	}
	return best
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
