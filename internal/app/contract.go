package app

import (
	"fmt"
	"reflect"
	"strings"
	"unicode"
)

// Application is the authored Kubernetes-shaped resource envelope. Spec is
// kept as the runtime model so the contract reset does not leak into machine
// artifacts or the engine's internal representation.
type Application struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   ApplicationMeta `json:"metadata"`
	Spec       Spec            `json:"spec"`
}

type ApplicationMeta struct {
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

const ApplicationKind = "Application"

func lowerCamel(name string) string {
	parts := strings.Split(name, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	return strings.Join(parts, "")
}

func upperCamel(value string) string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '_' || r == '-' })
	for i, part := range parts {
		switch strings.ToLower(part) {
		case "id":
			parts[i] = "ID"
		case "url":
			parts[i] = "URL"
		case "http":
			parts[i] = "HTTP"
		case "json":
			parts[i] = "JSON"
		case "ssh":
			parts[i] = "SSH"
		case "uid":
			parts[i] = "UID"
		case "gid":
			parts[i] = "GID"
		default:
			runes := []rune(part)
			if len(runes) > 0 {
				runes[0] = unicode.ToUpper(runes[0])
			}
			parts[i] = string(runes)
		}
	}
	return strings.Join(parts, "")
}

// These are Onebox constants, rather than external protocol/data tokens.
// Internally they deliberately retain their historical spellings so machine
// artifacts do not change as a side effect of the authored API reset.
var declarativeEnums = map[string][]string{
	"pull":             eImagePull,
	"role":             eRole,
	"strategy":         eStrategy,
	"deployment_phase": eJobDeploymentPhase,
	"operator_run":     eJobOperatorRun,
	"data_effect":      eDataEffect,
	"provider":         eSecretProvider,
	"condition":        eNeedCondition,
	"mode": append(append(append([]string{}, eMountMode...), ePersistence...),
		eEncryptionMode...),
	"tls":              append(append([]string{}, eRouteTLS...), eBackupTLS...),
	"notify":           eScheduleNotify,
	"deploy_lock":      eScheduleDeployLock,
	"policy":           eMigrationPolicy,
	"migration_policy": eMigrationPolicy,
	"format":           eNotifyFormat,
	"on":               eNotifyEvent,
	"kind": append(append(append(append([]string{}, eProxyKind...), eBackupTargetKind...),
		eRecoveryKind...), eExternalProbeKind...),
	"part":          eConnectionPart,
	"recovery_kind": eRecoveryKind,
	"pitr":          eEncryptionMode,
	"cold":          eEncryptionMode,
	"snapshot":      eEncryptionMode,
}

func internalEnum(field, public string) (string, bool) {
	for _, value := range declarativeEnums[field] {
		if public == upperCamel(value) {
			return value, true
		}
	}
	return public, len(declarativeEnums[field]) == 0
}

func publicEnum(field, internal string) string {
	for _, value := range declarativeEnums[field] {
		if internal == value {
			return upperCamel(value)
		}
	}
	return internal
}

func authoredFieldsOf(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || (t == reflect.TypeOf(Spec{}) && (name == "api_version" || name == "app")) {
			continue
		}
		out[lowerCamel(name)] = f
	}
	return out
}

