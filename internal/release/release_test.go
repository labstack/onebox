package release

import (
	"context"
	"regexp"
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

func TestPreviousAndPrune(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`readlink`), Result: transport.Result{Stdout: "releases/20260702-030000-ccc\n"}},
		{Match: regexp.MustCompile(`ls -1`), Result: transport.Result{
			Stdout: "20260701-010000-aaa\n20260701-020000-bbb\n20260702-030000-ccc\n"}},
	}}
	prev, err := Previous(context.Background(), f, app.Names{App: "sample", BasePath: app.DefaultBasePath})
	if err != nil || prev != "20260701-020000-bbb" {
		t.Fatalf("prev=%q err=%v", prev, err)
	}
	removed, err := Prune(context.Background(), f, app.Names{App: "sample", BasePath: app.DefaultBasePath}, 2)
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

	prev, err := Previous(context.Background(), f, names)
	if err != nil {
		t.Fatalf("previous: %v", err)
	}
	if prev != "20260101-000000-aaa" {
		t.Errorf("rollback would target %q, not the previous release", prev)
	}

	candidates, err := PruneCandidates(context.Background(), f, names, 2)
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
