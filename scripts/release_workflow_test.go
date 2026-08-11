package scripts_test

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

//go:embed release.sh
var releaseScript string

type testRepository struct {
	origin string
	work   string
	racer  string
	head   string
}

func TestReleaseRevision(t *testing.T) {
	requireReleaseTools(t)

	t.Run("first release in UTC month", func(t *testing.T) {
		repo := newTestRepository(t)
		month := utcPeriod()
		tag := "v" + month + ".0"

		output, err := runRelease(t, repo, normalJustShim, nil)
		skipIfUTCMonthChanged(t, month)
		if err != nil {
			t.Fatalf("release failed: %v\n%s", err, output)
		}
		assertPublishedRelease(t, repo, tag)
	})

	t.Run("revision compares integers across nine to ten", func(t *testing.T) {
		repo := newTestRepository(t)
		month := utcPeriod()
		for _, tag := range []string{"v" + month + ".8", "v" + month + ".9", "v" + month + ".08", "v" + month + ".09", "v" + month + ".010", "v" + month + ".invalid"} {
			runGit(t, repo.work, "tag", tag, repo.head)
		}
		runGit(t, repo.work, "push", "origin", "--tags")

		tag := "v" + month + ".10"
		output, err := runRelease(t, repo, normalJustShim, nil)
		skipIfUTCMonthChanged(t, month)
		if err != nil {
			t.Fatalf("release failed: %v\n%s", err, output)
		}
		assertPublishedRelease(t, repo, tag)
	})

	t.Run("revision resets in a new UTC month", func(t *testing.T) {
		repo := newTestRepository(t)
		now := time.Now().UTC()
		month := releasePeriod(now)
		// The last day of the previous month, not AddDate(0, -1, 0): from the
		// 31st that normalizes back into the current month, which silently
		// turns this into a same-month test — and fails outright on the days
		// where the normalization lands on today.
		previousMonth := releasePeriod(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1))
		runGit(t, repo.work, "tag", "v"+previousMonth+".41", repo.head)
		runGit(t, repo.work, "push", "origin", "--tags")

		tag := "v" + month + ".0"
		output, err := runRelease(t, repo, normalJustShim, nil)
		skipIfUTCMonthChanged(t, month)
		if err != nil {
			t.Fatalf("release failed: %v\n%s", err, output)
		}
		assertPublishedRelease(t, repo, tag)
	})
}

func TestReleaseRejectsMalformedUTCCalendar(t *testing.T) {
	requireReleaseTools(t)
	for _, calendar := range []string{"26:08", "2026:8", "2026:13"} {
		t.Run(calendar, func(t *testing.T) {
			repo := newTestRepository(t)
			output, err := runReleaseAt(t, repo, normalJustShim, nil, calendar)
			if err == nil {
				t.Fatalf("release unexpectedly accepted %s:\n%s", calendar, output)
			}
			if !strings.Contains(output, "could not determine the UTC release month") {
				t.Fatalf("release did not explain the invalid UTC calendar:\n%s", output)
			}
			if tags := gitOutput(t, repo.work, "tag", "--list", "v*"); tags != "" {
				t.Fatalf("release created tags after refusing the year: %s", tags)
			}
			assertRemoteMain(t, repo, repo.head)
		})
	}
}

