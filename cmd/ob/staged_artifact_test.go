package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeString(body string) func(string) error {
	return func(path string) error { return os.WriteFile(path, []byte(body), 0o600) }
}

// A caller told the run failed must not find a fresh, complete-looking artifact
// waiting for it. A rename cannot be undone, so the set is made absent rather
// than left half-written.
func TestCommitArtifactSetRemovesWhatAlreadyLandedOnFailure(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	plan := filepath.Join(dir, "plan.json")

	stagedReport, err := stageArtifact(report, ".report", writeString("report"))
	if err != nil {
		t.Fatal(err)
	}
	stagedPlan, err := stageArtifact(plan, ".plan", writeString("plan"))
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the plan's destination makes its rename fail.
	if err := os.Mkdir(plan, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := commitArtifactSet(stagedReport, stagedPlan); err == nil {
		t.Fatal("commit reported success with a destination it could not write")
	}
	if _, err := os.Stat(report); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the report survived a failed set commit: %v", err)
	}
	// The real staged names, not derived ones: stageArtifact reserves a unique
	// path, so a derived string would assert about a file that never existed.
	for _, staged := range []string{stagedReport.staged, stagedPlan.staged} {
		if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("staged file %s was left behind", staged)
		}
	}
}

// Distinct suffixes keep the staged writes apart. Note the commands refuse a
// shared destination outright (see runPlan/runJobPlan) — this only pins that
// staging itself does not conflate them, so a future caller that does allow it
// gets two independent files rather than one silently overwritten.
func TestStagedArtifactsDoNotCollideOnASharedDestination(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "same.json")
	stagedReport, err := stageArtifact(shared, ".report", writeString("report"))
	if err != nil {
		t.Fatal(err)
	}
	stagedPlan, err := stageArtifact(shared, ".plan", writeString("plan"))
	if err != nil {
		t.Fatal(err)
	}
	if stagedReport.staged == stagedPlan.staged {
		t.Fatalf("both artifacts staged to %s", stagedReport.staged)
	}
	if err := commitArtifactSet(stagedReport, stagedPlan); err != nil {
		t.Fatalf("commit: %v", err)
	}
	body, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "plan" {
		t.Fatalf("shared destination holds %q; the later artifact must win, not a mixture", body)
	}
}

func TestCommitArtifactSetLeavesNoTempFilesOnSuccess(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	// Pre-existing, so the commit takes a backup: a re-plan is the common
	// case, and a backup that outlives it leaves the previous run's complete,
	// approvable artifact sitting beside the new one forever.
	if err := os.WriteFile(plan, []byte("previous plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := stageArtifact(plan, ".plan", writeString("plan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := commitArtifactSet(staged); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "plan.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory holds %v, want only plan.json", names)
	}
	if body, err := os.ReadFile(plan); err != nil || string(body) != "plan" {
		t.Fatalf("destination = %q %v, want the new plan", body, err)
	}
}

// A commit that fails for any reason leaves nothing behind — here the rename
// itself fails because the destination directory does not exist.
//
// Note this does NOT reach the directory-sync rollback: the rename fails first.
// Making a directory fsync fail portably is not something a unit test can
// arrange, so that path is covered by inspection only — the sync error rolls
// the committed set back rather than being returned with the renames standing.
func TestCommitArtifactSetLeavesNothingWhenARenameFails(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	staged, err := stageArtifact(plan, ".plan", writeString("plan"))
	if err != nil {
		t.Fatal(err)
	}
	// Point the artifact at a directory that will not open for sync, after
	// staging so the write itself succeeded.
	staged.final = filepath.Join(dir, "gone", "plan.json")

	if err := commitArtifactSet(staged); err == nil {
		t.Fatal("commit reported success with an unsyncable destination")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("a failed commit left %s behind", entry.Name())
	}
}

