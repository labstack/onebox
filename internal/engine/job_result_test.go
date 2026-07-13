package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/journal"
)

func TestParseJobResultLegacyChangedFalse(t *testing.T) {
	result, err := parseJobResult([]byte("changed=false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.SchemaVersion != journal.JobResultSchemaVersion || result.Digest == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestParseJobResultAtlasRevisionEvidence(t *testing.T) {
	raw := `{
  "schema_version": "onebox.run/job-result/v1alpha1",
  "changed": true,
  "provider": "atlas",
  "before_revisions": ["202607010001"],
  "after_revisions": ["202607010001", "202607130001"]
}`
	result, err := parseJobResult([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Provider != "atlas" || len(result.AfterRevisions) != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestParseJobResultRejectsRevisionContradiction(t *testing.T) {
	raw := `{"schema_version":"onebox.run/job-result/v1alpha1","changed":false,"provider":"atlas","before_revisions":[],"after_revisions":["202607130001"]}`
	if _, err := parseJobResult([]byte(raw)); err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseJobResultRejectsAtlasHistoryRewriteOrDuplicates(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":"onebox.run/job-result/v1alpha1","changed":true,"provider":"atlas","before_revisions":["r1"],"after_revisions":["r2"]}`,
		`{"schema_version":"onebox.run/job-result/v1alpha1","changed":true,"provider":"atlas","before_revisions":["r1"],"after_revisions":["r1","r1"]}`,
	} {
		if _, err := parseJobResult([]byte(raw)); err == nil {
			t.Fatalf("expected Atlas history rejection for %s", raw)
		}
	}
}

func TestParseJobResultRejectsUnknownOrOversizedContent(t *testing.T) {
	if _, err := parseJobResult([]byte("changed=false\npassword=secret\n")); err == nil {
		t.Fatal("expected unknown key rejection")
	}
	if _, err := parseJobResult(make([]byte, maxJobResultBytes+1)); err == nil {
		t.Fatal("expected size rejection")
	}
	if _, err := parseJobResult(nil); !errors.Is(err, errJobResultMissing) {
		t.Fatalf("empty error = %v", err)
	}
}
