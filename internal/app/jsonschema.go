package app

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// The published schema describes the document an author writes, not the one the
// loader validates.
//
// Those differ: shorthand is expanded before validation, so a schema generated
// from the model alone would flag `image: nginx` and `services: {postgres: 17}`
// as errors. An editor that underlines correct projects is worse than no editor
// support, because the author learns to ignore it — so every form the loader
// accepts is a form this schema accepts.
//
// It is generated from the same declarations the loader enforces, and gated
// against the conformance corpus: it must accept and reject exactly what the
// loader does. A published schema that disagrees teaches something untrue.

// SchemaID is both the schema identity and its stable, publicly retrievable
// location. The main-branch path stays fixed across Onebox releases.
const SchemaID = "https://raw.githubusercontent.com/labstack/onebox/main/docs/onebox.run-v1.schema.json"

// JSONSchema is the published contract, ready to write.
func JSONSchema() ([]byte, error) {
	defs := map[string]any{}
	root := schemaFor(reflect.TypeOf(Spec{}), defs)

	doc := map[string]any{}
	for k, v := range root {
		doc[k] = v
	}
	doc["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	doc["$id"] = SchemaID
	doc["title"] = "Onebox project (onebox.run/v1)"
	doc["description"] = "One application, its workloads, the services it needs, and how a release rolls out."

	// The constraints the loader enforces, so the schema refuses what the
	// loader refuses. Without these it would describe only the shape, and an
	// editor would stay silent on a value that fails at deploy time.
	for _, c := range schemaConstraints {
		if at := indexPath(doc, c.path); at != nil {
			mergeSchema(at, c.apply)
		}
	}
	applyRoleRules(doc)

	// Every form the loader accepts, so an editor does not underline a correct
	// project. A schema that flags valid work teaches the author to ignore it.
	for _, form := range authoredForms {
		replaceAt(doc, form.path, form.alternative, form.note)
	}

	// A single-workload project may write the workload's own fields at the top
	// level. They are optional there and refused alongside a workloads block,
	// which the loader enforces and a schema cannot express.
	if props, ok := doc["properties"].(map[string]any); ok {
		if workloads, ok := indexPathOK(doc, []string{"workloads", "*"}); ok {
			if wp, ok := workloads["properties"].(map[string]any); ok {
				for k, v := range topLevelShorthand(wp) {
					props[k] = v
				}
			}
		}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// schemaFor renders one type, registering named object types in $defs so the
// document stays readable and recursive shapes terminate.
func schemaFor(t reflect.Type, defs map[string]any) map[string]any {
	t = deref(t)

	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Interface:
		// A field the contract accepts in several shapes — a command, an
		// environment value. Constrained by the loader, not here.
		return map[string]any{}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": schemaFor(t.Elem(), defs),
		}
	case reflect.Struct:
		props := map[string]any{}
		for name, field := range fieldsOf(t) {
			property := schemaFor(field.Type, defs)
			annotateSchemaField(property, field)
			props[name] = property
		}
		out := map[string]any{
			"type":       "object",
			"properties": props,
			// Extension keys are accepted wherever a mapping is; everything
			// else is refused, which is the whole point of a closed contract.
			"patternProperties":    map[string]any{"^x-": map[string]any{}},
			"additionalProperties": false,
		}
		return out
	}
	return map[string]any{}
}

// annotateSchemaField carries the public field contract beside the Go model.
// Adding a field without adding its description is caught by the schema tests.
func annotateSchemaField(schema map[string]any, field reflect.StructField) {
	if description := field.Tag.Get("description"); description != "" {
		schema["description"] = description
	}
	if value := field.Tag.Get("default"); value != "" {
		schema["default"] = schemaTagValue(value, field.Type)
	}
	if value := field.Tag.Get("example"); value != "" {
		schema["examples"] = []any{schemaTagValue(value, field.Type)}
	}
}

func schemaTagValue(value string, t reflect.Type) any {
	switch deref(t).Kind() {
	case reflect.Bool:
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	case reflect.Float32, reflect.Float64:
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return value
}

// mergeSchema preserves semantic hover text when a grammar adds its more
// mechanical constraint. Both are useful: what a field does, then what it accepts.
func mergeSchema(dst, src map[string]any) {
	for key, value := range src {
		if key == "description" {
			incoming, _ := value.(string)
			existing, _ := dst[key].(string)
			switch {
			case existing == "":
				dst[key] = incoming
			case incoming != "" && !strings.Contains(existing, incoming):
				dst[key] = existing + " " + incoming
			}
			continue
		}
		dst[key] = value
	}
}

// authoredForms are the shapes the author may write that the loader expands
// before it validates. Each names a path in the generated schema and the
// alternative form accepted there.
//
// They are listed rather than derived because expansion is deliberate
// behaviour, not a property of any type: `image: nginx` is a courtesy the
// contract extends, and the list of courtesies is exactly as long as the
// loader's.
var authoredForms = []struct {
	path        []string
	alternative map[string]any
	note        string
}{
	{[]string{"workloads", "*", "image"}, imageStringForm(), "an image reference"},
	{[]string{"workloads", "*", "build"}, stringForm(), "a build context path"},
	{[]string{"workloads", "*", "health"}, stringForm(), "an HTTP health path"},
	{[]string{"workloads", "*", "command"}, commandForms(), "a command line or argument list"},
	{[]string{"workloads", "*", "entrypoint"}, commandForms(), "an entrypoint or argument list"},
	{[]string{"workloads", "*", "needs", "items"}, stringForm(), "the name of a prerequisite"},
	{[]string{"environments", "*", "server"}, stringForm(), "user@host"},
	{[]string{"runtime", "env_files", "items"}, stringForm(), "a path to an environment file"},
	{[]string{"environments", "*", "env_files", "items"}, stringForm(), "a path to an environment file"},
	{[]string{"workloads", "*", "env_files", "items"}, stringForm(), "a path to an environment file"},
	{[]string{"hooks", "*"}, stringForm(), "the command to run"},
	{[]string{"services", "*"}, scalarForm(), "the version to run"},
}

func stringForm() map[string]any { return map[string]any{"type": "string"} }

func imageStringForm() map[string]any {
	out := pattern(gImageRef)
	out["type"] = "string"
	return out
}

func scalarForm() map[string]any {
	return map[string]any{"type": []any{"string", "number", "integer"}}
}

func commandForms() map[string]any {
	return map[string]any{"anyOf": []any{
		map[string]any{"type": "string"},
		map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}}
}

// shorthandKeysAtTopLevel are the workload fields a single-workload project may
// write at the top level instead of a workloads block.
func topLevelShorthand(workloadProps map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range shorthandKeys {
		if v, ok := workloadProps[k]; ok {
			out[k] = v
		}
	}
	return out
}

func indexPathOK(doc map[string]any, path []string) (map[string]any, bool) {
	v := indexPath(doc, path)
	return v, v != nil
}

func indexPath(doc map[string]any, path []string) map[string]any {
	cur := doc
	for _, step := range path {
		switch step {
		case "*":
			next, ok := cur["additionalProperties"].(map[string]any)
			if !ok {
				return nil
			}
			cur = next
		case "items":
			next, ok := cur["items"].(map[string]any)
			if !ok {
				return nil
			}
			cur = next
		default:
			props, ok := cur["properties"].(map[string]any)
			if !ok {
				return nil
			}
			next, ok := props[step].(map[string]any)
			if !ok {
				return nil
			}
			cur = next
		}
	}
	return cur
}

// replaceAt swaps the schema at a path for one that accepts both forms.
func replaceAt(doc map[string]any, path []string, alternative map[string]any, note string) {
	parent := indexPath(doc, path[:len(path)-1])
	if parent == nil {
		return
	}
	last := path[len(path)-1]
	var container map[string]any
	var key string
	switch last {
	case "*":
		container, key = parent, "additionalProperties"
	case "items":
		container, key = parent, "items"
	default:
		props, ok := parent["properties"].(map[string]any)
		if !ok {
			return
		}
		container, key = props, last
	}
	full, ok := container[key].(map[string]any)
	if !ok {
		return
	}
	description, _ := full["description"].(string)
	if description != "" {
		description += " "
	}
	description += "Also accepts " + note + "."
	container[key] = map[string]any{
		"description": description,
		"anyOf":       []any{alternative, full},
	}
}

func sortedStringKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasPrefixAny(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// schemaConstraints carries each grammar, enum and bound to the place in the
// published schema that field occupies.
//
// It is a second statement of what validate.go applies, and that is a real
// cost. The gate is what makes it safe: a constraint added to one and not the
// other shows up as the schema accepting a project the loader rejects, and
// fails the build. Duplication that cannot drift silently is a different thing
// from duplication.
var schemaConstraints = []struct {
	path  []string
	apply map[string]any
}{
	{[]string{"api_version"}, map[string]any{"const": APIVersion}},
	{[]string{"app"}, appNameConstraint()},
	{[]string{"base_path"}, pattern(gAbsPath)},

	{[]string{"environments", "*", "base_path"}, pattern(gAbsPath)},
	{[]string{"environments", "*", "policy", "minimum_onebox_version"}, pattern(gCalVer)},
	{[]string{"environments", "*", "policy", "minimum_plan_schema"}, pattern(gPlanSchema)},
	{[]string{"environments", "*", "policy", "migration_backup_maximum_age"}, pattern(gDur)},

	{[]string{"workloads", "*", "role"}, enum(eRole)},
	{[]string{"workloads", "*", "replicas"}, map[string]any{"minimum": 1}},
	{[]string{"workloads", "*", "strategy"}, enum(eStrategy)},
	{[]string{"workloads", "*", "when"}, enum(eJobWhen)},
	{[]string{"workloads", "*", "data_effect"}, enum(eDataEffect)},
	{[]string{"workloads", "*", "compose"}, pattern(gComposeRef)},
	{[]string{"workloads", "*", "port"}, portBounds()},
	{[]string{"workloads", "*", "working_dir"}, pattern(gAbsPath)},
	{[]string{"workloads", "*", "env_files", "items", "file"}, pattern(gRepoPath)},
	{[]string{"workloads", "*", "env_files", "items", "provider"}, enum(eSecretProvider)},
	{[]string{"workloads", "*", "env_files", "items"}, map[string]any{"required": []any{"file"}}},
	{[]string{"workloads", "*", "image", "reference"}, pattern(gImageRef)},
	{[]string{"workloads", "*", "image", "pull"}, enum(eImagePull)},
	{[]string{"workloads", "*", "build", "context"}, pattern(gRepoPath)},
	{[]string{"workloads", "*", "build", "dockerfile"}, pattern(gRepoPath)},
	{[]string{"workloads", "*", "health", "http"}, pattern(gURLPath)},
	{[]string{"workloads", "*", "health", "port"}, portBounds()},
	{[]string{"workloads", "*", "health", "interval"}, pattern(gDur)},
	{[]string{"workloads", "*", "health", "start_period"}, pattern(gDur)},
	{[]string{"workloads", "*", "health", "within"}, pattern(gDur)},
	{[]string{"workloads", "*", "drain", "signal"}, pattern(gSignal)},
	{[]string{"workloads", "*", "drain", "wait"}, pattern(gDur)},
	{[]string{"workloads", "*", "drain", "grace"}, pattern(gDur)},
	{[]string{"workloads", "*", "resources", "memory"}, pattern(gSize)},
	{[]string{"workloads", "*", "resources", "cpus"}, pattern(gCpus)},
	{[]string{"workloads", "*", "persistence", "mode"}, enum(ePersistence)},
	{[]string{"workloads", "*", "routes", "items", "path"}, pattern(gURLPath)},
	{[]string{"workloads", "*", "routes", "items", "port"}, portBounds()},
	{[]string{"workloads", "*", "routes", "items", "protocol"}, enum(eRouteProtocol)},
	{[]string{"workloads", "*", "routes", "items", "scheme"}, enum(eRouteScheme)},
	{[]string{"workloads", "*", "routes", "items", "tls"}, enum(eRouteTLS)},
	{[]string{"workloads", "*", "volumes", "items", "name"}, pattern(gIdent)},
	{[]string{"workloads", "*", "volumes", "items", "path"}, pattern(gAbsPath)},

	{[]string{"workloads", "*", "volumes", "items", "mode"}, enum(eMountMode)},
	// A named volume says where it mounts, or it is a bind pair. Either way
	// something has to say where it lands in the container.
	{[]string{"workloads", "*", "volumes", "items"}, map[string]any{
		"anyOf": []any{
			map[string]any{"required": []any{"name", "path"}},
			map[string]any{"required": []any{"source", "path"}},
		},
	}},
	{[]string{"workloads", "*", "published_ports", "items", "host"}, portBounds()},
	{[]string{"workloads", "*", "published_ports", "items", "container"}, portBounds()},
	{[]string{"workloads", "*", "published_ports", "items", "protocol"}, enum(ePortProtocol)},
	{[]string{"workloads", "*", "needs", "items", "name"}, pattern(gIdent)},
	{[]string{"workloads", "*", "needs", "items", "condition"}, enum(eNeedCondition)},
	{[]string{"workloads", "*", "schedule", "cron"}, pattern(gCron)},
	{[]string{"workloads", "*", "schedule", "timezone"}, pattern(gTZ)},

	{[]string{"services", "*", "driver"}, pattern(gIdent)},
	{[]string{"services", "*", "persistence", "mode"}, enum(ePersistence)},
	{[]string{"services", "*", "resources", "memory"}, pattern(gSize)},
	{[]string{"services", "*", "resources", "cpus"}, pattern(gCpus)},
	{[]string{"services", "*", "volumes", "items"}, pattern(gIdent)},
	{[]string{"services", "*", "protection", "target"}, pattern(gIdent)},
	{[]string{"services", "*", "protection", "recovery_kind"}, enum(eRecoveryKind)},
	{[]string{"services", "*", "protection", "maximum_data_loss"}, pattern(gDur)},
	{[]string{"services", "*", "protection", "schedule", "cron"}, pattern(gCron)},
	{[]string{"services", "*", "protection", "schedule", "timezone"}, pattern(gTZ)},
	{[]string{"services", "*", "protection", "retention", "minimum_generations"}, map[string]any{"minimum": 1}},
	{[]string{"services", "*", "protection", "retention", "recovery_window"}, pattern(gDur)},
	{[]string{"services", "*", "protection", "restore_drill", "schedule", "cron"}, pattern(gCron)},
	{[]string{"services", "*", "protection", "restore_drill", "schedule", "timezone"}, pattern(gTZ)},
	{[]string{"services", "*", "protection", "restore_drill", "proof_maximum_age"}, pattern(gDur)},
	{[]string{"services", "*", "protection", "restore_drill", "staging_filesystem"}, pattern(gAbsPath)},

	{[]string{"backup_targets", "*", "kind"}, enum(eBackupTargetKind)},
	{[]string{"backup_targets", "*", "endpoint"}, pattern(gHTTPURL)},
	{[]string{"backup_targets", "*", "bucket"}, pattern(gBucket)},
	{[]string{"backup_targets", "*", "prefix"}, pattern(gObjectPrefix)},
	{[]string{"backup_targets", "*", "region"}, pattern(gS3Region)},
	{[]string{"backup_targets", "*", "tls"}, enum(eBackupTLS)},
	{[]string{"backup_targets", "*", "failure_domain", "identity"}, pattern(gFailureDomain)},
	{[]string{"backup_targets", "*", "failure_domain", "host"}, pattern(gFailureDomain)},
	{[]string{"backup_targets", "*", "credentials", "file"}, pattern(gRepoPath)},
	{[]string{"backup_targets", "*", "credentials", "provider"}, enum(eSecretProvider)},
	{[]string{"backup_targets", "*", "credentials", "access_key_entry"}, pattern(gEnvName)},
	{[]string{"backup_targets", "*", "credentials", "secret_key_entry"}, pattern(gEnvName)},
	{[]string{"backup_targets", "*", "credentials", "session_token_entry"}, pattern(gEnvName)},
	{[]string{"backup_targets", "*", "encryption", "snapshot"}, enum(eEncryptionMode)},
	{[]string{"backup_targets", "*", "encryption", "pitr"}, enum(eEncryptionMode)},
	{[]string{"backup_targets", "*", "encryption", "cold"}, enum(eEncryptionMode)},

	{[]string{"external_services", "*", "driver"}, enum(DriverNames())},
	{[]string{"external_services", "*", "connection", "source", "file"}, pattern(gRepoPath)},
	{[]string{"external_services", "*", "connection", "source", "provider"}, enum(eSecretProvider)},
	{[]string{"external_services", "*", "connection", "entries", "*"}, pattern(gEnvName)},
	{[]string{"external_services", "*", "protection_owner"}, pattern(gProtectionOwner)},
	{[]string{"external_services", "*", "probe", "kind"}, enum(eExternalProbeKind)},
	{[]string{"external_services", "*", "probe", "timeout"}, pattern(gDur)},
	{[]string{"external_services", "*", "probe", "maximum_age"}, pattern(gDur)},

	{[]string{"proxy", "kind"}, enum(eProxyKind)},
	{[]string{"proxy", "image"}, pattern(gImageRef)},
	{[]string{"proxy", "config"}, pattern(gRepoPath)},
	{[]string{"deployment", "migration_policy"}, enum(eMigrationPolicy)},
	{[]string{"deployment", "retain_releases"}, map[string]any{"minimum": 1}},
	{[]string{"registries", "*", "server"}, pattern(gRegistryHost)},
	{[]string{"registries", "*", "username"}, pattern(gRegistryUser)},
	{[]string{"registries", "*", "password_env"}, pattern(gEnvName)},
	{[]string{"notifications", "*", "format"}, enum(eNotifyFormat)},
	{[]string{"runtime", "env_files", "items", "file"}, pattern(gRepoPath)},
	{[]string{"runtime", "env_files", "items", "provider"}, enum(eSecretProvider)},
	{[]string{"runtime", "env_files", "items"}, map[string]any{"required": []any{"file"}}},
	{[]string{"environments", "*", "env_files", "items", "file"}, pattern(gRepoPath)},
	{[]string{"environments", "*", "env_files", "items", "provider"}, enum(eSecretProvider)},
	{[]string{"environments", "*", "env_files", "items"}, map[string]any{"required": []any{"file"}}},
	{[]string{"runtime", "env_checks", "items", "file"}, pattern(gRepoPath)},
	{[]string{"verifications", "items", "http"}, pattern(gURLPath)},
	{[]string{"verifications", "items", "url"}, pattern(gHTTPURL)},
	{[]string{"verifications", "items", "port"}, portBounds()},
	{[]string{"verifications", "items", "status_codes", "items"}, map[string]any{"minimum": 100, "maximum": 599}},
	{[]string{"observability", "alerts", "unhealthy_after"}, pattern(gDur)},
	{[]string{"observability", "logs", "retention"}, pattern(gDur)},
	{[]string{"notifications", "*", "on", "items"}, enum(eNotifyEvent)},
	{[]string{"services", "*", "settings"}, propertyNames(gSettingKey)},
	{[]string{"workloads", "*", "logging", "driver"}, pattern(gLogDriver)},
	{[]string{"workloads", "*", "logging", "options"}, propertyNames(gLogOption)},
}

// applyRoleRules expresses what belongs to which role, and what a project must
// declare at all. Both are within a JSON Schema's reach and are exactly the
// mistakes an editor should catch while the file is still open.
func applyRoleRules(doc map[string]any) {
	doc["required"] = []any{"api_version", "environments"}

	workload := indexPath(doc, []string{"workloads", "*"})
	if workload == nil {
		return
	}
	// An environments block that exists but is empty declares nothing.
	if envs := indexPath(doc, []string{"environments"}); envs != nil {
		envs["minProperties"] = 1
	}

	// Shorthand replaces the workloads block; it does not extend it. Writing
	// both leaves it ambiguous which workload the top-level fields describe.
	sources := []any{"build", "image", "compose"}
	doc["not"] = map[string]any{"allOf": []any{
		map[string]any{"required": []any{"workloads"}},
		map[string]any{"anyOf": anyRequired(append(append([]any{}, sources...), "domain", "port", "health", "routes"))},
	}}

	// A project must describe something to run: a non-empty workloads block,
	// or the shorthand that becomes one.
	doc["anyOf"] = []any{
		map[string]any{
			"required":   []any{"workloads"},
			"properties": map[string]any{"workloads": map[string]any{"minProperties": 1}},
		},
		map[string]any{"anyOf": anyRequired(sources)},
	}

	jobOnly := []any{"when", "data_effect", "schedule"}
	workload["allOf"] = []any{
		// Exactly one source. A workload with none cannot run and a workload
		// with two does not say which image it is.
		map[string]any{"oneOf": anyRequired(sources)},
		// The domain shorthand and the routes list say the same thing twice,
		// and domain without a port does not say where to send the traffic.
		map[string]any{"not": map[string]any{"allOf": []any{
			map[string]any{"anyOf": anyRequired([]any{"domain", "port"})},
			map[string]any{"required": []any{"routes"}},
		}}},
		map[string]any{
			"if":   map[string]any{"required": []any{"domain"}},
			"then": map[string]any{"required": []any{"port"}},
		},
		map[string]any{
			"if":   map[string]any{"required": []any{"port"}},
			"then": map[string]any{"required": []any{"domain"}},
		},
		// A workload that declares durable persistence cannot be replicated:
		// every replica would mount the same volume. The loader refuses this,
		// and an editor should underline it too rather than leaving the author
		// to discover it at plan time.
		map[string]any{
			"if": map[string]any{
				// `persistence: {}` is durable too — mode defaults to it — so the
				// rule must fire on an absent mode as well as an explicit one.
				"properties": map[string]any{"persistence": map[string]any{
					"anyOf": []any{
						map[string]any{"properties": map[string]any{"mode": map[string]any{"const": "durable"}}, "required": []any{"mode"}},
						map[string]any{"not": map[string]any{"required": []any{"mode"}}},
					},
				}},
				"required": []any{"persistence"},
			},
			"then": map[string]any{"properties": map[string]any{"replicas": map[string]any{"maximum": 1}}},
		},
		// A job declares its data effect: the one field whose absence would
		// let an unknown migration through the rollback gate.
		map[string]any{
			"if":   map[string]any{"properties": map[string]any{"role": map[string]any{"const": RoleJob}}, "required": []any{"role"}},
			"then": map[string]any{"required": []any{"data_effect"}},
			"else": map[string]any{"not": map[string]any{"anyOf": anyRequired(jobOnly)}},
		},
	}
}

// anyRequired matches a document declaring any one of these fields.
func anyRequired(fields []any) []any {
	out := make([]any, 0, len(fields))
	for _, f := range fields {
		out = append(out, map[string]any{"required": []any{f}})
	}
	return out
}

// appNameConstraint is the identifier grammar plus the host layout's
// reservations, which a schema can hold as well as the loader can.
func appNameConstraint() map[string]any {
	forbidden := make([]any, 0, len(reservedAppNames)+1)
	forbidden = append(forbidden, map[string]any{"pattern": "^ob-"})
	for _, name := range reservedAppNames {
		forbidden = append(forbidden, map[string]any{"const": name})
	}
	out := pattern(gIdent)
	out["not"] = map[string]any{"anyOf": forbidden}
	out["description"] = "The application's name. Expects " + gIdent.means +
		", and may not begin \"ob-\" or be a name the host layout reserves."
	return out
}

func pattern(g grammar) map[string]any {
	return map[string]any{"pattern": g.pattern.String(), "description": "Expects " + g.means + "."}
}

// propertyNames constrains a free-form map's KEYS, which is where a shell
// metacharacter would otherwise reach a generated command.
func propertyNames(g grammar) map[string]any {
	return map[string]any{"propertyNames": map[string]any{"pattern": g.pattern.String()}}
}

func enum(values []string) map[string]any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return map[string]any{"enum": out}
}

func portBounds() map[string]any {
	return map[string]any{"minimum": 1, "maximum": 65535}
}