// A partial commit must put the tree back, not merely stop. Rolling forward
// leaves one destination new and another stale; rolling back without restoring
// deletes a file the failed run was never asked to touch. Both are half-sets.
func TestCommitArtifactSetRestoresReplacedFilesOnFailure(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	plan := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(report, []byte("old report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan, []byte("old plan"), 0o600); err != nil {
		t.Fatal(err)
	}

	stagedReport, err := stageArtifact(report, ".report", writeString("new report"))
	if err != nil {
		t.Fatal(err)
	}
	stagedPlan, err := stageArtifact(plan, ".plan", writeString("new plan"))
	if err != nil {
		t.Fatal(err)
	}
	// Make the plan's commit fail after the report's has landed. Remove the
	// file stageArtifact actually wrote first, so the only thing left in the
	// directory afterwards is what commitArtifactSet is responsible for.
	if err := os.Remove(stagedPlan.staged); err != nil {
		t.Fatal(err)
	}
	stagedPlan.staged = filepath.Join(dir, "never-written")

	if err := commitArtifactSet(stagedReport, stagedPlan); err == nil {
		t.Fatal("commit reported success with a missing staged file")
	}
	for path, want := range map[string]string{report: "old report", plan: "old plan"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s was not restored: %v", filepath.Base(path), err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s = %q, want the previous run's %q", filepath.Base(path), body, want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("directory holds %v, want only the two restored files", entries)
	}
}

// A destination that is not a regular file must be refused outright: backing it
// up and then discarding the backup would delete it.
func TestCommitArtifactSetRefusesANonRegularDestination(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	staged, err := stageArtifact(plan, ".plan", writeString("plan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(plan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := commitArtifactSet(staged); err == nil {
		t.Fatal("commit replaced a directory")
	}
	if info, err := os.Stat(plan); err != nil || !info.IsDir() {
		t.Fatalf("the directory at the destination did not survive: %v", err)
	}
}

// A raw string comparison lets `plan.json` and `./plan.json` through, and the
// two then commit onto one path with the second silently winning — the run
// reporting a backup_report_path that holds a deploy plan.
func TestSameArtifactPathSeesThroughEquivalentSpellings(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	for _, spelling := range []string{
		plan,
		"./" + filepath.Base(plan),
		filepath.Join(dir, "sub", "..", "plan.json"),
	} {
		against := plan
		if spelling == "./"+filepath.Base(plan) {
			// Relative spellings resolve against the process's directory, so
			// compare two relative forms of the same name.
			against = filepath.Base(plan)
		}
		if !sameArtifactPath(spelling, against) {
			t.Errorf("sameArtifactPath(%q, %q) = false, want true", spelling, against)
		}
	}
	if sameArtifactPath(plan, filepath.Join(dir, "report.json")) {
		t.Error("two genuinely different paths compared equal")
	}
}

// A restore that fails leaves the caller's previous file in the backup, which
// is then the only copy: rollback has already removed the destination.
// Deleting it on the way out would leave neither the new artifact nor the one
// it replaced — the opposite of the guarantee.
func TestDiscardKeepsTheBackupWhenTheRestoreFailed(t *testing.T) {
	dir := t.TempDir()
	backup := filepath.Join(dir, "plan.json.ob-bak.plan")
	if err := os.WriteFile(backup, []byte("previous plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := stagedArtifact{
		staged: filepath.Join(dir, "plan.json.ob-tmp.plan"),
		// A destination whose directory does not exist, so restore's rename
		// cannot succeed.
		final:    filepath.Join(dir, "gone", "plan.json"),
		backup:   backup,
		replaced: true,
	}
	artifact.rollback()
	artifact.discard()

	body, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("the only surviving copy was deleted: %v", err)
	}
	if string(body) != "previous plan" {
		t.Fatalf("backup = %q, want the previous plan", body)
	}
}

// When the restore succeeds there is nothing left to protect, so the backup
// goes with everything else.
func TestDiscardRemovesTheBackupWhenTheRestoreSucceeded(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "plan.json")
	backup := filepath.Join(dir, "plan.json.ob-bak.plan")
	if err := os.WriteFile(backup, []byte("previous plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := stagedArtifact{
		staged:   filepath.Join(dir, "plan.json.ob-tmp.plan"),
		final:    final,
		backup:   backup,
		replaced: true,
	}
	artifact.rollback()
	artifact.discard()

	if body, err := os.ReadFile(final); err != nil || string(body) != "previous plan" {
		t.Fatalf("the previous plan was not restored: %q %v", body, err)
	}
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the backup outlived a successful restore: %v", err)
	}
}

// A symlinked output directory otherwise defeats the collision guard entirely:
// both artifacts commit onto one file and the plan, renamed second, silently
// wins while the run reports a backup_report_path holding a deploy plan.
func TestSameArtifactPathSeesThroughASymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "out")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameArtifactPath(filepath.Join(real, "plan.json"), filepath.Join(link, "plan.json")) {
		t.Error("a symlinked directory defeated the collision guard")
	}
	if sameArtifactPath(filepath.Join(real, "plan.json"), filepath.Join(link, "report.json")) {
		t.Error("two different names in the same directory compared equal")
	}
}

