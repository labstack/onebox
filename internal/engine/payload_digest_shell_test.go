package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

// The remote digest is a shell pipeline, and the rest of the suite only asserts
// its shape against a fake. This runs the real thing through the Local
// transport and requires it to agree with the local walk — the property the
// whole comparison depends on, and the one a rewrite of the pipeline can break
// silently.
func remoteDigestEngine(t *testing.T, base string) *Engine {
	t.Helper()
	resolved := testConfig()
	resolved.Spec.BasePath = base
	return New(resolved, testProject(t), transport.NewLocal(),
		Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
}

func TestRemotePayloadDigestAgreesWithTheLocalWalk(t *testing.T) {
	base := t.TempDir()
	e := remoteDigestEngine(t, base)
	dir := release.PathsFor(e.Names()).Releases + "/R7"
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"compose.yaml":    "services: {}\n",
		"ob.snapshot.yml": "app: sample\n",
		"server/.env":     "KEY=one\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	local, err := LocalPayloadDigest(testConfig(), dir)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := e.RemotePayloadDigest(context.Background(), "R7")
	if err != nil {
		t.Fatalf("remote digest failed: %v", err)
	}
	if remote != local {
		t.Fatalf("remote digest %q != local digest %q", remote, local)
	}
}

// An empty payload is the case a staged listing gets wrong most easily: a
// stray newline hashes differently from the local walk's empty input.
func TestRemotePayloadDigestMatchesOnAnEmptyPayload(t *testing.T) {
	base := t.TempDir()
	e := remoteDigestEngine(t, base)
	dir := release.PathsFor(e.Names()).Releases + "/R8"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	local, err := LocalPayloadDigest(testConfig(), dir)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := e.RemotePayloadDigest(context.Background(), "R8")
	if err != nil {
		t.Fatalf("remote digest failed on an empty payload: %v", err)
	}
	if remote != local {
		t.Fatalf("empty payload: remote %q != local %q", remote, local)
	}
}

// A shape assertion, not a behavioural one: making sort fail needs a full
// $TMPDIR, which a unit test cannot arrange. What it pins is the rule the
// behavioural tests cannot reach — every stage propagates its own status,
// because a pipeline reports only its last stage and a truncated listing
// hashes to a well-formed wrong digest.
func TestRemotePayloadDigestGuardsEveryStage(t *testing.T) {
	e := remoteDigestEngine(t, t.TempDir())
	var issued string
	fake := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		issued = cmd
		return transport.Result{Stdout: "abc  -\n"}, true
	}}
	e.T = fake
	if _, err := e.RemotePayloadDigest(context.Background(), "R1"); err != nil {
		t.Fatal(err)
	}
	for _, guard := range []string{
		`-exec sha256sum {} + > "$listing" || exit $?`,
		`sort "$listing" > "$sorted" || exit $?`,
		`digest=$(sha256sum < "$sorted") || exit $?`,
	} {
		if !strings.Contains(issued, guard) {
			t.Errorf("digest command does not guard a stage: missing %q\n%s", guard, issued)
		}
	}
	if strings.Contains(issued, "| sha256sum") {
		t.Errorf("digest folds through a pipeline, which reports only its last stage:\n%s", issued)
	}
}

// An unreadable file must fail the read rather than quietly shrink the digest
// to the readable subset — the reason the listing is staged at all.
func TestRemotePayloadDigestFailsOnAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file, so the failure arm cannot be exercised")
	}
	base := t.TempDir()
	e := remoteDigestEngine(t, base)
	dir := release.PathsFor(e.Names()).Releases + "/R9"
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "server", ".env")
	if err := os.WriteFile(secret, []byte("KEY=one\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })

	if _, err := e.RemotePayloadDigest(context.Background(), "R9"); err == nil {
		t.Fatal("an unreadable payload file produced a digest instead of an error")
	}
}

// A missing release directory must fail, not digest whatever the login shell's
// working directory happens to hold. Written as `cd DIR && trap …; …` the &&
// binds to the trap alone and a failed cd falls through to `find .` in $HOME,
// producing a well-formed digest of the wrong tree and exit 0 — which reads
// downstream as "the payload changed" on a host where nothing did.
func TestRemotePayloadDigestFailsWhenTheReleaseDirectoryIsMissing(t *testing.T) {
	base := t.TempDir()
	e := remoteDigestEngine(t, base)

	// A file in the process's own working directory, so a digest computed in
	// the wrong place would still be well-formed.
	digest, err := e.RemotePayloadDigest(context.Background(), "R-does-not-exist")
	if err == nil {
		t.Fatalf("a missing release directory produced digest %q instead of an error", digest)
	}
	if digest != "" {
		t.Errorf("digest = %q, want empty on failure", digest)
	}
}

// An unreadable release directory is the same class: cd fails, and the answer
// must be an error rather than a digest of somewhere else.
func TestRemotePayloadDigestFailsWhenTheReleaseDirectoryIsUnsearchable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches every directory, so the permission arm cannot be exercised")
	}
	base := t.TempDir()
	e := remoteDigestEngine(t, base)
	dir := release.PathsFor(e.Names()).Releases + "/R10"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ob.snapshot.yml"), []byte("app: sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if digest, err := e.RemotePayloadDigest(context.Background(), "R10"); err == nil {
		t.Fatalf("an unsearchable release directory produced digest %q instead of an error", digest)
	}
}
