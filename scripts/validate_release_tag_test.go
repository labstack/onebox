package scripts_test

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/buildinfo"
)

//go:embed validate-release-tag.sh
var validateReleaseTagScript string

func TestValidateReleaseTagAcceptsTagOnMain(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	runGit(t, repo.work, "tag", "v2026.8.0", repo.head)

	output, err := runTagValidator(t, repo.work, "v2026.8.0", "origin/main")
	if err != nil {
		t.Fatalf("validator rejected a release on main: %v\n%s", err, output)
	}
	if !strings.Contains(output, "validated v2026.8.0") {
		t.Fatalf("validator did not report the accepted tag:\n%s", output)
	}
}

func TestValidateReleaseTagRejectsMalformedTags(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	for _, tag := range []string{"v26.8.0", "v2026.08.0", "v2026.8.00", "v2026.8.0-rc1"} {
		t.Run(tag, func(t *testing.T) {
			output, err := runTagValidator(t, repo.work, tag, "origin/main")
			if err == nil {
				t.Fatalf("validator accepted malformed tag %s:\n%s", tag, output)
			}
			if !strings.Contains(output, "must match vYYYY.M.REVISION") {
				t.Fatalf("validator did not explain the tag grammar:\n%s", output)
			}
		})
	}
}

func TestValidateReleaseTagRejectsCommitOffMain(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	runGit(t, repo.work, "checkout", "-b", "release-candidate")
	runGit(t, repo.work, "commit", "--allow-empty", "-m", "off-main release")
	runGit(t, repo.work, "tag", "v2026.8.0")

	output, err := runTagValidator(t, repo.work, "v2026.8.0", "origin/main")
	if err == nil {
		t.Fatalf("validator accepted a release commit off main:\n%s", output)
	}
	if !strings.Contains(output, "is not reachable from origin/main") {
		t.Fatalf("validator did not report the lineage refusal:\n%s", output)
	}
}

func runTagValidator(t *testing.T, dir, tag, mainRef string) (string, error) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "validate-release-tag.sh")
	if err := os.WriteFile(script, []byte(validateReleaseTagScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, tag, mainRef)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// The release grammar is written twice: once in Bash, where it gates the tag a
// human pushes, and once in Go, where it gates `min_version` and the
// runner's own provenance. Drift is silent in both directions — a looser shell
// publishes a tag the binary cannot parse, a looser loader accepts a minimum no
// tag can satisfy — so one corpus decides both.
func TestTagValidatorAndParserAgreeOnTheGrammar(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	for _, candidate := range []struct {
		tag   string
		valid bool
	}{
		{tag: "v2026.1.0", valid: true},
		{tag: "v2026.10.0", valid: true},
		{tag: "v2026.12.99", valid: true},
		{tag: "v2026.8.10", valid: true},
		{tag: "v2026.8.9999999999999999999", valid: true},
		// One digit wider than any uint64: the shell grammar alone would have
		// published a tag the runner cannot parse into its own provenance.
		{tag: "v2026.8.18446744073709551616"},
		{tag: "v2026.0.0"},
		{tag: "v2026.13.0"},
		{tag: "v2026.08.0"},
		{tag: "v2026.8.00"},
		{tag: "v2026.8.01"},
		{tag: "v26.8.0"},
		{tag: "v02026.8.0"},
		{tag: "v2026.8.0-rc1"},
		{tag: "2026.8.0"},
	} {
		t.Run(candidate.tag, func(t *testing.T) {
			runGit(t, repo.work, "tag", "--force", candidate.tag, repo.head)
			_, shellErr := runTagValidator(t, repo.work, candidate.tag, "origin/main")
			_, parseErr := buildinfo.ParseReleaseVersion(candidate.tag)
			if (shellErr == nil) != (parseErr == nil) {
				t.Fatalf("shell and Go disagree on %s: shell=%v go=%v", candidate.tag, shellErr, parseErr)
			}
			if (parseErr == nil) != candidate.valid {
				t.Fatalf("%s: valid=%v, want %v", candidate.tag, parseErr == nil, candidate.valid)
			}
		})
	}
}
