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

	// Matching the key anywhere in the concatenated pages let prose stand in for
	// documentation: `app` matched nine pages, `port` ten. A key is documented
	// when it owns a page or has a table row — not when its name occurs in a
	// sentence.
	for key := range props {
		if _, hasPage := pages["fields/"+key+".mdx"]; hasPage {
			continue
		}
		documented := false
		for _, page := range pages {
			if rowFor(page, key) != "" {
				documented = true
				break
			}
		}
		if !documented {
			t.Errorf("top-level key %q owns no page and has no table row", key)
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

// The site's content schema rejects an unknown status at build time, which is
// the real enforcement. This catches it one step earlier, where the failure
// names the block instead of the rendered file — and it iterates the constants
// rather than restating them, so the list cannot be a fourth copy that agrees
// with nothing.
func TestGeneratedStatusesAreKnown(t *testing.T) {
	known := map[status]bool{}
	for _, s := range knownStatuses() {
		known[s] = true
	}
	for _, b := range blocks {
		if !known[b.Status] {
			t.Errorf("block %q declares unknown status %q", b.Key, b.Status)
		}
	}
}

// Rendering twice must produce the same bytes. It did not: the renderer aliased
// the package-level registry and appended through the pointers, so a second call
// in one process emitted every row twice — and this file's own second call was
// asserting against doubled pages.
func TestRenderingTwiceProducesTheSameBytes(t *testing.T) {
	schema, err := loadSchema()
	if err != nil {
		t.Fatalf("cannot load the schema: %v", err)
	}
	first, err := renderFieldPages(schema)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	second, err := renderFieldPages(schema)
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("page count changed between renders: %d then %d", len(first), len(second))
	}
	for name, want := range first {
		if got := second[name]; got != want {
			t.Errorf("%s differs between renders: %d bytes then %d", name, len(want), len(got))
		}
	}
}

// `required` under oneOf/anyOf is a per-branch fact. Merging it published
// `build` as mandatory on every workload when the schema says exactly one of
// build, image or compose — the reference contradicting the loader in the one
// column an author cannot afford to have wrong.
func TestExclusiveAlternativesAreNotPublishedAsRequired(t *testing.T) {
	schema, err := loadSchema()
	if err != nil {
		t.Fatalf("cannot load the schema: %v", err)
	}
	pages, err := renderFieldPages(schema)
	if err != nil {
		t.Fatalf("cannot render: %v", err)
	}

	for _, path := range []string{"<name>.build", "<name>.image", "<name>.compose"} {
		if strings.Contains(pages["fields/workloads.mdx"], "`"+path+"` `*`") {
			t.Errorf("%s is marked required, but the schema makes it one of three alternatives", path)
		}
	}
	for _, path := range []string{"<name>.volumes[].name", "<name>.volumes[].path"} {
		if strings.Contains(pages["fields/workloads.mdx"], "`"+path+"` `*`") {
			t.Errorf("%s is marked required, but source/target is an equally valid pair", path)
		}
	}
}

// A field whose object form carries sub-rows must not be typed by its scalar
// shorthand. `image` rendered as `string` with five object sub-fields listed
// directly beneath it.
func TestShorthandDoesNotWinTheTypeCell(t *testing.T) {
	schema, err := loadSchema()
	if err != nil {
		t.Fatalf("cannot load the schema: %v", err)
	}
	pages, err := renderFieldPages(schema)
	if err != nil {
		t.Fatalf("cannot render: %v", err)
	}
	page := pages["fields/workloads.mdx"]
	for _, path := range []string{"<name>.image", "<name>.health", "<name>.build"} {
		row := rowFor(page, path)
		if row == "" {
			t.Errorf("%s has no row", path)
			continue
		}
		if !strings.Contains(page, "`"+path+".") {
			continue // no sub-fields; a scalar type is honest
		}
		if strings.Contains(row, "| string |") {
			t.Errorf("%s is typed `string` but has object sub-fields on the same page:\n  %s", path, row)
		}
	}
}

// Every top-level key must reach a page with its whole subtree. Emitting only
// the key's own row documented `routes` as a bare list and lost all seven of its
// item fields.
func TestUnclaimedTopLevelKeysDocumentTheirSubtree(t *testing.T) {
	schema, err := loadSchema()
	if err != nil {
		t.Fatalf("cannot load the schema: %v", err)
	}
	pages, err := renderFieldPages(schema)
	if err != nil {
		t.Fatalf("cannot render: %v", err)
	}
	page := pages["fields/top-level.mdx"]
	for _, path := range []string{"routes[].domain", "health.http", "image.reference", "build.context"} {
		if !strings.Contains(page, "`"+path+"`") {
			t.Errorf("top-level.mdx does not document %q", path)
		}
	}
}

// A block whose key the schema no longer has produces a live page reading "This
// block declares no fields." Nothing else would notice.
func TestEveryRegisteredBlockMatchesASchemaKey(t *testing.T) {
	schema, err := loadSchema()
	if err != nil {
		t.Fatalf("cannot load the schema: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	for _, b := range blocks {
		if _, ok := props[b.Key]; !ok {
			t.Errorf("block %q has no matching top-level schema key", b.Key)
		}
	}
}

func rowFor(page, path string) string {
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "| `"+path+"`") {
			return line
		}
	}
	return ""
}

