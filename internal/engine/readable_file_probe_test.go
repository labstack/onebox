package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The probe is a shell script, so a canned-answer runner would only restate
// the exit codes the test invented. This runs it against a real filesystem,
// which is the only way a symlink question gets an honest answer.
func TestReadableFileProbeClassifies(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "config.hash")
	if err := os.WriteFile(regular, []byte("sha256:abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.hash")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, "linked.hash")
	if err := os.Symlink(regular, linked); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		exit int
		out  string
	}{
		{"contents are read", regular, 0, "sha256:abc"},
		// A live link still reads through: status reporting is not the
		// place to introduce a refusal the mutations do not make.
		{"live link reads through", linked, 0, "sha256:abc"},
		// A dangling link satisfies neither -r nor -e. Without the -L arm
		// it scores as absent, and status calls the host un-applied while
		// reporting drift against a file it never read.
		{"dangling link is unreadable, not absent", dangling, 2, ""},
		{"absent is absent", filepath.Join(dir, "gone.hash"), 0, ""},
	}
	if os.Geteuid() != 0 {
		// An unsearchable directory makes -r, -e and -L all false, and
		// scoring that as absent has status report drift against a file
		// it never read.
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "config.hash"), []byte("sha256:xyz\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		cases = append(cases, struct {
			name string
			path string
			exit int
			out  string
		}{"unsearchable is not absent", filepath.Join(locked, "config.hash"), 5, ""})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", readableFileProbe(tc.path))
			out, err := cmd.Output()
			exit := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("run probe: %v", err)
				}
				exit = ee.ExitCode()
			}
			if exit != tc.exit {
				t.Fatalf("exit = %d, want %d", exit, tc.exit)
			}
			if strings.TrimSpace(string(out)) != tc.out {
				t.Fatalf("stdout = %q, want %q", out, tc.out)
			}
		})
	}
}