// A run killed between commit and settle leaves a backup beside a destination
// that still exists. That copy is redundant, so staging clears it and the leak
// is bounded to one crashed run.
func TestStageArtifactClearsARedundantBackup(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(plan, []byte("current plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := plan + ".ob-bak.plan"
	if err := os.WriteFile(stale, []byte("previous run's plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageArtifact(plan, ".plan", writeString("plan")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a redundant backup survived staging: %v", err)
	}
}

// The other kill window is between the two renames inside commit(): the
// destination is already gone and the backup is the caller's ONLY copy.
// Clearing it there destroys the data the staging machinery exists to protect.
func TestStageArtifactKeepsABackupThatIsTheOnlyCopy(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	orphan := plan + ".ob-bak.plan"
	if err := os.WriteFile(orphan, []byte("the only copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageArtifact(plan, ".plan", writeString("plan")); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(orphan)
	if err != nil {
		t.Fatalf("the only surviving copy was deleted by staging: %v", err)
	}
	if string(body) != "the only copy" {
		t.Fatalf("backup = %q, want it untouched", body)
	}
}

// The orphan guard in stageArtifact is worth nothing if discard() deletes the
// same file moments later. Full sequence: a previous run died between commit's
// two renames, this run stages, and this run then fails.
func TestAFailedRunLeavesAnOrphanBackupIntact(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	report := filepath.Join(dir, "report.json")
	orphan := plan + ".ob-bak.plan"
	if err := os.WriteFile(orphan, []byte("the only copy"), 0o600); err != nil {
		t.Fatal(err)
	}

	stagedReport, err := stageArtifact(report, ".report", writeString("new report"))
	if err != nil {
		t.Fatal(err)
	}
	stagedPlan, err := stageArtifact(plan, ".plan", writeString("new plan"))
	if err != nil {
		t.Fatal(err)
	}
	// Make the plan's commit fail after the report's has landed.
	if err := os.Remove(stagedPlan.staged); err != nil {
		t.Fatal(err)
	}
	stagedPlan.staged = filepath.Join(dir, "never-written")

	if err := commitArtifactSet(stagedReport, stagedPlan); err == nil {
		t.Fatal("commit reported success with a missing staged file")
	}
	body, err := os.ReadFile(orphan)
	if err != nil {
		t.Fatalf("a failed run destroyed the caller's only copy: %v", err)
	}
	if string(body) != "the only copy" {
		t.Fatalf("orphan backup = %q, want it untouched", body)
	}
}

// On success the destination exists again, so the orphan is redundant and goes
// with everything else — otherwise the leak the guard bounds becomes permanent.
func TestASuccessfulRunClearsAnOrphanBackup(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	orphan := plan + ".ob-bak.plan"
	if err := os.WriteFile(orphan, []byte("previous copy"), 0o600); err != nil {
		t.Fatal(err)
	}

	staged, err := stageArtifact(plan, ".plan", writeString("new plan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := commitArtifactSet(staged); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "plan.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory holds %v, want only plan.json", names)
	}
}

// Two concurrent runs against the same --out must not share a staged path.
// Sharing one means the second write wins, the first run commits it and
// reports success, and the second is told it failed while the destination
// holds ITS plan.
func TestStageArtifactReservesADistinctPathPerRun(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")

	first, err := stageArtifact(plan, ".plan", writeString("run A"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := stageArtifact(plan, ".plan", writeString("run B"))
	if err != nil {
		t.Fatal(err)
	}
	if first.staged == second.staged {
		t.Fatalf("both runs staged to %s", first.staged)
	}
	for staged, want := range map[string]string{first.staged: "run A", second.staged: "run B"} {
		body, err := os.ReadFile(staged)
		if err != nil {
			t.Fatalf("staged file %s missing: %v", staged, err)
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q — one run overwrote the other", staged, body, want)
		}
	}
	// Committing one must not consume the other's staged file.
	if err := commitArtifactSet(first); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(plan); err != nil || string(body) != "run A" {
		t.Fatalf("destination = %q %v, want run A", body, err)
	}
	if err := commitArtifactSet(second); err != nil {
		t.Fatalf("the second run could not commit after the first: %v", err)
	}
	if body, err := os.ReadFile(plan); err != nil || string(body) != "run B" {
		t.Fatalf("destination = %q %v, want run B", body, err)
	}
}

// `ob plan --out artifacts/plan.json` into a directory that does not exist yet
// is the normal CI shape. The writers used to create it; staging reserves a
// temp inside it first, so staging has to.
func TestStageArtifactCreatesTheDestinationDirectory(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "artifacts", "nested", "plan.json")
	staged, err := stageArtifact(plan, ".plan", writeString("plan"))
	if err != nil {
		t.Fatalf("staging into a new directory failed: %v", err)
	}
	if err := commitArtifactSet(staged); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if body, err := os.ReadFile(plan); err != nil || string(body) != "plan" {
		t.Fatalf("destination = %q %v, want the plan", body, err)
	}
}

// filepath.Abs runs a lexical Clean, which collapses `link/..` and loses the
// symlink. Two spellings that resolve to one file must still compare equal, or
// both artifacts commit onto one destination.
func TestSameArtifactPathResolvesDotDotThroughASymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "out")
	if err := os.MkdirAll(filepath.Join(real, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(real, "inner"), link); err != nil {
		t.Fatal(err)
	}
	// Built by concatenation, not filepath.Join: Join cleans lexically, so it
	// would collapse `link/..` to root before sameArtifactPath ever saw it —
	// which is exactly the collapse the function has to avoid doing itself.
	through := link + "/../plan.json"
	if !sameArtifactPath(filepath.Join(real, "plan.json"), through) {
		t.Errorf("a `..` through a symlink defeated the collision guard: %s", through)
	}
	if sameArtifactPath(filepath.Join(root, "plan.json"), through) {
		t.Error("two genuinely different files compared equal")
	}
}
