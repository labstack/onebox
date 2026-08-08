package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/onebox"
)

// The generated pages are only trustworthy if they cannot fall behind the
// binary. `just docs-check` runs `ob-docgen --check`, which is the enforcement;
// these tests cover the properties that check depends on.

func TestEveryLoaderErrorCodeIsDocumented(t *testing.T) {
	page := renderErrorPage()
	for _, code := range app.ErrorCodes() {
		if !strings.Contains(page, "`"+code+"`") {
			t.Errorf("loader error code %q is missing from the generated page", code)
		}
	}
}

func TestEveryLifecycleCodeIsDocumentedWithItsResolvingCommand(t *testing.T) {
	page := renderErrorPage()
	for _, code := range onebox.LifecycleFailureCodes() {
		failure, err := onebox.NewLifecycleFailure(code)
		if err != nil {
			t.Fatalf("lifecycle code %q does not resolve: %v", code, err)
		}
		if !strings.Contains(page, "`"+code+"`") {
			t.Errorf("lifecycle code %q is missing from the generated page", code)
		}
		if !strings.Contains(page, "`"+failure.Next+"`") {
			t.Errorf("lifecycle code %q is documented without its resolving command", code)
		}
	}
}

// A top-level key in the Go model that no block claims must still reach the
// documentation, or adding a field would silently remove it from the published
// reference. It lands on the top-level page instead of vanishing.
func TestEveryTopLevelSchemaKeyAppearsOnSomePage(t *testing.T) {
	schema, err := loadSchema()
	if err != nil {
		t.Fatalf("cannot load the schema: %v", err)
	}
	pages, err := renderFieldPages(schema)
	if err != nil {
		t.Fatalf("cannot render field pages: %v", err)
	}

	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		t.Fatal("the schema has no top-level properties")
	}

	joined := strings.Join(values(pages), "\n")
	for key := range props {
		if !strings.Contains(joined, key) {
			t.Errorf("top-level key %q appears on no generated page", key)
		}
	}
}

// Frontmatter is what llms.txt and the Markdown alternates are built from, so
// a generated page without it would be invisible to an agent.
func TestGeneratedPagesCarryAgentFrontmatter(t *testing.T) {
	schema, err := loadSchema()
	if err != nil {
		t.Fatalf("cannot load the schema: %v", err)
	}
	pages, err := renderFieldPages(schema)
	if err != nil {
		t.Fatalf("cannot render field pages: %v", err)
	}
	pages["errors.mdx"] = renderErrorPage()

	for name, body := range pages {
		for _, required := range []string{"title:", "summary:", "status:", "generated: true", generatedMarker} {
			if !strings.Contains(body, required) {
				t.Errorf("%s is missing %q", name, required)
			}
		}
		if strings.Contains(body, "<!--") {
			t.Errorf("%s contains an HTML comment, which MDX parses as JSX", name)
		}
	}
}

// A status a page cannot honour is the failure this whole scheme exists to
// prevent, so the values are constrained to the three the content schema knows.
func TestGeneratedStatusesAreKnown(t *testing.T) {
	known := map[string]bool{"shipped": true, "schema-only": true, "intent-only": true}
	for _, b := range blocks {
		if !known[b.Status] {
			t.Errorf("block %q declares unknown status %q", b.Key, b.Status)
		}
	}
}

// The published schema and the checked-in copy are the same artifact. If they
// diverge, an editor and the loader disagree.
func TestPublishedSchemaMatchesTheCheckedInCopy(t *testing.T) {
	generated, err := app.JSONSchema()
	if err != nil {
		t.Fatalf("cannot produce the schema: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "docs", "onebox.run-v1.schema.json"))
	if err != nil {
		t.Skipf("checked-in schema not readable: %v", err)
	}
	if strings.TrimSpace(string(generated)) != strings.TrimSpace(string(onDisk)) {
		t.Error("the published schema differs from docs/onebox.run-v1.schema.json")
	}
}

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
