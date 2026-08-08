package transport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Staging must not land in a namespace something else enumerates.
//
// The first version of this staged at `<remoteDir>.partial`, which for a release
// meant inside `releases/` — where every entry is read as a release id. The
// debris was handed to `ob rollback` and counted against retention. This is the
// property that failure was about, and it does not depend on timing.
func TestStagingLandsOutsideTheDestinationsDirectory(t *testing.T) {
	dest := "/var/lib/ob/shop/releases/20260808-120000-abc"
	staging := stagingPath(dest)

	if filepath.Dir(staging) == filepath.Dir(dest) {
		t.Fatalf("staging %q shares a directory with the destination %q", staging, dest)
	}
	if strings.HasPrefix(staging, filepath.Dir(dest)+"/") && !strings.Contains(staging, stagingRoot) {
		t.Errorf("staging %q is inside the releases directory", staging)
	}
	// And it must not be *under* the destination, which would make a removal of
	// the destination take the payload with it.
	if strings.HasPrefix(staging, dest+"/") {
		t.Errorf("staging %q is a child of the destination", staging)
	}
}

func TestStagingIsDistinctPerDestination(t *testing.T) {
	a := stagingPath("/var/lib/ob/shop/releases/r1")
	b := stagingPath("/var/lib/ob/shop/releases/r2")
	if a == b {
		t.Fatal("two destinations share one staging path")
	}
}

// A destination that already exists means something is wrong. The upload must
// say so rather than replacing it — an earlier version removed it first, which
// left a window with the previous release gone and the new one not installed.
func TestUploadRefusesAnExistingDestination(t *testing.T) {
	source := t.TempDir()
	writeFile(t, filepath.Join(source, "compose.yaml"), "new\n")

	dest := filepath.Join(t.TempDir(), "releases", "r1")
	writeFile(t, filepath.Join(dest, "compose.yaml"), "existing\n")

	err := NewLocal().Upload(context.Background(), source, dest)
	if err == nil {
		t.Fatal("upload replaced an existing destination instead of refusing")
	}
	body, readErr := os.ReadFile(filepath.Join(dest, "compose.yaml"))
	if readErr != nil {
		t.Fatalf("the existing destination was damaged: %v", readErr)
	}
	if strings.TrimSpace(string(body)) != "existing" {
		t.Errorf("the existing destination was overwritten: %q", body)
	}
	// And the payload must not have been buried inside it, which is what a bare
	// `mv` into an existing directory would do.
	if _, err := os.Stat(filepath.Join(dest, filepath.Base(dest))); err == nil {
		t.Error("the payload was nested inside the existing destination")
	}
}

// uploadScript exists to be safe, so it validates rather than trusting a caller
// two packages away. None of these is reachable from a project file today.
func TestUploadScriptRefusesDangerousDestinations(t *testing.T) {
	for _, dest := range []string{"", "/", "//", "relative/path", ".", ".."} {
		if _, err := uploadScript(dest, func(s string) string { return "true" }); err == nil {
			t.Errorf("uploadScript accepted %q", dest)
		}
	}
}

// A trailing slash previously made staging a child of the target, so removing
// the target destroyed the payload too. Cleaning first is what prevents it.
func TestUploadScriptCleansItsDestination(t *testing.T) {
	withSlash, err := uploadScript("/var/lib/ob/shop/releases/r1/", func(s string) string { return "true" })
	if err != nil {
		t.Fatal(err)
	}
	without, err := uploadScript("/var/lib/ob/shop/releases/r1", func(s string) string { return "true" })
	if err != nil {
		t.Fatal(err)
	}
	if withSlash != without {
		t.Errorf("a trailing slash changes the script:\n  %s\n  %s", withSlash, without)
	}
}

func TestUploadScriptQuotesItsPaths(t *testing.T) {
	script, err := uploadScript("/var/lib/ob/a b; rm -rf /x", func(s string) string { return "true" })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, shq("/var/lib/ob/a b; rm -rf /x")) {
		t.Errorf("destination is not single-quoted: %s", script)
	}
	// The metacharacters must never appear outside a quoted span.
	for _, unquoted := range []string{"; rm -rf /x &&", "; rm -rf /x |"} {
		if strings.Contains(script, unquoted) {
			t.Errorf("destination escaped its quoting: %s", script)
		}
	}
}

// The destination must appear whole or not at all — never partially populated.
func TestUploadLeavesNoDestinationWhenTheTransferFails(t *testing.T) {
	source := t.TempDir()
	writeFile(t, filepath.Join(source, "compose.yaml"), "services: {}\n")
	secret := filepath.Join(source, "unreadable")
	writeFile(t, secret, "x")
	if os.Geteuid() == 0 {
		t.Skip("running as root; a mode-000 file is still readable")
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })

	dest := filepath.Join(t.TempDir(), "releases", "r1")
	if err := NewLocal().Upload(context.Background(), source, dest); err == nil {
		t.Fatal("upload reported success for a source it could not fully read")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("a failed transfer left a destination behind: %v", err)
	}
}
