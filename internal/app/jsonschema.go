package app

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"

	applicationv1alpha1 "github.com/labstack/onebox/api/application/v1alpha1"
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
const SchemaID = "https://onebox.run/schemas/application/v1alpha1/application.schema.json"

// JSONSchema is the published contract, ready to write.
func JSONSchema() ([]byte, error) {
	embedded := bytes.TrimSpace(applicationv1alpha1.Schema)
	return append([]byte(nil), embedded...), nil
}

// GenerateJSONSchema derives the public schema from the loader's declarations.
// It is used only to regenerate and verify the embedded published artifact.
func GenerateJSONSchema() ([]byte, error) {
	defs := map[string]any{}
	spec := schemaFor(reflect.TypeOf(Spec{}), defs)

	// The constraints the loader enforces, so the schema refuses what the
	// loader refuses. Without these it would describe only the shape, and an
	// editor would stay silent on a value that fails at deploy time.
	for _, c := range schemaConstraints {
		if at := indexPath(spec, c.path); at != nil {
			mergeSchema(at, c.apply)
		}
	}
	applyRoleRules(spec)

	// Every form the loader accepts, so an editor does not underline a correct
	// project. A schema that flags valid work teaches the author to ignore it.
	for _, form := range authoredForms {
		replaceAt(spec, form.path, form.alternative, form.note)
	}
	if props, ok := spec["properties"].(map[string]any); ok {
		delete(props, "api_version")
		delete(props, "app")
	}
	spec["required"] = []any{"environments", "workloads"}
	spec = authoredSchema(spec, "")
	nameSchema := appNameConstraint()
	mergeSchema(nameSchema, map[string]any{
		"description": "Stable application name used in generated runtime identities.",
		"examples":    []any{"shop"},
	})

	doc := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         SchemaID,
		"title":       "Onebox Application (onebox.run/v1alpha1)",
		"description": "One application, its workloads, the services it needs, and how a release rolls out.",
		"type":        "object",
		"properties": map[string]any{
			"apiVersion": map[string]any{"type": "string", "const": APIVersion, "description": "Authored Application API identity."},
			"kind":       map[string]any{"type": "string", "const": ApplicationKind, "description": "Authored resource kind."},
			"metadata": map[string]any{
				"type":        "object",
				"description": "Application identity and opaque user metadata.",
				"properties": map[string]any{
					"name":        nameSchema,
					"annotations": map[string]any{"type": "object", "description": "Opaque user metadata that never affects plans or runtime behavior.", "additionalProperties": map[string]any{"type": "string"}},
				},
				"required":             []any{"name"},
				"additionalProperties": false,
			},
			"spec": mergeDescription(spec, "Desired Onebox application configuration."),
		},
		"required":             []any{"apiVersion", "kind", "metadata", "spec"},
		"additionalProperties": false,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func mergeDescription(schema map[string]any, description string) map[string]any {
	schema["description"] = description
	return schema
}

func authoredSchema(node map[string]any, field string) map[string]any {
	out := map[string]any{}
	for key, value := range node {
		switch key {
		case "patternProperties":
			continue
		case "properties":
			props, _ := value.(map[string]any)
			converted := map[string]any{}
			for name, child := range props {
				if schema, ok := child.(map[string]any); ok {
					converted[lowerCamel(name)] = authoredSchema(schema, name)
				} else {
					converted[lowerCamel(name)] = child
				}
			}
			out[key] = converted
		case "required":
			items, _ := value.([]any)
			converted := make([]any, len(items))
			for i, item := range items {
				if name, ok := item.(string); ok {
					converted[i] = lowerCamel(name)
				} else {
					converted[i] = item
				}
			}
			out[key] = converted
		case "enum":
			items, _ := value.([]any)
			converted := make([]any, len(items))
			for i, item := range items {
				if text, ok := item.(string); ok {
					converted[i] = publicEnum(field, text)
				} else {
					converted[i] = item
				}
			}
			out[key] = converted
		case "const", "default":
			if text, ok := value.(string); ok {
				out[key] = publicEnum(field, text)
			} else {
				out[key] = value
			}
		default:
			switch child := value.(type) {
			case map[string]any:
				out[key] = authoredSchema(child, field)
			case []any:
				items := make([]any, len(child))
				for i, item := range child {
					if schema, ok := item.(map[string]any); ok {
						items[i] = authoredSchema(schema, field)
					} else {
						items[i] = item
					}
				}
				out[key] = items
			default:
				out[key] = value
			}
		}
	}
	return out
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
			"type":                 "object",
			"properties":           props,
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
	target := deref(t)
	switch target.Kind() {
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
	case reflect.Slice:
		// A list's default is written in the tag the way it reads in prose,
		// `success, failure`, because that is what the field reference prints.
		// The schema needs the value itself: a string default on an array
		// property is a contradiction, and an editor that applies defaults
		// would fill the list with one sentence.
		parts := strings.Split(value, ",")
		out := make([]any, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			out = append(out, schemaTagValue(part, target.Elem()))
		}
		return out
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
	{[]string{"environments", "*", "jump"}, stringForm(), "user@host or user@host:port"},
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
	{[]string{"services"}, serviceNamesConstraint()},
	{[]string{"services", "*", "features", "extensions"}, map[string]any{
		"propertyNames": map[string]any{"pattern": gExtension.pattern.String()},
	}},

	{[]string{"environments", "*", "base_path"}, pattern(gAbsPath)},
	{[]string{"environments", "*", "policy", "min_onebox_version"}, pattern(gCalVer)},
	{[]string{"environments", "*", "policy", "min_plan_schema"}, pattern(gPlanSchema)},
	{[]string{"environments", "*", "policy", "migrations", "backup_max_age"}, pattern(gDur)},

	{[]string{"workloads", "*", "role"}, enum(eRole)},
	{[]string{"workloads", "*", "replicas"}, map[string]any{"minimum": 1}},
	{[]string{"workloads", "*", "strategy"}, enum(eStrategy)},
	{[]string{"workloads", "*", "deployment_phase"}, enum(eJobDeploymentPhase)},
	{[]string{"workloads", "*", "operator_run"}, enum(eJobOperatorRun)},
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
	{[]string{"workloads", "*", "schedule", "notify", "items"}, enum(eScheduleNotify)},
	{[]string{"workloads", "*", "execution", "retention"}, pattern(gDur)},
	{[]string{"workloads", "*", "execution", "steps"}, map[string]any{"maxItems": 32}},
	{[]string{"workloads", "*", "execution", "steps", "items"}, map[string]any{"required": []any{"id", "command"}}},
	{[]string{"workloads", "*", "execution", "steps", "items", "id"}, pattern(gIdent)},
	{[]string{"workloads", "*", "execution", "steps", "items", "command"}, map[string]any{"minItems": 1, "maxItems": 128}},
	{[]string{"workloads", "*", "execution", "steps", "items", "inputs"}, propertyNames(gInputName)},
	{[]string{"workloads", "*", "execution", "steps", "items", "outputs"}, map[string]any{"maxItems": 32, "uniqueItems": true}},
	{[]string{"workloads", "*", "execution", "steps", "items", "outputs", "items"}, pattern(gInputName)},
	{[]string{"workloads", "*", "execution", "steps", "items", "retry", "attempts"}, map[string]any{"minimum": 1, "maximum": maxRetryAttempts}},
	{[]string{"workloads", "*", "execution", "steps", "items", "retry", "backoff"}, pattern(gDur)},
	{[]string{"workloads", "*", "execution", "steps", "items", "retry", "max_backoff"}, pattern(gDur)},
	{[]string{"workloads", "*", "schedule", "retry", "attempts"}, map[string]any{"minimum": 1, "maximum": maxRetryAttempts}},
	{[]string{"workloads", "*", "schedule", "retry", "backoff"}, pattern(gDur)},
	{[]string{"workloads", "*", "schedule", "retry", "max_backoff"}, pattern(gDur)},
	{[]string{"workloads", "*", "inputs"}, propertyNames(gInputName)},
	// An input says what it accepts, one way, and always has a default: the
	// timer fires without anyone to ask.
	{[]string{"workloads", "*", "inputs", "*"}, map[string]any{
		"required": []any{"default"},
		"oneOf":    anyRequired([]any{"enum", "pattern"}),
	}},
	{[]string{"workloads", "*", "resources", "memory"}, pattern(gSize)},
	{[]string{"workloads", "*", "resources", "cpus"}, pattern(gCpus)},
	{[]string{"workloads", "*", "persistence", "mode"}, enum(ePersistence)},
	{[]string{"workloads", "*", "routes", "items"}, map[string]any{
		"required": []any{"hostname"},
		"allOf": []any{
			map[string]any{
				"if": map[string]any{
					"required":   []any{"hostname"},
					"properties": map[string]any{"hostname": map[string]any{"const": "*"}},
				},
				"then": map[string]any{
					"required": []any{"protocol", "tls"},
					"properties": map[string]any{
						"protocol": map[string]any{"const": "tcp"},
						"tls":      map[string]any{"enum": []any{"none", "passthrough"}},
					},
				},
			},
			map[string]any{
				"if": map[string]any{
					"required":   []any{"hostname"},
					"properties": map[string]any{"hostname": map[string]any{"pattern": `^\*\.`}},
				},
				"then": map[string]any{
					"properties": map[string]any{"protocol": map[string]any{"const": "http"}},
				},
			},
			map[string]any{
				"if": map[string]any{
					"required":   []any{"tls"},
					"properties": map[string]any{"tls": map[string]any{"const": "passthrough"}},
				},
				"then": map[string]any{
					"required":   []any{"protocol"},
					"properties": map[string]any{"protocol": map[string]any{"const": "tcp"}},
				},
			},
		},
	}},
	{[]string{"workloads", "*", "routes", "items", "hostname"}, map[string]any{"anyOf": []any{
		map[string]any{
			"pattern":   gRouteHostname.pattern.String(),
			"maxLength": 253,
		},
		map[string]any{"const": "*"},
	}}},
	{[]string{"workloads", "*", "routes", "items", "path"}, pattern(gURLPath)},
	{[]string{"workloads", "*", "routes", "items", "port"}, portBounds()},
	{[]string{"workloads", "*", "routes", "items", "protocol"}, enum(eRouteProtocol)},
	{[]string{"workloads", "*", "routes", "items", "scheme"}, enum(eRouteScheme)},
	{[]string{"workloads", "*", "routes", "items", "tls"}, enum(eRouteTLS)},
	{[]string{"workloads", "*", "routes", "items", "middlewares", "items"}, pattern(gMiddlewareRef)},
	{[]string{"workloads", "*", "volumes", "items", "name"}, pattern(gIdent)},
	{[]string{"workloads", "*", "volumes", "items", "path"}, pattern(gAbsPath)},
	{[]string{"workloads", "*", "volumes", "items", "source"}, bindSourceConstraint()},

	{[]string{"workloads", "*", "volumes", "items", "mode"}, enum(eMountMode)},
	// A named volume says where it mounts, or it is a bind pair. Either way
	// something has to say where it lands in the container.
	{[]string{"workloads", "*", "volumes", "items"}, map[string]any{
		"anyOf": []any{
			map[string]any{"required": []any{"name", "path"}},
			map[string]any{"required": []any{"source", "path"}},
		},
		"allOf": []any{
			map[string]any{
				"if": map[string]any{
					"required": []any{"source"},
					"properties": map[string]any{
						"source": map[string]any{"pattern": "^[^/]"},
					},
				},
				"then": map[string]any{
					"required": []any{"mode"},
					"properties": map[string]any{
						"mode": map[string]any{"const": "ro"},
					},
				},
			},
		},
	}},
	{[]string{"workloads", "*", "published_ports", "items", "host"}, portBounds()},
	{[]string{"workloads", "*", "published_ports", "items", "container"}, portBounds()},
	{[]string{"workloads", "*", "published_ports", "items", "protocol"}, enum(ePortProtocol)},
	{[]string{"workloads", "*", "needs", "items", "name"}, pattern(gIdent)},
	{[]string{"workloads", "*", "needs", "items", "condition"}, enum(eNeedCondition)},
	{[]string{"workloads", "*", "schedule", "cron"}, pattern(gCron)},
	{[]string{"workloads", "*", "schedule", "timezone"}, pattern(gTZ)},
	{[]string{"workloads", "*", "schedule", "timeout"}, pattern(gDur)},
	{[]string{"workloads", "*", "schedule", "shutdown_grace"}, pattern(gDur)},
	{[]string{"workloads", "*", "schedule", "deploy_lock"}, enum(eScheduleDeployLock)},

	{[]string{"services", "*", "driver"}, pattern(gIdent)},
	{[]string{"services", "*", "persistence", "mode"}, enum(ePersistence)},
	{[]string{"services", "*", "resources", "memory"}, pattern(gSize)},
	{[]string{"services", "*", "resources", "cpus"}, pattern(gCpus)},
	{[]string{"services", "*", "volumes", "items"}, pattern(gIdent)},
	{[]string{"services", "*", "backup", "target"}, pattern(gIdent)},
	{[]string{"services", "*", "backup", "recovery_kind"}, enum(eRecoveryKind)},
	{[]string{"services", "*", "backup", "max_data_loss"}, pattern(gDur)},
	{[]string{"services", "*", "backup", "schedule", "cron"}, pattern(gCron)},
	{[]string{"services", "*", "backup", "schedule", "timezone"}, pattern(gTZ)},
	{[]string{"services", "*", "backup", "retention", "keep"}, map[string]any{"minimum": 1}},
	{[]string{"services", "*", "backup", "retention", "window"}, pattern(gDur)},
	{[]string{"services", "*", "backup", "drill", "schedule", "cron"}, pattern(gCron)},
	{[]string{"services", "*", "backup", "drill", "schedule", "timezone"}, pattern(gTZ)},
	{[]string{"services", "*", "backup", "drill", "max_age"}, pattern(gDur)},

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
	{[]string{"external_services", "*", "backup_owner"}, pattern(gBackupOwner)},
	{[]string{"external_services", "*", "probe", "kind"}, enum(eExternalProbeKind)},
	{[]string{"external_services", "*", "probe", "timeout"}, pattern(gDur)},
	{[]string{"external_services", "*", "probe", "max_age"}, pattern(gDur)},

	{[]string{"proxy", "kind"}, enum(eProxyKind)},
	{[]string{"proxy", "image"}, pattern(gImageRef)},
	{[]string{"proxy", "config"}, pattern(gRepoPath)},
	{[]string{"proxy", "dns_challenge", "provider"}, pattern(gDNSProvider)},
	{[]string{"proxy", "dns_challenge", "resolvers", "items"}, pattern(gDNSResolver)},
	{[]string{"proxy", "dns_challenge"}, map[string]any{"required": []any{"provider"}}},
	{[]string{"proxy", "entrypoints"}, propertyNames(gIdent)},
	{[]string{"proxy", "entrypoints", "*", "port"}, portBounds()},
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
	{[]string{"checks", "http", "items", "path"}, pattern(gURLPath)},
	{[]string{"checks", "url", "items", "url"}, pattern(gHTTPURL)},
	{[]string{"checks", "http", "items", "port"}, portBounds()},
	{[]string{"checks", "url", "items", "status_codes", "items"}, map[string]any{"minimum": 100, "maximum": 599}},
	{[]string{"notifications", "*", "on", "items"}, enum(eNotifyEvent)},
	{[]string{"services", "*", "settings"}, propertyNames(gSettingKey)},
	{[]string{"workloads", "*", "logging", "driver"}, pattern(gLogDriver)},
	{[]string{"workloads", "*", "logging", "options"}, propertyNames(gLogOption)},
}

// applyRoleRules expresses what belongs to which role, and what a project must
// declare at all. Both are within a JSON Schema's reach and are exactly the
// mistakes an editor should catch while the file is still open.
func applyRoleRules(doc map[string]any) {
	doc["required"] = []any{"environments", "workloads"}

	workload := indexPath(doc, []string{"workloads", "*"})
	if workload == nil {
		return
	}
	// An environments block that exists but is empty declares nothing.
	if envs := indexPath(doc, []string{"environments"}); envs != nil {
		envs["minProperties"] = 1
	}
	if workloads := indexPath(doc, []string{"workloads"}); workloads != nil {
		workloads["minProperties"] = 1
	}

	sources := []any{"build", "image", "compose"}
	jobOnly := []any{"deployment_phase", "operator_run", "data_effect", "schedule", "inputs", "execution"}
	workload["allOf"] = []any{
		map[string]any{
			"if": map[string]any{"required": []any{"execution"}},
			"then": map[string]any{
				"required": []any{"schedule", "data_effect"},
				"properties": map[string]any{
					"data_effect":      map[string]any{"const": "none"},
					"deployment_phase": map[string]any{"const": "none"},
					"operator_run":     map[string]any{"const": "allowed"},
				},
				"not": map[string]any{"required": []any{"compose"}},
			},
		},
		// Exactly one source. A workload with none cannot run and a workload
		// with two does not say which image it is.
		map[string]any{"oneOf": anyRequired(sources)},
		// A published host socket is singular, so it cannot be held by both
		// sides of a rolling handover. Include the authored default case:
		// application + health + no strategy is rolling after normalization.
		map[string]any{"not": map[string]any{"allOf": []any{
			map[string]any{"required": []any{"published_ports"}},
			map[string]any{"anyOf": []any{
				map[string]any{
					"properties": map[string]any{"strategy": map[string]any{"const": "rolling"}},
					"required":   []any{"strategy"},
				},
				map[string]any{"allOf": []any{
					map[string]any{"not": map[string]any{"required": []any{"strategy"}}},
					map[string]any{"required": []any{"health"}},
					map[string]any{"anyOf": []any{
						map[string]any{
							"properties": map[string]any{"role": map[string]any{"const": RoleApplication}},
							"required":   []any{"role"},
						},
						map[string]any{"not": map[string]any{"required": []any{"role"}}},
					}},
				}},
			}},
		}}},
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
	forbidden = append(forbidden, map[string]any{"pattern": "^onebox-"})
	for _, name := range reservedAppNames {
		forbidden = append(forbidden, map[string]any{"const": name})
	}
	out := pattern(gIdent)
	out["not"] = map[string]any{"anyOf": forbidden}
	out["description"] = "The application's name. Expects " + gIdent.means +
		", and may not begin \"onebox-\" or be a name the host layout reserves."
	return out
}

// serviceNamesConstraint holds the service names the host proxy reserves.
func serviceNamesConstraint() map[string]any {
	forbidden := make([]any, 0, len(reservedServiceNames))
	for _, name := range reservedServiceNames {
		forbidden = append(forbidden, map[string]any{"const": name})
	}
	return map[string]any{"propertyNames": map[string]any{"not": map[string]any{"anyOf": forbidden}}}
}

func bindSourceConstraint() map[string]any {
	out := pattern(gBindSource)
	out["not"] = map[string]any{"pattern": `(^|/)\.\.(/|$)`}
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
