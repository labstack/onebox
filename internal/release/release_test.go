package release

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/transport"

	"github.com/labstack/onebox/internal/app"
)

func TestNewID(t *testing.T) {
	id := NewID(time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC), "abc1234")
	if id != "20260702-150405-abc1234" {
		t.Fatal(id)
	}
	if !strings.HasSuffix(NewID(time.Now(), ""), "-nogit") {
		t.Fatal("empty sha should yield -nogit")
	}
	if !strings.HasSuffix(NewID(time.Now(), "not$(safe)"), "-nogit") {
		t.Fatal("unsafe sha must be replaced with nogit")
	}
}

func seedReleaseChain(t *testing.T, target *transport.Fake, names app.Names, previousID, currentID string) {
	t.Helper()
	at := time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC)
	previous, err := NewManifest(previousID, KindApplication, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{StateVerified, StateServing, StateSuperseded} {
		if err := previous.Transition(state, at, ""); err != nil {
			t.Fatal(err)
		}
	}
	current, err := NewManifest(currentID, KindApplication, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Transition(StateVerified, at, ""); err != nil {
		t.Fatal(err)
	}
	if err := current.Transition(StateServing, at, previousID); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(context.Background(), target, names, previous); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(context.Background(), target, names, current); err != nil {
		t.Fatal(err)
	}
}

func TestPreviousAndPrune(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`readlink`), Result: transport.Result{Stdout: "releases/20260702-030000-ccc\n"}},
		{Match: regexp.MustCompile(`ls -1`), Result: transport.Result{
			Stdout: "20260701-010000-aaa\n20260701-020000-bbb\n20260702-030000-ccc\n"}},
	}}
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	seedReleaseChain(t, f, names, "20260701-020000-bbb", "20260702-030000-ccc")
	prev, err := Previous(context.Background(), f, names)
	if err != nil || prev != "20260701-020000-bbb" {
		t.Fatalf("prev=%q err=%v", prev, err)
	}
	removed, err := Prune(context.Background(), f, names, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "20260701-010000-aaa" {
		t.Fatalf("removed=%v", removed)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "rm -rf '/var/lib/ob/sample/releases/20260701-010000-aaa'") {
		t.Fatalf("prune command missing:\n%s", joined)
	}
}

func TestPruneReturnsRemoteRemovalFailure(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/20260103-000000-ccc\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "20260101-000000-aaa\n20260102-000000-bbb\n20260103-000000-ccc\n"}, true
		case strings.Contains(cmd, "rm -rf"):
			return transport.Result{ExitCode: 13, Stderr: "permission denied"}, true
		}
		return transport.Result{}, false
	}}
	removed, err := Prune(context.Background(), f, app.Names{App: "sample", BasePath: app.DefaultBasePath}, 2)
	if len(removed) != 0 || err == nil || !strings.Contains(err.Error(), "prune release 20260101-000000-aaa failed (exit 13): permission denied") {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
}

// Everything under the releases directory is handed to rollback by Previous and
// counted against retention by PruneCandidates, so anything that is not a
// release id has to be excluded rather than assumed absent. An upload staging
// directory landing there once made rollback activate the debris.
func TestListIgnoresEntriesThatAreNotReleaseIds(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/20260102-000000-bbb\n"}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: strings.Join([]string{
				"20260101-000000-aaa",
				"20260102-000000-bbb.partial",
				"20260102-000000-bbb",
				".uploads",
				"lost+found",
			}, "\n")}, true
		}
		return transport.Result{}, false
	}}
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	seedReleaseChain(t, f, names, "20260101-000000-aaa", "20260102-000000-bbb")

	prev, err := Previous(context.Background(), f, names)
	if err != nil {
		t.Fatalf("previous: %v", err)
	}
	if prev != "20260101-000000-aaa" {
		t.Errorf("rollback would target %q, not the previous release", prev)
	}

	candidates, unrecognized, err := PruneCandidates(context.Background(), f, names, 2)
	if err != nil {
		t.Fatalf("prune candidates: %v", err)
	}
	for _, id := range candidates {
		if !IsID(id) {
			t.Errorf("prune would remove %q, which is not a release", id)
		}
	}
	if len(candidates) != 0 {
		t.Errorf("two real releases with retain=2 should leave nothing to prune, got %v", candidates)
	}
	// The three non-releases are excluded from rollback and retention, which is
	// the safe direction — but they still occupy the directory retention is
	// enforced over, so they have to be reported rather than silently dropped.
	want := []string{"20260102-000000-bbb.partial", ".uploads", "lost+found"}
	if !slices.Equal(unrecognized, want) {
		t.Errorf("unrecognised entries = %v, want %v", unrecognized, want)
	}
}

