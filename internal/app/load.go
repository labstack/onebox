package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

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
	p, err := LoadBytes(b, path)
	if err != nil {
		return nil, err
	}
	// Only here. LoadBytes answers "are these bytes a valid project"; whether
	// the files it names are on disk is a different question and needs a disk.
	// Folding it into LoadBytes made every byte-based caller — the conformance
	// corpus among them — fail on files that were never meant to exist.
	if err := p.checkDeclaredFilesExist(); err != nil {
		return nil, err
	}
	return p, nil
}

// LoadBytes runs the fixed pipeline: parse, expand, validate, then apply the
// cross-field rules the schema cannot express.
func LoadBytes(b []byte, filename string) (*Spec, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, errf("project_unparsable", filename, "", "invalid YAML: %v", firstLine(err.Error()))
	}
	if raw == nil {
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
	// Before checkShape, deliberately: the contract requires this to fail with
	// direction rather than as an unknown field, and closedness would otherwise
	// answer first with a generic refusal.
	//
	// The block is withdrawn rather than repurposed. Its keys are arbitrary
	// names today, so reading them as environment names would silently change
	// what an existing project means — the failure this contract exists to
	// remove — and leaving it accepted would keep two mechanisms for one idea.
	if _, ok := raw["secrets"]; ok {
		return nil, errf("secrets_withdrawn", "secrets",
			"runtime.env_files: [{file: <path>, provider: sops}]",
			"the `secrets` block is withdrawn: declare the file as an env_files "+
				"entry carrying a provider, at the project, environment or workload "+
				"scope that should receive it")
	}
	if err := checkShape(raw, lines); err != nil {
		return nil, err
	}

	p, err := decodeSpec(raw)
	if err != nil {
		return nil, err
	}
	applyDefaults(p, raw, derived)
	// Before validation, not after: validation stats the files a project
	// declares, and without the directory that check silently passed on every
	// project. A guard that cannot see what it guards is worse than none,
	// because the enumeration then promises a failure that never fires.
	p.Dir = filepath.Dir(filename)
	if err := validateSpec(p); err != nil {
		return nil, err
	}
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
	expandEnvFiles(m)
	if ns, ok := m["needs"].([]any); ok {
		for i, n := range ns {
			if s, ok := n.(string); ok {
				ns[i] = map[string]any{"name": s}
			}
		}
	}
}

// expandEnvFiles turns `- path` into `- {file: path}`. The scalar form is what
// every existing project writes and stays accepted permanently; the object form
// is what carries a provider.
func expandEnvFiles(m map[string]any) {
	fs, ok := m["env_files"].([]any)
	if !ok {
		return
	}
	for i, f := range fs {
		if s, ok := f.(string); ok {
			fs[i] = map[string]any{"file": s}
		}
	}
}

