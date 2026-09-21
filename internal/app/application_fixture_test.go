package app

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadFixtureBytes repairs only v1alpha1 test fragments assembled through Go
// string concatenation. Repository-owned YAML files and the shipped loader do
// not pass through it. Direct contract tests below call LoadBytes itself.
func loadFixtureBytes(body []byte, filename string) (*Spec, error) {
	return LoadBytes(normalizeApplicationFixture(body), filename)
}

func TestAuthoredVocabularyIsMechanicallyNormalized(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		typ = deref(typ)
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Struct:
			for name, field := range authoredFieldsOf(typ) {
				if strings.Contains(name, "_") || name != lowerCamel(name) {
					t.Errorf("%s.%s publishes non-lowerCamel field %q", typ.Name(), field.Name, name)
				}
				walk(field.Type)
			}
		case reflect.Map, reflect.Slice, reflect.Array:
			walk(typ.Elem())
		}
	}
	walk(reflect.TypeOf(Application{}))

	for field, values := range declarativeEnums {
		for _, internal := range values {
			public := publicEnum(field, internal)
			if public != upperCamel(internal) {
				t.Errorf("%s value %q publishes as %q", field, internal, public)
			}
			if roundTrip, ok := internalEnum(field, public); !ok || roundTrip != internal {
				t.Errorf("%s value %q does not round-trip through %q", field, internal, public)
			}
		}
	}
}

func TestPublishedApplicationFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "api", "testdata", "application")
	for _, tc := range []struct {
		dir string
		ok  bool
	}{{"valid", true}, {"invalid", false}} {
		entries, err := os.ReadDir(filepath.Join(root, tc.dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			t.Run(tc.dir+"/"+entry.Name(), func(t *testing.T) {
				path := filepath.Join(root, tc.dir, entry.Name())
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = LoadBytes(body, path)
				if (err == nil) != tc.ok {
					t.Fatalf("load error = %v, want valid=%v", err, tc.ok)
				}
			})
		}
	}
}