func authoredToInternal(t reflect.Type, value any, fieldName, path string) (any, error) {
	t = deref(t)
	switch t.Kind() {
	case reflect.Struct:
		body, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		out := map[string]any{}
		fields := authoredFieldsOf(t)
		for key, child := range body {
			field, exists := fields[key]
			if !exists {
				return nil, errf("unknown_field", join2(path, key), "", "%q is not a field of this contract", key)
			}
			legacy, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			var converted any
			var err error
			if t.Name() == "Overrides" && (legacy == "workloads" || legacy == "services") {
				target := reflect.TypeOf(Workload{})
				if legacy == "services" {
					target = reflect.TypeOf(Service{})
				}
				converted, err = authoredPatchMap(target, child, join2(path, key))
			} else {
				converted, err = authoredToInternal(field.Type, child, legacy, join2(path, key))
			}
			if err != nil {
				return nil, err
			}
			out[legacy] = converted
		}
		return out, nil
	case reflect.Map:
		body, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		out := map[string]any{}
		for key, child := range body {
			mapKey := key
			if fieldName == "hooks" {
				for _, seam := range eHookSeam {
					if key == upperCamel(seam) {
						mapKey = seam
						break
					}
				}
			}
			converted, err := authoredToInternal(t.Elem(), child, fieldName, join2(path, key))
			if err != nil {
				return nil, err
			}
			out[mapKey] = converted
		}
		return out, nil
	case reflect.Slice:
		items, ok := value.([]any)
		if !ok {
			return value, nil
		}
		out := make([]any, len(items))
		for i, child := range items {
			converted, err := authoredToInternal(t.Elem(), child, fieldName, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	case reflect.String:
		text, ok := value.(string)
		if !ok {
			return value, nil
		}
		if fieldName == "provider" && !strings.Contains(path, "envFiles") &&
			!strings.Contains(path, "credentials.provider") &&
			!strings.Contains(path, "connection.source.provider") {
			return text, nil
		}
		converted, valid := internalEnum(fieldName, text)
		if !valid {
			allowed := declarativeEnums[fieldName]
			public := make([]string, len(allowed))
			for i, candidate := range allowed {
				public[i] = upperCamel(candidate)
			}
			return nil, errf("project_invalid", path, "", "%q is not accepted; expected one of %s", text, strings.Join(public, ", "))
		}
		return converted, nil
	default:
		return value, nil
	}
}

func authoredPatchMap(target reflect.Type, value any, path string) (any, error) {
	body, ok := value.(map[string]any)
	if !ok {
		return value, nil
	}
	out := map[string]any{}
	for name, patch := range body {
		converted, err := authoredToInternal(target, patch, "", join2(path, name))
		if err != nil {
			return nil, err
		}
		out[name] = converted
	}
	return out, nil
}

func internalToAuthored(t reflect.Type, value any, fieldName string) any {
	t = deref(t)
	switch t.Kind() {
	case reflect.Struct:
		body, _ := value.(map[string]any)
		out := map[string]any{}
		for legacy, field := range fieldsOf(t) {
			if t == reflect.TypeOf(Spec{}) && (legacy == "api_version" || legacy == "app") {
				continue
			}
			if child, ok := body[legacy]; ok {
				if t.Name() == "Overrides" && (legacy == "workloads" || legacy == "services") {
					target := reflect.TypeOf(Workload{})
					if legacy == "services" {
						target = reflect.TypeOf(Service{})
					}
					patches := map[string]any{}
					for name, patch := range child.(map[string]any) {
						patches[name] = internalToAuthored(target, patch, "")
					}
					out[lowerCamel(legacy)] = patches
				} else {
					out[lowerCamel(legacy)] = internalToAuthored(field.Type, child, legacy)
				}
			}
		}
		return out
	case reflect.Map:
		body, _ := value.(map[string]any)
		out := map[string]any{}
		for key, child := range body {
			if fieldName == "hooks" {
				key = upperCamel(key)
			}
			out[key] = internalToAuthored(t.Elem(), child, fieldName)
		}
		return out
	case reflect.Slice:
		items, _ := value.([]any)
		out := make([]any, len(items))
		for i, child := range items {
			out[i] = internalToAuthored(t.Elem(), child, fieldName)
		}
		return out
	case reflect.String:
		if text, ok := value.(string); ok {
			return publicEnum(fieldName, text)
		}
	}
	return value
}

func publicPath(path string) string {
	if path == "" {
		return path
	}
	parts := strings.Split(path, ".")
	for i, part := range parts {
		base := part
		suffix := ""
		if bracket := strings.IndexByte(part, '['); bracket >= 0 {
			base, suffix = part[:bracket], part[bracket:]
		}
		public := lowerCamel(base)
		if i > 0 && lowerCamel(strings.SplitN(parts[i-1], "[", 2)[0]) == "hooks" {
			// Hook map keys are either fixed lifecycle seams or authored job names.
			// Preserve job names byte-for-byte and publish only the fixed seams in
			// their UpperCamelCase contract spelling.
			public = base
			for _, seam := range eHookSeam {
				if base == seam {
					public = upperCamel(seam)
					break
				}
			}
		}
		parts[i] = public + suffix
	}
	return "spec." + strings.Join(parts, ".")
}

func authoredOriginPath(path string) string {
	switch path {
	case "app":
		return "metadata.name"
	case "api_version":
		return "apiVersion"
	default:
		return publicPath(path)
	}
}
