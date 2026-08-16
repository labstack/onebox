package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The plan is what `ob approve` and `ob deploy --plan` consume, and the report
// template is what a CI step reads as "this deploy needs a backup". Whichever
// of the two is written first is left orphaned when the other fails, and on a
// re-plan the paths already exist, so removing them afterwards cannot restore
// the caller's previous files. Staging both and committing together is the only
// ordering where a failed run leaves the tree as it was.
//
// A source assertion: driving a real plan needs a resolvable project and a
// reachable target, and what has to hold is the write discipline itself.
func TestPlanArtifactsAreStagedAndCommittedTogether(t *testing.T) {
	for source, function := range map[string]string{
		"commands.go": "func runPlan(",
		"job.go":      "func runJobPlan(",
	} {
		body, err := os.ReadFile(filepath.Join(".", source))
		if err != nil {
			t.Fatal(err)
		}
		// Scope to the planning function: sibling commands write a single
		// artifact and have nothing to stage it against.
		start := strings.Index(string(body), function)
		if start < 0 {
			t.Fatalf("%s: %s not found", source, function)
		}
		text := string(body)[start:]
		if end := strings.Index(text[1:], "\nfunc "); end >= 0 {
			text = text[:end+1]
		}

		// Neither artifact may be written straight to its destination.
		for _, direct := range []string{
			".Save(outPath)",
			".SaveTemplate(backupReportOut)",
		} {
			if strings.Contains(text, direct) {
				t.Errorf("%s: writes %s directly; a later failure then leaves it behind", source, direct)
			}
		}
		for _, staged := range []string{
			"stageArtifact(outPath,",
			"stageArtifact(backupReportOut,",
			"commitArtifactSet(",
		} {
			if !strings.Contains(text, staged) {
				t.Errorf("%s: missing %s", source, staged)
			}
		}
		// Committing one at a time is what leaves a half-set behind when the
		// second rename fails; the set commit owns rollback and cleanup.
		if strings.Contains(text, ".commit()") {
			t.Errorf("%s: commits an artifact individually instead of as a set", source)
		}
		// A shared destination is refused outright: staging gives the two
		// distinct temp names, so the set would commit and the plan — renamed
		// second — would silently win.
		if !strings.Contains(text, "sameArtifactPath(outPath, backupReportOut)") {
			t.Errorf("%s: does not refuse --out and --backup-report-out naming one path", source)
		}
		// Validation before staging, so an invalid plan is not reported as a
		// write that never happened.
		if !strings.Contains(text, ".Validate(); err != nil") {
			t.Errorf("%s: stages before validating; an invalid plan would report artifact_write_failed", source)
		}
		// Distinct staging paths: two artifacts sharing a destination would
		// otherwise stage to one path and silently take each other's content.
		if strings.Contains(text, `stageArtifact(outPath, ".plan"`) == strings.Contains(text, `stageArtifact(backupReportOut, ".plan"`) {
			t.Errorf("%s: staged artifacts do not use distinct suffixes", source)
		}
		// The cleanup that used to paper over the ordering is gone; if it
		// comes back, staging is what should be fixed instead.
		if strings.Contains(text, "removeArtifactWeCreated") {
			t.Errorf("%s: reintroduces artifact cleanup; staging is the guarantee, not removal after the fact", source)
		}
	}
}
