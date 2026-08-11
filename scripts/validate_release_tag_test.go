package scripts_test

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed validate-release-tag.sh
var validateReleaseTagScript string

func TestValidateReleaseTagAcceptsTagOnMain(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	runGit(t, repo.work, "tag", "v26.8.1", repo.head)

	output, err := runTagValidator(t, repo.work, "v26.8.1", "origin/main")
	if err != nil {
		t.Fatalf("validator rejected a release on main: %v\n%s", err, output)
	}
	if !strings.Contains(output, "validated v26.8.1") {
		t.Fatalf("validator did not report the accepted tag:\n%s", output)
	}
}

func TestValidateReleaseTagRejectsMalformedTags(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	for _, tag := range []string{"v2026.8.1", "v26.08.1", "v26.8.0", "v26.8.1-rc1"} {
		t.Run(tag, func(t *testing.T) {
			output, err := runTagValidator(t, repo.work, tag, "origin/main")
			if err == nil {
				t.Fatalf("validator accepted malformed tag %s:\n%s", tag, output)
			}
			if !strings.Contains(output, "must match vYY.M.SEQUENCE") {
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
	runGit(t, repo.work, "tag", "v26.8.1")

	output, err := runTagValidator(t, repo.work, "v26.8.1", "origin/main")
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
