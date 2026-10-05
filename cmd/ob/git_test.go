package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRevisionMarksUntrackedPayloadDirty(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("-c", "user.name=Onebox Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	want := git("rev-parse", "--short=7", "HEAD") + "+dirty"
	if err := os.WriteFile(filepath.Join(dir, "payload.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gitShortSHA(t.Context(), dir); got != want {
		t.Fatalf("CLI revision = %q, want %q", got, want)
	}
}