// A real release that IsID rejected would be invisible: on the host but absent
// from rollback and from retention. That is the dangerous direction, so the
// error has to distinguish it from a genuinely empty releases directory.
func TestPreviousRejectsUnknownCurrentManifestWithoutUsingDirectoryOrder(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/20260102-000000-bbb\n"}, true
		case strings.Contains(cmd, "/manifest.json") && strings.Contains(cmd, "mode=%s"):
			return transport.Result{Stdout: "mode=600\n{\"schema_version\":\"future\"}\n"}, true
		}
		return transport.Result{}, false
	}}

	_, err := Previous(context.Background(), f, app.Names{App: "sample", BasePath: app.DefaultBasePath})
	if err == nil {
		t.Fatal("previous accepted an unknown current manifest")
	}
	if !strings.Contains(err.Error(), "manifest_schema_unknown") {
		t.Errorf("error does not preserve the unknown-schema cause: %v", err)
	}
	for _, command := range f.Commands {
		if strings.Contains(command, "ls -1") {
			t.Fatalf("rollback consulted directory order after manifest failure: %s", command)
		}
	}
}

func TestPreviousFollowsRecordedPredecessorAcrossMixedStore(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	target := &transport.Fake{}
	seedReleaseChain(t, target, names, "20260101-000000-aaa", "20260103-000000-ccc")
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/20260103-000000-ccc\n"}, true
		case strings.Contains(command, "ls -1"):
			return transport.Result{Stdout: "20260101-000000-aaa\n20260102-000000-bootstrap\n20260103-000000-ccc\n20260104-000000-newer\nupload.partial\n"}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}
	previous, err := Previous(context.Background(), target, names)
	if err != nil || previous != "20260101-000000-aaa" {
		t.Fatalf("previous = %q, %v", previous, err)
	}
	for _, command := range target.Commands {
		if strings.Contains(command, "ls -1") {
			t.Fatalf("rollback consulted mixed directory order: %s", command)
		}
	}
}

func TestPreviousAllowsFailedPostActivationOutcome(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "readlink") {
			return transport.Result{Stdout: "releases/20260102-000000-bbb\n"}, true
		}
		return transport.Result{}, false
	}}
	seedReleaseChain(t, target, names, "20260101-000000-aaa", "20260102-000000-bbb")
	current, err := ReadManifest(context.Background(), target, names, "20260102-000000-bbb")
	if err != nil {
		t.Fatal(err)
	}
	if err := current.RecordOperationOutcome(OutcomeFailed, time.Date(2026, 7, 2, 4, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(context.Background(), target, names, current); err != nil {
		t.Fatal(err)
	}
	previous, err := Previous(context.Background(), target, names)
	if err != nil || previous != "20260101-000000-aaa" {
		t.Fatalf("previous = %q, %v", previous, err)
	}
}

func TestPreviousRejectsCorruptUnknownAndBootstrapTargets(t *testing.T) {
	for _, test := range []struct {
		name       string
		targetBody string
		want       string
	}{
		{name: "corrupt", targetBody: "not-json", want: "manifest_invalid"},
		{name: "unknown schema", targetBody: `{"schema_version":"future"}`, want: "manifest_schema_unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
			target := &transport.Fake{}
			seedReleaseChain(t, target, names, "20260101-000000-aaa", "20260102-000000-bbb")
			base := target.Dynamic
			target.Dynamic = func(command string) (transport.Result, bool) {
				switch {
				case strings.Contains(command, "readlink"):
					return transport.Result{Stdout: "releases/20260102-000000-bbb\n"}, true
				case strings.Contains(command, "/releases/20260101-000000-aaa/manifest.json") && strings.Contains(command, "mode=%s"):
					return transport.Result{Stdout: "mode=600\n" + test.targetBody + "\n"}, true
				}
				if base != nil {
					return base(command)
				}
				return transport.Result{}, false
			}
			_, err := Previous(context.Background(), target, names)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("bootstrap", func(t *testing.T) {
		names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
		target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
			if strings.Contains(command, "readlink") {
				return transport.Result{Stdout: "releases/20260102-000000-bbb\n"}, true
			}
			return transport.Result{}, false
		}}
		at := time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC)
		bootstrap, err := NewManifest("20260101-000000-aaa", KindBootstrap, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := bootstrap.Transition(StateVerified, at, ""); err != nil {
			t.Fatal(err)
		}
		current, err := NewManifest("20260102-000000-bbb", KindApplication, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := current.Transition(StateVerified, at, ""); err != nil {
			t.Fatal(err)
		}
		if err := current.Transition(StateServing, at, "20260101-000000-aaa"); err != nil {
			t.Fatal(err)
		}
		if err := WriteManifest(context.Background(), target, names, bootstrap); err != nil {
			t.Fatal(err)
		}
		if err := WriteManifest(context.Background(), target, names, current); err != nil {
			t.Fatal(err)
		}
		_, err = Previous(context.Background(), target, names)
		if err == nil || !strings.Contains(err.Error(), "not a previously serving application release") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestIsIDAcceptsWhatNewIDProduces(t *testing.T) {
	for _, id := range []string{NewID(time.Now(), "abc1234"), NewID(time.Now(), ""), NewID(time.Now(), "abc1234") + "-v2"} {
		if !IsID(id) {
			t.Errorf("IsID rejected an id NewID produced: %q", id)
		}
	}
	for _, notID := range []string{"", ".uploads", "20260101-000000-aaa.partial", "lost+found", "current"} {
		if IsID(notID) {
			t.Errorf("IsID accepted %q", notID)
		}
	}
}
