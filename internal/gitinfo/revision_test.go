package gitinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRevisionReportsSubmoduleChangesButExcludesIgnoredFiles(t *testing.T) {
	for _, change := range []string{"clean", "ignored", "modified", "untracked", "new commit"} {
		t.Run(change, func(t *testing.T) {
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(dir, name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			commit := func(dir string) {
				t.Helper()
				git(dir, "add", ".")
				git(dir, "-c", "user.name=Onebox Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
			}
			source, root := t.TempDir(), t.TempDir()
			git(source, "init")
			write(source, "payload.txt", "committed\n")
			write(source, ".gitignore", "cache.txt\n")
			commit(source)
			git(root, "init")
			git(root, "-c", "protocol.file.allow=always", "submodule", "add", source, "module")
			commit(root)
			want := git(root, "rev-parse", "--short=7", "HEAD")
			// Local preferences must not hide real submodule changes.
			git(root, "config", "submodule.module.ignore", "all")
			git(root, "config", "status.showUntrackedFiles", "no")
			module := filepath.Join(root, "module")
			switch change {
			case "ignored":
				write(module, "cache.txt", "ignored\n")
			case "modified", "new commit":
				write(module, "payload.txt", "changed\n")
				if change == "new commit" {
					commit(module)
				}
			case "untracked":
				write(module, "new-payload.txt", "untracked\n")
			}
			if change != "clean" && change != "ignored" {
				want += "+dirty"
			}
			if got := Revision(t.Context(), root); got != want {
				t.Fatalf("revision = %q, want %q", got, want)
			}
		})
	}
}
