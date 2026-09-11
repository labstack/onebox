package release

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestActiveScheduleLeasesDistinguishesContentionFromFlockFailure(t *testing.T) {
	root := t.TempDir()
	names := app.Names{App: "sample", BasePath: root}
	releaseID := "20260911-120000-abcd"
	releaseDir := filepath.Join(PathsFor(names).Releases, releaseID)
	if err := os.MkdirAll(releaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, scheduleLeaseFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var command string
	fake := &transport.Fake{Dynamic: func(candidate string) (transport.Result, bool) {
		command = candidate
		return transport.Result{}, true
	}}
	if _, err := ActiveScheduleLeases(context.Background(), fake, names); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "--conflict-exit-code 200") || !strings.Contains(command, "200) dir=") {
		t.Fatalf("lease probe does not use its distinct conflict sentinel: %s", command)
	}

	flock := filepath.Join(root, "flock")
	run := func(exitCode string) ([]byte, error) {
		t.Helper()
		if err := os.WriteFile(flock, []byte("#!/bin/sh\nexit "+exitCode+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		probe := strings.ReplaceAll(command, "/usr/bin/flock", q(flock))
		return exec.CommandContext(t.Context(), "sh", "-c", probe).CombinedOutput()
	}

	if output, err := run("74"); err == nil {
		t.Fatalf("flock infrastructure failure was accepted: output=%q", output)
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 74 {
			t.Fatalf("flock infrastructure failure = %v, want exit 74; output=%q", err, output)
		}
	}
	if output, err := run("200"); err != nil || strings.TrimSpace(string(output)) != releaseID {
		t.Fatalf("lease contention was not reported as active: err=%v output=%q", err, output)
	}
}
