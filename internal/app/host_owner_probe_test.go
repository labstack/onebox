package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The owner probe is a shell script, so asserting on it with a canned-answer
// runner would only restate the exit codes the test itself invented. These run
// the real script against a real filesystem, which is the only way a symlink
// question gets an honest answer.
func runOwnerProbe(t *testing.T, path string) (int, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", HostOwnerProbe(path))
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

func TestHostOwnerProbeClassifiesTheRecord(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "owner")
	if err := os.WriteFile(regular, []byte("ledger\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), dangling); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, "linked")
	if err := os.Symlink(regular, linked); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		exit int
		out  string
	}{
		{"missing is unclaimed", filepath.Join(dir, "absent"), 3, ""},
		{"regular file is read", regular, 0, "ledger"},
		// A dangling symlink fails -e, so an -e-first probe calls it
		// unclaimed and preflight promises a claim that bootstrap cannot
		// make: the noclobber write refuses any symlink, dangling included.
		{"dangling symlink is refused", dangling, 4, ""},
		{"symlink to a record is refused", linked, 4, ""},
		{"directory is refused", subdir, 4, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exit, out := runOwnerProbe(t, tc.path)
			if exit != tc.exit {
				t.Fatalf("exit = %d, want %d", exit, tc.exit)
			}
			if strings.TrimSpace(out) != tc.out {
				t.Fatalf("stdout = %q, want %q", out, tc.out)
			}
		})
	}
}

// Absence that could not be established is not absence: when the state
// directory cannot be searched, both -e and -L are false for a record that may
// well exist, and calling that unclaimed is how one application adopts a host
// that already has an owner.
func TestHostOwnerProbeSeparatesUnsearchableFromAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches every directory, so the permission arm cannot be exercised")
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "_host")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(state, "owner")
	if err := os.WriteFile(record, []byte("ledger\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })

	if exit, _ := runOwnerProbe(t, record); exit != ProbeUndetermined {
		t.Fatalf("exit = %d, want %d (undetermined)", exit, ProbeUndetermined)
	}
}

// An unsearchable grandparent hides the parent too, so checking only the
// record's immediate parent would find it "absent" and answer unclaimed for
// the same wrong reason.
func TestHostOwnerProbeWalksPastAnUnsearchableGrandparent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches every directory, so the permission arm cannot be exercised")
	}
	root := t.TempDir()
	grand := filepath.Join(root, "ob")
	state := filepath.Join(grand, "_host")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(state, "owner")
	if err := os.WriteFile(record, []byte("ledger\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(grand, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(grand, 0o700) })

	if exit, _ := runOwnerProbe(t, record); exit != ProbeUndetermined {
		t.Fatalf("exit = %d, want %d (undetermined)", exit, ProbeUndetermined)
	}
}

// A plain file where the state directory belongs is a broken host, not a
// permission problem. Reporting it as undetermined hands the operator a remedy
// ("verify access") that no permission change can satisfy.
func TestHostOwnerProbeNamesANonDirectoryStatePath(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "_host")
	if err := os.WriteFile(state, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not ProbeNotRegular: that code is a statement about the record, and
	// the record is not what is wrong here.
	if exit, _ := runOwnerProbe(t, filepath.Join(state, "owner")); exit != ProbeStatePathNotDirectory {
		t.Fatalf("exit = %d, want %d (state path not a directory)", exit, ProbeStatePathNotDirectory)
	}
}

// Searchability is tested by entering the directory, not by reading its
// permission bits: a mode-0000 directory has no execute bit but root can still
// search it, and `test -x` would call that undetermined for the one account
// that can in fact look.
func TestHostOwnerProbeAsksTheFilesystemNotThePermissionBits(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "_host")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })

	want := ProbeUndetermined
	if os.Geteuid() == 0 {
		// Root can enter it, so absence really is established.
		want = ProbeAbsent
	}
	if exit, _ := runOwnerProbe(t, filepath.Join(state, "owner")); exit != want {
		t.Fatalf("exit = %d, want %d", exit, want)
	}
}

// The record is mode 0600, so "present, regular, and not ours to read" is its
// most likely refusal. Falling through to cat would exit 1, which callers that
// render a report treat as a failed command and pay for with the whole report.
func TestHostOwnerProbeNamesAnUnreadableRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file, so the permission arm cannot be exercised")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "owner")
	if err := os.WriteFile(record, []byte("ledger\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(record, 0o600) })

	if exit, _ := runOwnerProbe(t, record); exit != ProbeUnreadable {
		t.Fatalf("exit = %d, want %d (unreadable)", exit, ProbeUnreadable)
	}
}

// A host with no state directory at all is the fresh-install case, and must
// stay unclaimed — the directory arm only speaks when the directory is there.
func TestHostOwnerProbeTreatsMissingStateDirAsUnclaimed(t *testing.T) {
	record := filepath.Join(t.TempDir(), "_host", "owner")
	if exit, _ := runOwnerProbe(t, record); exit != ProbeAbsent {
		t.Fatalf("exit = %d, want %d (absent)", exit, ProbeAbsent)
	}
}

// The probe quotes its path, so a record under an awkward directory name is
// still classified rather than word-split into a different question.
func TestHostOwnerProbeQuotesThePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a dir; touch pwned")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "owner")
	if err := os.WriteFile(path, []byte("ledger\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, out := runOwnerProbe(t, path)
	if exit != 0 || strings.TrimSpace(out) != "ledger" {
		t.Fatalf("exit = %d, stdout = %q; want 0 and %q", exit, out, "ledger")
	}
}
