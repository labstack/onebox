package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// basePathCheck is a shell walk, so the honest test runs it against a real
// tree. A canned-answer runner would only restate the exit codes the test
// invented.
type shellRunner struct{}

func (shellRunner) Run(_ context.Context, cmd string) (transport.Result, error) {
	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	result := transport.Result{Stdout: string(out)}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return transport.Result{}, err
		}
		result.ExitCode = exit.ExitCode()
	}
	return result, nil
}

func TestBasePathCheckRefusesADanglingBasePath(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "ob")
	if err := os.Symlink(filepath.Join(root, "gone"), base); err != nil {
		t.Fatal(err)
	}
	check := basePathCheck(context.Background(), shellRunner{}, base)
	if check.OK {
		t.Fatalf("a dangling base path passed preflight: %+v", check)
	}
	if !strings.Contains(check.Detail, "target does not exist") {
		t.Fatalf("dangling base path was not named: %+v", check)
	}
}

func TestBasePathCheckRefusesANonDirectoryBasePath(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "ob")
	if err := os.WriteFile(base, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := basePathCheck(context.Background(), shellRunner{}, base)
	if check.OK || !strings.Contains(check.Detail, "not a directory") {
		t.Fatalf("a file where the base path belongs passed preflight: %+v", check)
	}
}

// A base path that does not exist yet is the fresh-host case and must still
// pass: preflight runs before the mutation that creates it.
func TestBasePathCheckAcceptsAMissingBasePathUnderAWritableParent(t *testing.T) {
	base := filepath.Join(t.TempDir(), "ob", "nested")
	check := basePathCheck(context.Background(), shellRunner{}, base)
	if !check.OK {
		t.Fatalf("a creatable base path was refused: %+v", check)
	}
}
