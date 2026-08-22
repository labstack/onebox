package transport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Staging lands inside the destination's directory, so what keeps it from being
// read as a release is that the entry it adds there is hidden. This fails if
// stagingRoot loses its leading dot.
func TestStagingAddsOnlyAHiddenEntryToTheDestinationsDirectory(t *testing.T) {
	dest := "/var/lib/ob/shop/releases/20260808-120000-abc"
	staging := stagingPath(dest)
	parent := filepath.Dir(dest)

	if !strings.HasPrefix(staging, parent+"/") {
		t.Fatalf("staging %q is no longer under %q; this test guards the wrong property now", staging, parent)
	}
	// `release.list` runs `ls -1` here and reads every entry as a release id.
	// The entry this adds is the first path segment below the releases
	// directory, and `ls -1` omits it only while it starts with a dot.
	entry, _, _ := strings.Cut(strings.TrimPrefix(staging, parent+"/"), "/")
	if !strings.HasPrefix(entry, ".") {
		t.Errorf("staging adds visible entry %q to %s; `ls -1` will list it and it will be read as a release id", entry, parent)
	}
	// And it must not be *under* the destination, which would make a removal of
	// the destination take the payload with it.
	if strings.HasPrefix(staging, dest+"/") {
		t.Errorf("staging %q is a child of the destination", staging)
	}
}

// Staging holds the payload, and for a secrets push that payload is plaintext.
// The three non-release callers clean up the destination they asked for, not the
// staging path — which only uploadScript knows — so a failed transfer that left
// staging behind stranded an app's .env on the host with nothing to reap it.
func TestAFailedUploadLeavesNoStagingBehind(t *testing.T) {
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

	dest := filepath.Join(t.TempDir(), "releases", "20260808-120000-abc")
	if err := NewLocal().Upload(context.Background(), source, dest); err == nil {
		t.Fatal("upload reported success for a source it could not fully read")
	}
	if _, err := os.Stat(stagingPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("a failed transfer left its staging payload at %s: %v", stagingPath(dest), err)
	}
}

// An empty archive is not a malformed one: bsdtar exits 0 on empty stdin, so
// without the sentinel the script ran clean through `mv` and published an empty
// directory as a complete release. This runs the real generated script.
func TestTheRemoteScriptRefusesAnEmptyArchive(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "releases", "20260808-120000-abc")
	script, err := uploadScript(dest, tarTransfer)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.Stdin = strings.NewReader("")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("the script accepted an empty archive: %s", out)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("an empty archive published a destination at %s", dest)
	}
	if _, err := os.Stat(stagingPath(dest)); !os.IsNotExist(err) {
		t.Errorf("an empty archive left staging behind at %s", stagingPath(dest))
	}
}

// The sentinel must not survive into the published release.
func TestAStreamedUploadDoesNotPublishItsSentinel(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "releases", "20260808-120000-abc")
	script, err := uploadScript(dest, tarTransfer)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	body := []byte("services: {}\n")
	if err := tw.WriteHeader(&tar.Header{Name: "compose.yaml", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: uploadSentinel, Mode: 0o600, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.Stdin = &archive
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script rejected a complete archive: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "compose.yaml")); err != nil {
		t.Errorf("the payload was not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, uploadSentinel)); !os.IsNotExist(err) {
		t.Errorf("the sentinel was published into the release: %v", err)
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
// say so rather than replacing it: clearing it first would leave a window with
// the previous release gone and the new one not installed.
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

// A trailing slash would make staging a child of the target, so removing the
// target would destroy the payload too. Cleaning the path first prevents it.
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

// Asserting on the script's text cannot catch a quoting bug, because the
// metacharacters to grep for are ones `path.Dir`/`path.Base` never reproduce.
// Running it can: a destination holding `;` that escapes its quotes executes on
// the target host. app.gAbsPath permits both `;` and spaces in base_path, so
// this is reachable from a project file.
func TestUploadScriptDoesNotExecuteAHostileDestination(t *testing.T) {
	root := t.TempDir()
	canary := filepath.Join(root, "pwned")
	dest := filepath.Join(root, "a b; touch "+canary, "x")
	script, err := uploadScript(dest, func(staging string) string { return "true" })
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.Dir = root
	out, _ := cmd.CombinedOutput()
	if _, err := os.Stat(canary); err == nil {
		t.Fatalf("the destination path executed as a command: %s", out)
	}
	// A space alone must not break the trap either — an unparsable handler
	// installs nothing, so staging would survive a failure.
	if strings.Contains(string(out), "invalid signal") || strings.Contains(string(out), "not found") {
		t.Errorf("the script mis-parsed a quoted path: %s", out)
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
