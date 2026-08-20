package onebox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

// Both probes are shell scripts, so a canned-answer runner would only restate
// the exit codes the test invented. These run them against a real filesystem,
// which is the only way a symlink question gets an honest answer.
func runStateProbe(t *testing.T, script string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), string(out)
		}
		t.Fatalf("run probe: %v", err)
	}
	return 0, string(out)
}

func stateFixtures(t *testing.T) (regular, dangling, absent string) {
	t.Helper()
	dir := t.TempDir()
	regular = filepath.Join(dir, "state.json")
	if err := os.WriteFile(regular, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dangling = filepath.Join(dir, "dangling.json")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	return regular, dangling, filepath.Join(dir, "absent.json")
}

// A dangling link must not read as never-seeded: seeding would then proceed
// against state that exists.
func TestLifecycleStateProbeRefusesUnsearchableAncestor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches every directory, so the permission arm cannot be exercised")
	}
	locked := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(locked, "state.json")
	if err := os.WriteFile(record, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	if exit, _ := runStateProbe(t, lifecycleStateProbe(record)); exit != app.ProbeUndetermined {
		t.Fatalf("exit = %d, want %d", exit, app.ProbeUndetermined)
	}
}

// A dangling link must not read as 'missing': reporting no state at all drops
// the service's protection silently.
func TestLifecycleStateProbeClassifies(t *testing.T) {
	regular, dangling, absent := stateFixtures(t)
	for _, tc := range []struct {
		name   string
		path   string
		marker string
	}{
		{"record is present", regular, "present"},
		{"dangling link is invalid", dangling, "invalid"},
		{"absent is missing", absent, "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit, out := runStateProbe(t, lifecycleStateProbe(tc.path))
			if exit != 0 {
				t.Fatalf("exit = %d, want 0", exit)
			}
			marker, _, _ := strings.Cut(out, "\n")
			if marker != tc.marker {
				t.Fatalf("marker = %q, want %q", marker, tc.marker)
			}
		})
	}
}
