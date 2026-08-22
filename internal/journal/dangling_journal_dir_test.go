package journal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// The journal read is a shell script, so a canned-answer runner would only
// restate the exit codes the test invented. This captures the command the
// production code actually emits, points it at a real temp tree, and runs it —
// the only way a symlink question gets an honest answer.
func TestJournalReadRefusesDanglingDirectory(t *testing.T) {
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}

	journalDir := dir(names)
	if err := os.MkdirAll(filepath.Dir(journalDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "gone"), journalDir); err != nil {
		t.Fatal(err)
	}

	fake := &transport.Fake{}
	if _, _, err := Journals(context.Background(), fake, names); err != nil {
		t.Fatalf("capture command: %v", err)
	}
	if len(fake.Commands) == 0 {
		t.Fatal("no command was issued")
	}

	// A dangling journal directory must not read as the never-deployed
	// state: reporting it as "no journals" is the false completeness the
	// exit-2 arm exists to prevent.
	if exit := shellExit(t, fake.Commands[len(fake.Commands)-1]); exit != 2 {
		t.Fatalf("dangling journal directory exited %d, want 2", exit)
	}
}

// The directory guard has a twin one level down: skipping a dangling entry
// file drops that deploy's records, and FindIncomplete then reports nothing
// incomplete — the same false completeness.
func TestJournalReadRefusesDanglingEntry(t *testing.T) {
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}
	if err := os.MkdirAll(dir(names), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "gone"), filepath.Join(dir(names), "R1.jsonl")); err != nil {
		t.Fatal(err)
	}
	fake := &transport.Fake{}
	if _, _, err := Journals(context.Background(), fake, names); err != nil {
		t.Fatalf("capture command: %v", err)
	}
	if exit := shellExit(t, fake.Commands[len(fake.Commands)-1]); exit != 2 {
		t.Fatalf("dangling journal entry exited %d, want 2", exit)
	}
}

// An unsearchable ancestor hides the journal directory as thoroughly as a
// missing one. Answering "never deployed" there strands an interrupted deploy:
// FindIncomplete reports nothing to resume.
func TestJournalReadRefusesUnsearchableAncestor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches every directory, so the permission arm cannot be exercised")
	}
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}
	journalDir := dir(names)
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journalDir, "R1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Dir(journalDir)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	fake := &transport.Fake{}
	if _, _, err := Journals(context.Background(), fake, names); err != nil {
		t.Fatalf("capture command: %v", err)
	}
	if exit := shellExit(t, fake.Commands[len(fake.Commands)-1]); exit != app.ProbeUndetermined {
		t.Fatalf("unsearchable ancestor exited %d, want %d", exit, app.ProbeUndetermined)
	}
}

// Searchable but not readable: cd succeeds, the glob cannot enumerate, and the
// loop never runs — so the read looks like a never-deployed host while an
// interrupted deploy sits in the directory.
func TestJournalReadRefusesUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory, so the permission arm cannot be exercised")
	}
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}
	journalDir := dir(names)
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journalDir, "R1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 0111: searchable, not readable.
	if err := os.Chmod(journalDir, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(journalDir, 0o700) })

	fake := &transport.Fake{}
	if _, _, err := Journals(context.Background(), fake, names); err != nil {
		t.Fatalf("capture command: %v", err)
	}
	if exit := shellExit(t, fake.Commands[len(fake.Commands)-1]); exit != 2 {
		t.Fatalf("unreadable journal directory exited %d, want 2", exit)
	}
}

// A dangling link is not the only way an entry stops being a journal: a
// directory, fifo or device sitting at <id>.jsonl is skipped just as silently,
// and that deploy disappears from the read.
func TestJournalReadRefusesNonRegularEntry(t *testing.T) {
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}
	if err := os.MkdirAll(dir(names), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where a journal belongs.
	if err := os.Mkdir(filepath.Join(dir(names), "R1.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &transport.Fake{}
	if _, _, err := Journals(context.Background(), fake, names); err != nil {
		t.Fatalf("capture command: %v", err)
	}
	if exit := shellExit(t, fake.Commands[len(fake.Commands)-1]); exit != 2 {
		t.Fatalf("non-regular journal entry exited %d, want 2", exit)
	}
}

// A real directory still reads clean, so the guard above did not turn the
// never-deployed state into a failure.
func TestJournalReadAcceptsRealDirectory(t *testing.T) {
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}
	if err := os.MkdirAll(dir(names), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &transport.Fake{}
	if _, _, err := Journals(context.Background(), fake, names); err != nil {
		t.Fatalf("capture command: %v", err)
	}
	if exit := shellExit(t, fake.Commands[len(fake.Commands)-1]); exit != 0 {
		t.Fatalf("real journal directory exited %d, want 0", exit)
	}
}

func shellExit(t *testing.T, cmd string) int {
	t.Helper()
	run := exec.CommandContext(t.Context(), "/bin/sh", "-c", cmd)
	if err := run.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		t.Fatalf("run command: %v", err)
	}
	return 0
}
