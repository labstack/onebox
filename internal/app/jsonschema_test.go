package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// The published schema is what an editor reads. If it disagrees with the loader
// an author is taught something untrue — underlined where they are right, or
// silent where they are wrong — so it is held to the same corpus.
//
// It is deliberately allowed to be more permissive than the loader on rules a
// JSON Schema cannot express: cross-field exclusivity, prerequisite existence,
// derived-name length. Those are listed, not waved away, so the gap is a
// decision rather than an accident.
func compiledSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	body, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(SchemaID, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(SchemaID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// asJSON converts the authored YAML the way a language server would.
func asJSON(t *testing.T, in string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(in), &v); err != nil {
		t.Fatalf("fixture is not YAML: %v", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("fixture does not convert: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// beyondJSONSchema are the corpus cases the loader rejects for a reason no
// JSON Schema can express. Each is a cross-field or semantic rule, and each is
// still enforced — by the loader, which is what runs before a deploy.
var beyondJSONSchema = map[string]string{
	// Two structures have to agree, which a schema evaluates independently.
	"proxy kind none with a route":      "a route needs something to route it, which is a fact about the proxy block",
	"workload and service share a name": "an identifier is unique across workloads and services, which are separate objects",

	// Facts about values that only resolution knows.
	"unknown prerequisite":             "a prerequisite must name something the project declares",
	"healthy condition without health": "a wait for health needs a health check to wait on",
	"derived name too long":            "the length of a name Onebox derives, not one written here",
	"unknown service driver":           "a driver Onebox has an implementation for",
	"absolute env_file":                "a path that resolves inside the repository after joining",
	"absolute compose ref":             "a path that resolves inside the repository after joining",
	"protection self target":           "a target and environment host are declared in separate objects",
	"protection unsupported objective": "a recovery kind is qualified by the selected service driver",
	"protection sparse drill":          "cron cadence must be compared with restore proof age",

	// Exclusivity within one object that a schema could express, and does not
	// here because the resulting document would be harder to read than the
	// rule it encodes.
	"verification url with exec":          "a verification is exactly one kind",
	"verification workload without probe": "an http or exec check names the workload it runs in",
}

func TestPublishedSchemaMatchesTheLoader(t *testing.T) {
	schema := compiledSchema(t)

	for _, c := range conformanceCases() {
		accepted := schema.Validate(asJSON(t, c.yaml)) == nil

		if c.ok && !accepted {
			t.Errorf("%s: the loader accepts this and the published schema does not — "+
				"an editor would underline a correct project\n%s", c.name, c.yaml)
			continue
		}
		if !c.ok && accepted {
			// Permitted only where a JSON Schema structurally cannot decide.
			if _, known := beyondJSONSchema[c.name]; !known {
				t.Errorf("%s: the loader rejects this and the published schema accepts it, "+
					"and it is not a rule JSON Schema cannot express\n%s", c.name, c.yaml)
			}
		}
	}
}

// Every real project must validate, or the schema is not describing the
// contract people actually write.
func TestPublishedSchemaAcceptsEveryRealProject(t *testing.T) {
	schema := compiledSchema(t)
	for _, path := range corpusProjects(t) {
		body := readFixture(t, path)
		if err := schema.Validate(asJSON(t, body)); err != nil {
			t.Errorf("%s: a real project fails the published schema:\n%v", path, err)
		}
	}
}

// The shorthand an author actually writes must validate. This is the failure
// mode a schema generated from the model alone would have.
func TestPublishedSchemaAcceptsAuthoredShorthand(t *testing.T) {
	schema := compiledSchema(t)
	for _, y := range []string{
		"api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: root@h}}\nimage: nginx\n",
		"api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: root@h}}\nworkloads: {w: {image: nginx}}\nservices: {postgres: 17}\n",
		"api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: root@h}}\nworkloads: {w: {image: nginx, volumes: [{name: data, path: /data}], needs: [db], command: run}}\n",
		"api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: root@h}}\nworkloads: {w: {image: nginx}}\nhooks: {post_deploy: \"echo done\"}\n",
		"api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: root@h}}\nworkloads: {w: {image: nginx}}\nx-note: anything\n",
	} {
		if err := schema.Validate(asJSON(t, y)); err != nil {
			t.Errorf("authored shorthand rejected:\n%s\n%v", y, err)
		}
	}
}

// And it must still refuse a field the contract does not define, which is the
// completion and error support the schema exists to provide.
func TestPublishedSchemaRefusesAnUndefinedField(t *testing.T) {
	schema := compiledSchema(t)
	y := "api_version: onebox.run/v1\napp: a\nenvironments: {p: {server: root@h}}\nworkloads: {w: {image: nginx, replicaz: 3}}\n"
	if err := schema.Validate(asJSON(t, y)); err == nil {
		t.Error("the published schema accepted a field the contract does not define")
	} else if !strings.Contains(err.Error(), "replicaz") {
		t.Errorf("the failure should name the field: %v", err)
	}
}

func TestPublishedSchemaDocumentsEveryPublicField(t *testing.T) {
	body, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}

	var walk func(any, string)
	walk = func(value any, path string) {
		switch node := value.(type) {
		case map[string]any:
			// Closed objects are model structs. Conditional schemas also use a
			// properties key, but those entries are predicates rather than fields.
			if closed, ok := node["additionalProperties"].(bool); ok && !closed {
				if properties, ok := node["properties"].(map[string]any); ok {
					for name, raw := range properties {
						property, ok := raw.(map[string]any)
						if !ok {
							t.Errorf("%s.%s is not an object schema", path, name)
							continue
						}
						if strings.TrimSpace(stringValue(property["description"])) == "" {
							t.Errorf("%s.%s has no description", path, name)
						}
					}
				}
			}
			for key, child := range node {
				walk(child, path+"."+key)
			}
		case []any:
			for _, child := range node {
				walk(child, path+"[]")
			}
		}
	}
	walk(doc, "$")
}

func TestPublishedSchemaDocumentsImportantDefaultsAndExamples(t *testing.T) {
	body, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		path     []string
		key      string
		expected any
	}{
		{[]string{"base_path"}, "default", "/var/lib/ob"},
		{[]string{"deployment", "retain_releases"}, "default", float64(5)},
		{[]string{"environments", "*", "policy", "require_approval"}, "default", true},
		{[]string{"workloads", "*", "replicas"}, "default", float64(1)},
		{[]string{"app"}, "examples", []any{"shop"}},
	}
	for _, check := range checks {
		at := indexPath(doc, check.path)
		if at == nil {
			t.Errorf("schema path %v is missing", check.path)
			continue
		}
		if got, ok := at[check.key]; !ok || !reflect.DeepEqual(got, check.expected) {
			t.Errorf("schema path %v %s = %#v, want %#v", check.path, check.key, got, check.expected)
		}
	}
}

func TestCheckedInSchemaMatchesGenerator(t *testing.T) {
	body, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), body...), '\n')
	path := filepath.Join("..", "..", "docs", "onebox.run-v1.schema.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published schema: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate it with `go run ./cmd/ob schema --to docs/onebox.run-v1.schema.json`", path)
	}
}

// The schema is only useful to an author if the guide tells them the URL to
// point their editor at. A published identity nobody is told about helps no one.
func TestPublishedSchemaURLIsUsedByTheHumanGuide(t *testing.T) {
	if !strings.HasPrefix(SchemaID, "https://raw.githubusercontent.com/labstack/onebox/main/") {
		t.Fatalf("schema identity must be a retrievable main-branch URL, got %q", SchemaID)
	}
	guides := []string{
		"site/src/content/docs/start/reading-it-back.mdx",
		"site/src/content/docs/reference/project-file.mdx",
	}
	for _, guide := range guides {
		body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(guide)))
		if err != nil {
			t.Fatalf("%s must exist: %v", guide, err)
		}
		if !strings.Contains(string(body), SchemaID) {
			t.Fatalf("%s does not tell an author the published schema URL %q", guide, SchemaID)
		}
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