func TestReleaseRejectsMainAdvanceDuringChecks(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	month := utcPeriod()
	shim := `#!/bin/sh
set -eu
if [ "${1:-}" = "docs-check" ]; then
  git -C "$RACER_REPO" commit --allow-empty -m during-checks
  git -C "$RACER_REPO" push origin main
fi
`

	output, err := runRelease(t, repo, shim, nil)
	skipIfUTCMonthChanged(t, month)
	if err == nil {
		t.Fatalf("release unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(output, "origin/main advanced while release checks were running") {
		t.Fatalf("release did not report post-check revalidation failure:\n%s", output)
	}
	assertNoReleaseTags(t, repo, month)
	assertRemoteMain(t, repo, gitOutput(t, repo.racer, "rev-parse", "HEAD"))
}

func TestReleaseRejectsMainAdvanceBeforeAtomicPublication(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	month := utcPeriod()
	tag := "v" + month + ".0"
	hook := `#!/bin/sh
set -eu
git -C "$RACER_REPO" fetch origin main
git -C "$RACER_REPO" reset --hard origin/main
git -C "$RACER_REPO" commit --allow-empty -m before-publication
git -C "$RACER_REPO" push origin main
`

	output, err := runRelease(t, repo, normalJustShim, &hook)
	skipIfUTCMonthChanged(t, month)
	if err == nil {
		t.Fatalf("release unexpectedly succeeded after origin/main advanced:\n%s", output)
	}
	assertLocalTag(t, repo, tag, false)
	assertRemoteTag(t, repo, tag, "")
	assertRemoteMain(t, repo, gitOutput(t, repo.racer, "rev-parse", "HEAD"))
	if got := gitOutput(t, repo.work, "rev-parse", "HEAD"); got != repo.head {
		t.Fatalf("local main = %s after failed publication, want %s", got, repo.head)
	}
}

func TestNoOpBranchRefMissesMainAdvanceAfterAdvertisement(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	month := utcPeriod()
	tag := "v" + month + ".0"
	hook := `#!/bin/sh
set -eu
git -C "$RACER_REPO" fetch origin main
git -C "$RACER_REPO" reset --hard origin/main
git -C "$RACER_REPO" commit --allow-empty -m after-advertisement
git -C "$RACER_REPO" push origin main
`
	writeExecutable(t, filepath.Join(repo.work, ".git", "hooks", "pre-push"), hook)
	runGit(t, repo.work, "tag", "--no-sign", tag, repo.head)

	cmd := gitCommand(repo.work,
		"push", "--atomic",
		"--force-with-lease=refs/heads/main:"+repo.head,
		"origin",
		repo.head+":refs/heads/main",
		"refs/tags/"+tag+":refs/tags/"+tag,
	)
	cmd.Env = append(cmd.Env, "RACER_REPO="+repo.racer)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("no-op branch characterization push failed: %v\n%s", err, output)
	}

	assertRemoteMain(t, repo, gitOutput(t, repo.racer, "rev-parse", "HEAD"))
	assertRemoteTag(t, repo, tag, repo.head)
}

func TestReleaseLosesCompetingTagRaceWithoutReplacingWinner(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	month := utcPeriod()
	tag := "v" + month + ".0"
	hook := `#!/bin/sh
set -eu
git -C "$RACER_REPO" commit --allow-empty -m competing-tag
git -C "$RACER_REPO" tag "$RELEASE_TEST_TAG"
git -C "$RACER_REPO" push origin "refs/tags/$RELEASE_TEST_TAG:refs/tags/$RELEASE_TEST_TAG"
`

	output, err := runRelease(t, repo, normalJustShim, &hook, "RELEASE_TEST_TAG="+tag)
	skipIfUTCMonthChanged(t, month)
	if err == nil {
		t.Fatalf("release unexpectedly replaced a competing tag:\n%s", output)
	}
	assertLocalTag(t, repo, tag, false)
	assertRemoteTag(t, repo, tag, gitOutput(t, repo.racer, "rev-parse", "HEAD"))
	assertRemoteMain(t, repo, repo.head)
}

func TestReleaseFailsClosedWhenBranchPolicyRejectsMainUpdate(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	month := utcPeriod()
	tag := "v" + month + ".0"
	hook := `#!/bin/sh
set -eu
while read -r old_object new_object ref_name; do
  if [ "$ref_name" = "refs/heads/main" ]; then
    echo "direct main updates are disabled" >&2
    exit 1
  fi
done
`
	writeExecutable(t, filepath.Join(repo.origin, "hooks", "pre-receive"), hook)

	output, err := runRelease(t, repo, normalJustShim, nil)
	skipIfUTCMonthChanged(t, month)
	if err == nil {
		t.Fatalf("release unexpectedly bypassed branch policy:\n%s", output)
	}
	assertLocalTag(t, repo, tag, false)
	assertRemoteTag(t, repo, tag, "")
	assertRemoteMain(t, repo, repo.head)
	if got := gitOutput(t, repo.work, "rev-parse", "HEAD"); got != repo.head {
		t.Fatalf("local main = %s after branch-policy refusal, want %s", got, repo.head)
	}
}

const normalJustShim = `#!/bin/sh
set -eu
exit 0
`

func requireReleaseTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release workflow requires Bash and Unix Git hooks")
	}
	for _, tool := range []string{"bash", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is unavailable: %v", tool, err)
		}
	}
}

func newTestRepository(t *testing.T) testRepository {
	t.Helper()
	root := t.TempDir()
	repo := testRepository{
		origin: filepath.Join(root, "origin.git"),
		work:   filepath.Join(root, "work"),
		racer:  filepath.Join(root, "racer"),
	}

	runGit(t, root, "init", "--bare", "--initial-branch=main", repo.origin)
	runGit(t, root, "init", "--initial-branch=main", repo.work)
	configureGitIdentity(t, repo.work)
	runGit(t, repo.work, "commit", "--allow-empty", "-m", "initial")
	runGit(t, repo.work, "remote", "add", "origin", repo.origin)
	runGit(t, repo.work, "push", "--set-upstream", "origin", "main")
	runGit(t, root, "clone", repo.origin, repo.racer)
	configureGitIdentity(t, repo.racer)
	repo.head = gitOutput(t, repo.work, "rev-parse", "HEAD")
	return repo
}

