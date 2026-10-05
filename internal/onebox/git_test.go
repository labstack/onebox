package onebox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitShortSHAReportsWorkingTreeChanges(t *testing.T) {
	for _, change := range []string{"clean", "ignored", "modified", "staged", "untracked", "deleted", "index unavailable"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			git("init")
			write("payload.txt", "committed payload\n")
			git("add", "payload.txt")
			git("-c", "user.name=Onebox Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
			want := git("rev-parse", "--short=7", "HEAD")
			switch change {
			case "ignored":
				write(".git/info/exclude", "cache.txt\n")
				write("cache.txt", "ignored cache\n")
			case "modified", "staged":
				write("payload.txt", "uncommitted payload\n")
				if change == "staged" {
					git("add", "payload.txt")
				}
			case "untracked":
				// An operator's preference must not hide payload changes.
				git("config", "status.showUntrackedFiles", "no")
				write("new-payload.txt", "untracked payload\n")
			case "deleted":
				if err := os.Remove(filepath.Join(dir, "payload.txt")); err != nil {
					t.Fatal(err)
				}
			case "index unavailable":
				write(".git/index", "corrupted index")
			}
			if change == "index unavailable" {
				want = ""
			} else if change != "clean" && change != "ignored" {
				want += "+dirty"
			}
			if got := gitShortSHA(t.Context(), dir); got != want {
				t.Errorf("revision = %q, want %q", got, want)
			}
		})
	}
}

func TestGitShortSHAOmitsUnavailableRevision(t *testing.T) {
	if got := gitShortSHA(t.Context(), t.TempDir()); got != "" {
		t.Fatalf("non-repository revision = %q, want empty", got)
	}
	unborn := t.TempDir()
	if out, err := exec.CommandContext(t.Context(), "git", "-C", unborn, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if got := gitShortSHA(t.Context(), unborn); got != "" {
		t.Fatalf("unborn repository revision = %q, want empty", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := gitShortSHA(ctx, "."); got != "" {
		t.Fatalf("cancelled revision = %q, want empty", got)
	}
}