func expandTopLevelUnions(raw map[string]any) {
	// The project's own list. Every scope that accepts entries has to expand
	// them, or the scalar form works in one place and not another.
	if rt, ok := raw["runtime"].(map[string]any); ok {
		expandEnvFiles(rt)
	}
	if envs, ok := raw["environments"].(map[string]any); ok {
		for _, e := range envs {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			expandEnvFiles(em)
			// An override is authored in the same vocabulary as what it
			// overrides, so it expands by the same rule. Without this the
			// scalar form works everywhere except the one place an environment
			// varies a workload's list, which is the case the field exists for.
			if ov, ok := em["overrides"].(map[string]any); ok {
				if wls, ok := ov["workloads"].(map[string]any); ok {
					for _, w := range wls {
						if wm, ok := w.(map[string]any); ok {
							expandEnvFiles(wm)
						}
					}
				}
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

// decodeSpec turns the expanded document into the model. The shape walk has
// already refused anything the model does not define, so a failure here is a
// type mismatch — a string where a number belongs — and is reported as one.
func decodeSpec(raw map[string]any) (*Spec, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, errf("internal_decode_failed", "", "", "cannot encode project: %v", err)
	}
	var p Spec
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, errf("project_invalid", "", "", "%s", typeMismatch(err))
	}
	return &p, nil
}

// typeMismatch turns the decoder's own vocabulary into the field and the two
// kinds involved, which is all the author needs.
func typeMismatch(err error) string {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) && ute.Field != "" {
		return fmt.Sprintf("%s expects %s, got %s", ute.Field, friendlyKind(ute.Type.String()), ute.Value)
	}
	return firstLine(err.Error())
}

func friendlyKind(goType string) string {
	switch {
	case strings.HasPrefix(goType, "[]"):
		return "a list"
	case strings.HasPrefix(goType, "map["):
		return "a mapping"
	case goType == "string":
		return "a string"
	case strings.HasPrefix(goType, "int"), strings.HasPrefix(goType, "float"):
		return "a number"
	case goType == "bool":
		return "true or false"
	}
	return goType
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

		// A managed volume's name carries no replica index, so every replica
		// mounts the same directory. For durable state that is two database
		// processes on one data directory, which corrupts it — and nothing
		// about the runtime would say so until the damage was done.
		//
		// Replicating a stateful service needs a volume per instance and a
		// protocol between them. This contract derives neither, so it refuses
		// the declaration rather than generating something that starts.
		// Asked for explicitly, so say why it cannot be honoured rather than
		// quietly substituting recreate: rolling has nothing to gate on.
		for _, variable := range sortedKeys(w.Env) {
			if service, claimed := p.connectionVars(name, w)[variable]; claimed {
				return errf("connection_variable_claimed", path+".env."+variable, "",
					"%q is supplied to this workload by the managed service %q. The container runtime ranks "+
						"an inline environment above an environment file, so this value would replace the "+
						"generated credential, which exists nowhere else",
					variable, service)
			}
		}
		// An HTTP probe needs somewhere to probe. Defaulting supplies the
		// routed or published port; a workload with neither — a worker or a job
		// using the bare path shorthand — would otherwise generate a check
		// against port 0 that can never pass, and a rolling release would wait
		// out its whole budget before saying so without naming a port.
		if w.Health != nil && w.Health.HTTP != "" && w.Health.Port == 0 {
			return errf("health_port_unknown", path+".health", "",
				"an http health check needs a port: workload %q declares no port, no route and no published port, "+
					"so there is nothing to probe. Name one as health.port", name)
		}
		if w.Strategy == "rolling" && w.Health == nil {
			return errf("strategy_ungated", path+".strategy", "",
				"workload %q asks for a rolling release but declares no health check, so nothing says when the "+
					"newcomer is ready to take traffic. Declare health:, or use strategy: recreate", name)
		}
		// Keyed on the authored block, not on HoldsDurableData: inferring
		// durability must not tighten a refusal against a project that already
		// loads. `ob doctor` reports the hazard instead.
		if w.Replicas > 1 && w.Persistence != nil && w.Persistence.Mode == "durable" {
			return errf("stateful_replicas", path+".replicas", "",
				"workload %q keeps durable state and asks for %d replicas; they would all mount the same volume. "+
					"Run one instance, or declare the workload without durable persistence if its data is not state",
				name, w.Replicas)
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
			_, isExternal := p.ExternalServices[n.Name]
			if !isWorkload {
				if _, isService := p.Services[n.Name]; !isService && !isExternal {
					return errf("unknown_prerequisite", path+".needs", "",
						"workload %q needs %q, which is neither a workload, a run service, nor an external service", name, n.Name)
				}
			}
			if isExternal {
				external := p.ExternalServices[n.Name]
				for variable, part := range n.Env {
					if external.Connection.Entries[part] == "" {
						return errf("project_invalid", path+".needs", "ob validate",
							"workload %q maps %s from external service %q, but its trusted connection source declares no %q entry", name, variable, n.Name, part)
					}
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
			if isExternal {
				hasHealth = p.ExternalServices[n.Name].Probe != nil
			} else if !isWorkload {
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
		if external, clash := p.ExternalServices[name]; clash {
			return errf("identifier_collision", "external_services."+name, "",
				"%q is declared as both a Onebox-run service and an external service owned by %q", name, external.ProtectionOwner)
		}
		svc := p.Services[name]
		key, d, known := driverOf(name, svc)
		if !known {
			return errf("unknown_service_driver", "services."+name, "",
				"no managed driver named %q; Onebox runs these: %s.\n"+
					"To run something else, declare it as a daemon workload — you own the image and the settings then.",
				key, strings.Join(DriverNames(), ", "))
		}
		if err := validateProtectionSelection(p, name, key, svc); err != nil {
			return err
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
	for _, name := range sortedKeys(p.ExternalServices) {
		if _, clash := p.Workloads[name]; clash {
			return errf("identifier_collision", "external_services."+name, "",
				"%q names both a workload and an external service; dependency ownership would be ambiguous", name)
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// checkDeclaredFilesExist refuses an entry naming a file that is not there.
//
// The generated runtime references it, so the container runtime would refuse to
// start against it on the target — a failure that arrives after a deploy has
// begun, naming a path the operator has to trace back themselves.
func (p *Spec) checkDeclaredFilesExist() error {
	check := func(entries []EnvFile, at string) error {
		for i, entry := range entries {
			if _, err := os.Stat(filepath.Join(p.Dir, entry.File)); err != nil {
				return errf("env_file_missing", indexed(at, i), "",
					"the environment file %q does not exist", entry.File)
			}
		}
		return nil
	}
	if p.Runtime != nil {
		if err := check(p.Runtime.EnvFiles, "runtime.env_files"); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(p.Environments) {
		if err := check(p.Environments[name].EnvFiles, "environments."+name+".env_files"); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(p.Workloads) {
		if err := check(p.Workloads[name].EnvFiles, "workloads."+name+".env_files"); err != nil {
			return err
		}
	}
	return nil
}
