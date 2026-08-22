package scripts_test

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed require-newest-release.sh
var requireNewestReleaseScript string

// The guard exists so a re-run of an old tag cannot downgrade Homebrew and
// Scoop, and it must reach that verdict from the published release alone: local
// tags would reject a queued release that is still perfectly valid, and a
// malformed high tag would poison any selector reading them.
func TestRequireNewestRelease(t *testing.T) {
	for _, test := range []struct {
		name      string
		published string
		tag       string
		apiFails  bool
		wantErr   string
	}{
		{name: "first release ever", published: "", tag: "v2026.8.0"},
		{name: "newer revision", published: "v2026.8.0", tag: "v2026.8.1"},
		{name: "revision past nine", published: "v2026.8.9", tag: "v2026.8.10"},
		{name: "month past nine", published: "v2026.9.0", tag: "v2026.11.0"},
		{name: "next year", published: "v2026.12.3", tag: "v2027.1.0"},
		{name: "behind by a revision", published: "v2026.8.1", tag: "v2026.8.0", wantErr: "is behind the published v2026.8.1"},
		{name: "behind by a month", published: "v2026.11.0", tag: "v2026.9.0", wantErr: "is behind the published v2026.11.0"},
		{name: "already published", published: "v2026.8.0", tag: "v2026.8.0", wantErr: "is already published"},
		// An outage is not evidence that nothing is published. A guard that
		// reads one as "first release" disables itself precisely when it
		// cannot see what it protects.
		{name: "api failure", published: "", apiFails: true, tag: "v2026.8.0", wantErr: "cannot read the published releases"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := runNewestReleaseGuard(t, test.tag, test.published, test.apiFails)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("guard rejected %s after %s: %v\n%s", test.tag, test.published, err, output)
			case test.wantErr != "" && err == nil:
				t.Fatalf("guard accepted %s after %s:\n%s", test.tag, test.published, output)
			case test.wantErr != "" && !strings.Contains(output, test.wantErr):
				t.Fatalf("guard did not explain the refusal:\n%s", output)
			}
		})
	}
}

// runNewestReleaseGuard stands in a `gh` that answers with one published tag, so
// the test drives the real script rather than a transcription of it.
func runNewestReleaseGuard(t *testing.T, tag, published string, apiFails bool) (string, error) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "require-newest-release.sh")
	if err := os.WriteFile(script, []byte(requireNewestReleaseScript), 0o700); err != nil {
		t.Fatal(err)
	}
	stub := "#!/usr/bin/env bash\n"
	switch {
	case apiFails:
		stub += "echo 'HTTP 401: Bad credentials' >&2\nexit 1\n"
	case published == "":
		stub += "printf '\\n'\n" // an empty release list prints an empty line
	default:
		stub += "printf '%s\\n' " + published + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", script, tag, "labstack/onebox")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	return string(output), err
}
