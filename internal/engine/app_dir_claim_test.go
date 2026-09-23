package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

// The state directory is a generic name under a basePath the operator chose,
// and destroy removes it whole. Bootstrap must only ever take a directory that
// is new, empty, or already marked as this application's.
func TestClaimAppDirOnlyTakesWhatIsOurs(t *testing.T) {
	run := func(t *testing.T, base string) (int, string) {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "sh", "-c", claimAppDirCommand(app.Names{App: "shop", BasePath: base}, "shop")).Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), string(out)
		} else if err != nil {
			t.Fatal(err)
		}
		return 0, string(out)
	}
	marker := func(base string) string { return filepath.Join(base, "app", app.AppMarkerFile) }

	t.Run("new", func(t *testing.T) {
		base := t.TempDir()
		if code, _ := run(t, base); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if body, err := os.ReadFile(marker(base)); err != nil || strings.TrimSpace(string(body)) != "shop" {
			t.Fatalf("marker = %q, %v", body, err)
		}
		if code, _ := run(t, base); code != 0 {
			t.Fatalf("re-claim of our own directory: exit %d", code)
		}
	})
	t.Run("unmarked", func(t *testing.T) {
		base := t.TempDir()
		if err := os.MkdirAll(filepath.Join(base, "app"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "app", "someone-elses"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _ := run(t, base); code != appDirUnmarked {
			t.Fatalf("unmarked directory: exit %d, want %d", code, appDirUnmarked)
		}
	})
	t.Run("foreign", func(t *testing.T) {
		base := t.TempDir()
		if err := os.MkdirAll(filepath.Join(base, "app"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker(base), []byte("blog\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, out := run(t, base); code != appDirForeign || out != "blog" {
			t.Fatalf("foreign directory: exit %d, owner %q", code, out)
		}
	})
}