// The published schema and the checked-in copy are the same artifact. If they
// diverge, an editor and the loader disagree.
func TestPublishedSchemaMatchesTheCheckedInCopy(t *testing.T) {
	generated, err := app.JSONSchema()
	if err != nil {
		t.Fatalf("cannot produce the schema: %v", err)
	}
	// Skipping on a read failure would turn "someone moved the file" into a
	// passing test, which is the drift this exists to catch.
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "docs", "onebox.run-v1.schema.json"))
	if err != nil {
		t.Fatalf("the checked-in schema must be readable: %v", err)
	}
	if strings.TrimSpace(string(generated)) != strings.TrimSpace(string(onDisk)) {
		t.Error("the published schema differs from docs/onebox.run-v1.schema.json")
	}
}

// The committed pages must be what this binary would write.
//
// `--check` enforces the same thing, but only when someone runs the recipe. The
// repository has no CI, and `just check` is `test vet` — so without this, the
// whole suite can be green while every published field description is stale.
// This puts the guarantee in `go test ./...`, where it is unavoidable.
//
// The CLI page needs a built `ob` and is covered by `--check` plus
// TestEveryCommandIsDocumented in cmd/ob; everything else is pure.
func TestCommittedPagesMatchTheGenerator(t *testing.T) {
	schema, err := loadSchema()
	if err != nil {
		t.Fatalf("cannot load the schema: %v", err)
	}
	want, err := renderFieldPages(schema)
	if err != nil {
		t.Fatalf("cannot render: %v", err)
	}
	want["errors.mdx"] = renderErrorPage()

	root := filepath.Join("..", "..", "site", "src", "content", "docs", "reference")
	for name, body := range want {
		onDisk, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s is missing — run `just docs-generate`: %v", name, err)
			continue
		}
		if string(onDisk) != body {
			t.Errorf("%s is stale — run `just docs-generate`", name)
		}
	}
}

// A generated page nothing produces any more stays committed and published, so
// verify has to look at the directory as well as at its own output.
func TestVerifyReportsOrphanedPages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kept.mdx"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := "---\ntitle: Ghost\n---\n\n" + generatedMarker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ghost.mdx"), []byte(orphan), 0o644); err != nil {
		t.Fatal(err)
	}
	// A hand-authored page carries no marker and is none of our business.
	if err := os.WriteFile(filepath.Join(dir, "authored.mdx"), []byte("---\ntitle: Mine\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verify(dir, map[string]string{"kept.mdx": "body"})
	if err == nil {
		t.Fatal("verify accepted an orphaned generated page")
	}
	if !strings.Contains(err.Error(), "ghost.mdx") {
		t.Errorf("the orphan is not named in the failure: %v", err)
	}
	if strings.Contains(err.Error(), "authored.mdx") {
		t.Errorf("a hand-authored page was reported as an orphan: %v", err)
	}
}

func TestVerifyReportsMissingAndDifferingPages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "same.mdx"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "different.mdx"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verify(dir, map[string]string{
		"same.mdx":      "same",
		"different.mdx": "new",
		"absent.mdx":    "anything",
	})
	if err == nil {
		t.Fatal("verify accepted a stale directory")
	}
	for _, want := range []string{"different.mdx", "absent.mdx", "missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure does not mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "same.mdx") {
		t.Errorf("an identical page was reported stale: %v", err)
	}
}

// A help text with no command section means cobra's template changed, and the
// reference would otherwise be published with every command section missing.
func TestSubcommandsParsesCobraHelp(t *testing.T) {
	grouped := `Usage:
  ob [command]

Core Commands:
  deploy      release
  plan        propose

Recovery Commands:
  abort       revert
  resume      finish

Flags:
  -h, --help
`
	got, err := subcommands(grouped)
	if err != nil {
		t.Fatalf("grouped help: %v", err)
	}
	want := []string{"abort", "deploy", "plan", "resume"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("grouped help: got %v, want %v", got, want)
	}

	if _, err := subcommands("Usage:\n  ob thing\n\nFlags:\n  -h, --help\n"); err == nil {
		t.Error("a help text with no command section must be an error, not an empty list")
	}

	deduped, err := subcommands("Available Commands:\n  plan  a\n\nAdditional Commands:\n  plan  a\n  help  x\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deduped, ",") != "plan" {
		t.Errorf("expected a single deduplicated command, got %v", deduped)
	}
}