func normalizeApplicationFixture(body []byte) []byte {
	var root map[string]any
	if yaml.Unmarshal(body, &root) != nil || root["apiVersion"] != APIVersion {
		return body
	}
	spec, _ := root["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		root["spec"] = spec
	}
	for key, value := range root {
		switch key {
		case "apiVersion", "kind", "metadata", "spec":
		default:
			if existing, ok := spec[key].(map[string]any); ok {
				if incoming, ok := value.(map[string]any); ok {
					for name, child := range incoming {
						existing[name] = child
					}
				} else {
					spec[key] = value
				}
			} else {
				spec[key] = value
			}
			delete(root, key)
		}
	}
	metadata, _ := root["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	shorthand := map[string]any{}
	for _, key := range fixtureShorthandKeys {
		if value, ok := spec[key]; ok {
			shorthand[key] = value
			delete(spec, key)
		}
	}
	if len(shorthand) > 0 {
		workloads, _ := spec["workloads"].(map[string]any)
		if workloads == nil {
			workloads = map[string]any{}
			spec["workloads"] = workloads
		}
		if len(workloads) == 0 {
			workloads[name] = shorthand
		} else {
			for key, value := range shorthand {
				spec[key] = value
			}
		}
	}
	normalizeFixtureMap(spec, "spec")
	normalized, err := yaml.Marshal(root)
	if err != nil {
		return body
	}
	return normalized
}

var fixtureShorthandKeys = []string{"build", "image", "compose", "port", "health", "routes"}

var fixtureIdentifierMaps = map[string]bool{
	"environments": true, "workloads": true, "services": true,
	"external_services": true, "externalServices": true,
	"backup_targets": true, "backupTargets": true,
	"notifications": true, "registries": true, "entrypoints": true,
	"env": true, "labels": true, "settings": true, "args": true,
	"inputs": true, "annotations": true, "extensions": true, "hooks": true,
}

func normalizeFixtureMap(value map[string]any, field string) {
	currentIdentifiers := fixtureIdentifierMaps[field]
	for key, child := range value {
		publicKey := key
		if currentIdentifiers {
			if field == "hooks" {
				for _, seam := range eHookSeam {
					if key == seam {
						publicKey = upperCamel(key)
					}
				}
			}
		} else {
			publicKey = lowerCamel(key)
		}
		normalized := normalizeFixtureValue(child, key)
		if publicKey != key {
			delete(value, key)
		}
		value[publicKey] = normalized
	}
}

func normalizeFixtureValue(value any, field string) any {
	switch typed := value.(type) {
	case map[string]any:
		normalizeFixtureMap(typed, field)
		return typed
	case []any:
		for i, item := range typed {
			typed[i] = normalizeFixtureValue(item, field)
		}
		return typed
	case string:
		if field == "provider" && typed != "sops" && typed != "Sops" {
			return typed
		}
		if _, ok := declarativeEnums[field]; ok {
			for _, candidate := range declarativeEnums[field] {
				if typed == candidate || strings.EqualFold(typed, upperCamel(candidate)) {
					return upperCamel(candidate)
				}
			}
		}
		return typed
	default:
		return value
	}
}

func TestApplicationContractRejectsLegacyForms(t *testing.T) {
	legacy := []string{
		"api_version: onebox.run/v1\napp: shop\nenvironments: {}\nimage: nginx\n",
		"apiVersion: onebox.run/v1\nkind: Application\nmetadata: {name: shop}\nspec: {environments: {}, workloads: {}}\n",
		"apiVersion: onebox.run/v1alpha1\nkind: Application\nmetadata: {name: shop}\nspec: {environments: {}, workloads: {}, base_path: /srv/ob}\n",
		"apiVersion: onebox.run/v1alpha1\nkind: Application\nmetadata: {name: shop}\nspec: {environments: {}, workloads: {web: {image: nginx, role: application}}}\n",
		"apiVersion: onebox.run/v1alpha1\nkind: Application\nmetadata: {name: shop}\nspec: {environments: {}}\nimage: nginx\n",
		"apiVersion: onebox.run/v1alpha1\nkind: Application\nmetadata: {name: shop}\nspec: {environments: {}, workloads: {}, x-behavior: true}\n",
	}
	for _, source := range legacy {
		if _, err := LoadBytes([]byte(source), "legacy.yml"); err == nil {
			t.Errorf("legacy form was accepted:\n%s", source)
		}
	}
}

func TestApplicationContractRejectsMalformedAnnotations(t *testing.T) {
	for name, annotations := range map[string]string{
		"scalar":     "nope",
		"list":       "[nope]",
		"null":       "null",
		"non-string": "{note: 1}",
	} {
		t.Run(name, func(t *testing.T) {
			source := `apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: shop
  annotations: ` + annotations + `
spec:
  environments: {}
  workloads: {}
`
			_, err := LoadBytes([]byte(source), "annotations.yml")
			if err == nil {
				t.Fatal("malformed annotations were accepted")
			}
			var contractErr *Error
			if !errors.As(err, &contractErr) || contractErr.Code != "project_invalid" || !strings.HasPrefix(contractErr.Path, "metadata.annotations") {
				t.Fatalf("error = %#v, want project_invalid at metadata.annotations", err)
			}
		})
	}
}

func TestProviderNativeValuesRemainData(t *testing.T) {
	source := `apiVersion: onebox.run/v1alpha1
kind: Application
metadata: {name: shop}
spec:
  environments: {production: {server: deploy@example.com}}
  workloads:
    migrate: {role: Job, image: migrate:1, dataEffect: Migration}
  checks:
    migrations:
      - {job: migrate, provider: atlas, appliedRevisions: ["202609200001"]}
  proxy:
    managed: true
    config: traefik
    dnsChallenge: {provider: cloudflare}
`
	loaded, err := LoadBytes([]byte(source), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Checks.Migrations[0].Provider; got != "atlas" {
		t.Fatalf("migration provider = %q", got)
	}
	if got := loaded.Proxy.DNSChallenge.Provider; got != "cloudflare" {
		t.Fatalf("DNS provider = %q", got)
	}
}