func configureGitIdentity(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "config", "user.name", "Onebox Release Test")
	runGit(t, dir, "config", "user.email", "release-test@onebox.invalid")
	runGit(t, dir, "config", "commit.gpgSign", "false")
	runGit(t, dir, "config", "tag.gpgSign", "false")
}

func runRelease(t *testing.T, repo testRepository, justShim string, prePushHook *string, extraEnv ...string) (string, error) {
	t.Helper()
	return runReleaseAt(t, repo, justShim, prePushHook, "", extraEnv...)
}

func runReleaseAt(t *testing.T, repo testRepository, justShim string, prePushHook *string, calendar string, extraEnv ...string) (string, error) {
	t.Helper()
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "just"), justShim)
	// A gh that reports the previous release as published, so the in-flight gate
	// is exercised by the tests that care about it and inert everywhere else.
	ghShim := "#!/usr/bin/env bash\nexit 0\n"
	for _, entry := range extraEnv {
		if value, ok := strings.CutPrefix(entry, "RELEASE_TEST_GH="); ok {
			ghShim = value
		}
	}
	writeExecutable(t, filepath.Join(binDir, "gh"), ghShim)
	if calendar != "" {
		writeExecutable(t, filepath.Join(binDir, "date"), "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$RELEASE_TEST_CALENDAR\"\n")
		extraEnv = append(extraEnv, "RELEASE_TEST_CALENDAR="+calendar)
	}
	if prePushHook != nil {
		writeExecutable(t, filepath.Join(repo.work, ".git", "hooks", "pre-push"), *prePushHook)
	}

	script := filepath.Join(t.TempDir(), "release.sh")
	if err := os.WriteFile(script, []byte(releaseScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = repo.work
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RACER_REPO="+repo.racer,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if output, err := gitCommand(dir, args...).CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := gitCommand(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	return cmd
}

func assertLocalTag(t *testing.T, repo testRepository, tag string, want bool) {
	t.Helper()
	err := gitCommand(repo.work, "rev-parse", "--quiet", "--verify", "refs/tags/"+tag).Run()
	if (err == nil) != want {
		t.Fatalf("local tag %s existence = %v, want %v", tag, err == nil, want)
	}
}

func assertPublishedRelease(t *testing.T, repo testRepository, tag string) {
	t.Helper()
	assertLocalTag(t, repo, tag, true)
	releaseCommit := remoteRef(t, repo, "refs/heads/main")
	if releaseCommit == repo.head {
		t.Fatalf("remote main did not advance from checked commit %s", repo.head)
	}
	assertRemoteTag(t, repo, tag, releaseCommit)
	if got := gitOutput(t, repo.work, "rev-parse", "HEAD"); got != releaseCommit {
		t.Fatalf("local main = %s, want published release commit %s", got, releaseCommit)
	}
	if parent := gitOutput(t, repo.work, "rev-parse", releaseCommit+"^"); parent != repo.head {
		t.Fatalf("release parent = %s, want checked commit %s", parent, repo.head)
	}
	releaseTree := gitOutput(t, repo.work, "rev-parse", releaseCommit+"^{tree}")
	checkedTree := gitOutput(t, repo.work, "rev-parse", repo.head+"^{tree}")
	if releaseTree != checkedTree {
		t.Fatalf("release tree = %s, want checked tree %s", releaseTree, checkedTree)
	}
}

func assertRemoteTag(t *testing.T, repo testRepository, tag, wantObject string) {
	t.Helper()
	output := gitOutput(t, repo.work, "ls-remote", "--tags", "origin", "refs/tags/"+tag)
	if wantObject == "" {
		if output != "" {
			t.Fatalf("remote tag %s unexpectedly exists: %s", tag, output)
		}
		return
	}
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[0] != wantObject {
		t.Fatalf("remote tag %s = %q, want object %s", tag, output, wantObject)
	}
}

func assertNoReleaseTags(t *testing.T, repo testRepository, month string) {
	t.Helper()
	if output := gitOutput(t, repo.work, "tag", "--list", "v"+month+".*"); output != "" {
		t.Fatalf("local release tags unexpectedly exist: %s", output)
	}
	if output := gitOutput(t, repo.work, "ls-remote", "--tags", "origin", "refs/tags/v"+month+".*"); output != "" {
		t.Fatalf("remote release tags unexpectedly exist: %s", output)
	}
}

func assertRemoteMain(t *testing.T, repo testRepository, want string) {
	t.Helper()
	if got := remoteRef(t, repo, "refs/heads/main"); got != want {
		t.Fatalf("remote main = %s, want object %s", got, want)
	}
}

func remoteRef(t *testing.T, repo testRepository, ref string) string {
	t.Helper()
	output := gitOutput(t, repo.work, "ls-remote", "origin", ref)
	fields := strings.Fields(output)
	if len(fields) != 2 {
		t.Fatalf("remote ref %s has unexpected output %q", ref, output)
	}
	return fields[0]
}

func utcPeriod() string {
	return releasePeriod(time.Now().UTC())
}

func releasePeriod(value time.Time) string {
	return fmt.Sprintf("%d.%d", value.Year(), int(value.Month()))
}

func skipIfUTCMonthChanged(t *testing.T, start string) {
	t.Helper()
	if end := utcPeriod(); end != start {
		t.Skipf("UTC month changed during test from %s to %s", start, end)
	}
}

// A nineteen-digit revision is valid, and one past the widest machine integer is
// still valid: the contract bounds the width, not the arithmetic, so the creator
// carries by hand rather than refusing a range it is supposed to serve.
func TestReleaseIncrementsARevisionWiderThanMachineArithmetic(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	month := utcPeriod()
	// One past the signed 64-bit maximum, so plain arithmetic would wrap.
	runGit(t, repo.work, "tag", "v"+month+".9223372036854775808", repo.head)
	runGit(t, repo.work, "push", "origin", "--tags")

	output, err := runRelease(t, repo, normalJustShim, nil)
	skipIfUTCMonthChanged(t, month)
	if err != nil {
		t.Fatalf("release failed: %v\n%s", err, output)
	}
	assertPublishedRelease(t, repo, "v"+month+".9223372036854775809")
}

// Bash arithmetic is signed 64-bit, so incrementing a revision the grammar still
// admits can wrap negative and tag garbage. The creator refuses instead.
func TestReleaseRefusesWhenTheRevisionSpaceIsExhausted(t *testing.T) {
	requireReleaseTools(t)
	repo := newTestRepository(t)
	month := utcPeriod()
	exhausted := "v" + month + ".9999999999999999999"
	runGit(t, repo.work, "tag", exhausted, repo.head)
	runGit(t, repo.work, "push", "origin", "--tags")

	output, err := runRelease(t, repo, normalJustShim, nil)
	skipIfUTCMonthChanged(t, month)
	if err == nil {
		t.Fatalf("release created a tag past the representable revision:\n%s", output)
	}
	if !strings.Contains(output, "revision space for") {
		t.Fatalf("release did not explain the refusal:\n%s", output)
	}
	if tags := gitOutput(t, repo.origin, "tag", "--list"); tags != exhausted {
		t.Fatalf("a refused release must publish no new tag, got:\n%s", tags)
	}
}

// Only one release may be in flight, because GitHub orders queued runs by when
// they start waiting rather than by tag age: two tags created close together can
// publish out of order, and the older one is then correctly refused at publish
// time — a release nobody gets. This refuses to create the second tag instead.
func TestReleaseWaitsForThePreviousReleaseToPublish(t *testing.T) {
	requireReleaseTools(t)
	for _, test := range []struct {
		name      string
		published bool
		wantErr   string
	}{
		{name: "previous release published", published: true},
		{name: "previous release still publishing", wantErr: "has not published yet"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newTestRepository(t)
			month := utcPeriod()
			runGit(t, repo.work, "tag", "v"+month+".0", repo.head)
			runGit(t, repo.work, "push", "origin", "--tags")

			stub := "#!/usr/bin/env bash\nexit 1\n"
			if test.published {
				stub = "#!/usr/bin/env bash\nexit 0\n"
			}
			output, err := runRelease(t, repo, normalJustShim, nil, "OB_RELEASE_REPOSITORY=labstack/onebox", "RELEASE_TEST_GH="+stub)
			skipIfUTCMonthChanged(t, month)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("release failed: %v\n%s", err, output)
			case test.wantErr == "":
				assertPublishedRelease(t, repo, "v"+month+".1")
			case err == nil:
				t.Fatalf("release created a tag while the previous one was still publishing:\n%s", output)
			case !strings.Contains(output, test.wantErr):
				t.Fatalf("release did not explain the refusal:\n%s", output)
			}
		})
	}
}
