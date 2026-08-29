package engine

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// A tag must always resolve through the registry, because a tag can move. A
// digest must not: it is immutable, so a second pull cannot return different
// bytes — it only spends registry quota and makes a re-enable fail on a host
// that is offline or rate-limited while already holding exactly what it needs.
func TestOnlyDigestPinnedReferencesSkipTheRegistry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reference string
		skippable bool
	}{
		{"tag", "postgres:18", false},
		{"tag that looks pinned", "postgres:sha256-abc", false},
		{"digest", "postgres@sha256:" + "a" + "b" + "c", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The guard is the reference shape; the registry call is what it
			// gates. Asserting the shape keeps the rule readable without a
			// docker daemon.
			got := containsDigest(tc.reference)
			if got != tc.skippable {
				t.Fatalf("containsDigest(%q) = %v, want %v", tc.reference, got, tc.skippable)
			}
		})
	}
}

// A service that is already bound to a repository keeps the exact bytes it was
// bound with. Re-running enable — after a policy edit, or after a disable
// somebody changed their mind about — must not re-resolve the tag: the bytes
// would move under a live data directory the moment upstream published a patch,
// and the command would need a registry it has no reason to need. Both showed
// up the same way in a live run: `ob backup enable` on an already-enabled
// service failed on a Docker Hub 429 while the host held the pinned image.
func TestReEnableKeepsTheRecordedPinAndDoesNotReachTheRegistry(t *testing.T) {
	const pin = "ghcr.io/labstack/onebox-postgres@sha256:06cad38a5d9f5d24b4d83d86def30795d5e4b757fedbf5281172b576dedcd941"
	fake := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker image inspect") && strings.Contains(cmd, pin) {
			return transport.Result{Stdout: "present\n"}, true
		}
		return transport.Result{}, false
	}}
	e := protectedImageTestEngine(fake)

	// Recorded as the authored reference, which is what an enable after a
	// disable still declares even though the runtime selection has reverted.
	got, err := e.ResolveProtectedImage(context.Background(), "database", pin, "ghcr.io/labstack/onebox-postgres:18")
	if err != nil {
		t.Fatalf("resolving an already-bound image: %v", err)
	}
	if got != pin {
		t.Fatalf("resolved %q, want the recorded pin %q", got, pin)
	}
	for _, cmd := range fake.Commands {
		if strings.Contains(cmd, "docker pull") {
			t.Fatalf("pulled while holding the recorded pin: %s", cmd)
		}
	}
}

func TestReEnableRejectsPinFromDifferentRepository(t *testing.T) {
	const stalePin = "postgres@sha256:06cad38a5d9f5d24b4d83d86def30795d5e4b757fedbf5281172b576dedcd941"
	const managedPin = "ghcr.io/labstack/onebox-postgres@sha256:16cad38a5d9f5d24b4d83d86def30795d5e4b757fedbf5281172b576dedcd942"
	fake := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker image inspect") && strings.Contains(cmd, stalePin):
			return transport.Result{Stdout: "present\n"}, true
		case strings.Contains(cmd, "docker pull"):
			return transport.Result{}, true
		case strings.Contains(cmd, "RepoDigests"):
			return transport.Result{Stdout: managedPin + "\n"}, true
		}
		return transport.Result{Stdout: "absent\n"}, true
	}}
	e := protectedImageTestEngine(fake)
	bound, err := e.Spec.WithServiceRuntimeStates(map[string]app.ServiceRuntimeState{
		"database": {
			BackupState: "disabled", ServiceImage: stalePin,
			PublicationVerified: true, DigestAvailable: true,
		},
	})
	if err != nil {
		t.Fatalf("binding disabled service state: %v", err)
	}
	e.Spec = bound

	got, err := e.ResolveProtectedImage(context.Background(), "database", stalePin, "ghcr.io/labstack/onebox-postgres:18")
	if err != nil {
		t.Fatalf("resolving an inconsistent recorded pin: %v", err)
	}
	if got != managedPin {
		t.Fatalf("resolved %q, want %q", got, managedPin)
	}
}

func TestReEnableDoesNotCreatePinFromDifferentRepository(t *testing.T) {
	const stalePin = "postgres@sha256:06cad38a5d9f5d24b4d83d86def30795d5e4b757fedbf5281172b576dedcd941"
	const managedPin = "ghcr.io/labstack/onebox-postgres@sha256:16cad38a5d9f5d24b4d83d86def30795d5e4b757fedbf5281172b576dedcd942"
	fake := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker image inspect") && strings.Contains(cmd, stalePin):
			return transport.Result{Stdout: "present\n"}, true
		case strings.Contains(cmd, "docker pull"):
			return transport.Result{}, true
		case strings.Contains(cmd, "RepoDigests"):
			return transport.Result{Stdout: managedPin + "\n"}, true
		}
		return transport.Result{Stdout: "absent\n"}, true
	}}
	e := protectedImageTestEngine(fake)
	bound, err := e.Spec.WithServiceRuntimeStates(map[string]app.ServiceRuntimeState{
		"database": {
			BackupState: "enabled", ServiceImage: stalePin,
			PublicationVerified: true, DigestAvailable: true,
		},
	})
	if err != nil {
		t.Fatalf("binding enabled service state: %v", err)
	}
	e.Spec = bound

	got, err := e.ResolveProtectedImage(context.Background(), "database", stalePin, "postgres:18")
	if err != nil {
		t.Fatalf("resolving after a repository migration: %v", err)
	}
	if got != managedPin {
		t.Fatalf("resolved %q, want %q", got, managedPin)
	}
}

// The pin is only reusable while the project still declares the reference that
// produced it. Changing the declared version is how an operator asks for
// different bytes, and that has to reach the registry.
func TestADeclaredVersionChangeStillResolvesThroughTheRegistry(t *testing.T) {
	const stalePin = "ghcr.io/labstack/onebox-postgres@sha256:06cad38a5d9f5d24b4d83d86def30795d5e4b757fedbf5281172b576dedcd941"
	fake := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker pull"):
			return transport.Result{}, true
		case strings.Contains(cmd, "RepoDigests"):
			return transport.Result{Stdout: "ghcr.io/labstack/onebox-postgres@sha256:" + strings.Repeat("b", 64) + "\n"}, true
		}
		return transport.Result{Stdout: "absent\n"}, true
	}}
	e := protectedImageTestEngine(fake)

	// Recorded against postgres:17; the project now declares 18.
	got, err := e.ResolveProtectedImage(context.Background(), "database", stalePin, "ghcr.io/labstack/onebox-postgres:17")
	if err != nil {
		t.Fatalf("resolving after a declared version change: %v", err)
	}
	if got == stalePin {
		t.Fatal("kept the pin recorded for a reference the project no longer declares")
	}
	pulled := false
	for _, cmd := range fake.Commands {
		if strings.Contains(cmd, "docker pull") {
			pulled = true
		}
	}
	if !pulled {
		t.Fatal("a changed declared reference resolved without reaching the registry")
	}
}

func protectedImageTestEngine(fake *transport.Fake) *Engine {
	spec := &app.Spec{
		Name:     "shop",
		BasePath: "/var/lib/ob",
		Services: map[string]app.Service{"database": {Driver: "postgres", Version: "18"}},
	}
	resolved := &app.Resolved{Spec: spec, Env: "production"}
	return New(resolved, nil, fake, Options{Out: io.Discard, Sleep: func(time.Duration) {}})
}

// `image.pull` was defaulted, validated and documented, and read by nothing:
// every release pulled every workload from the registry, including an image
// already pinned by digest and already on the host. That is a request that
// cannot change the outcome, and on a rate-limited registry it failed a deploy
// with nothing to fetch.
func TestPullPolicyDecidesWhetherTheRegistryIsAskedAtAll(t *testing.T) {
	const pinned = "nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10"
	for _, tc := range []struct {
		name   string
		policy string
		held   bool
		pull   bool
	}{
		{"never, held", "never", true, false},
		{"never, absent", "never", false, false},
		{"missing, held", "missing", true, false},
		{"missing, absent", "missing", false, true},
		{"always, held", "always", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			presence := "absent\n"
			if tc.held {
				presence = "present\n"
			}
			fake := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, "docker image inspect") {
					return transport.Result{Stdout: presence}, true
				}
				return transport.Result{}, false
			}}
			e := pullPolicyTestEngine(fake, tc.policy, pinned)

			if err := e.pullBeforeRelease(context.Background(), "web", "docker compose -p shop"); err != nil {
				t.Fatalf("pull decision: %v", err)
			}
			pulled := false
			for _, cmd := range fake.Commands {
				if strings.Contains(cmd, "pull --quiet") {
					pulled = true
				}
			}
			if pulled != tc.pull {
				t.Fatalf("pulled = %v, want %v (commands: %v)", pulled, tc.pull, fake.Commands)
			}
		})
	}
}

func pullPolicyTestEngine(fake *transport.Fake, policy, image string) *Engine {
	spec := &app.Spec{
		Name:     "shop",
		BasePath: "/var/lib/ob",
		Workloads: map[string]app.Workload{
			"web": {Role: "application", Image: &app.Image{Reference: image, Pull: policy}},
		},
	}
	e := New(&app.Resolved{Spec: spec, Env: "production"}, nil, fake,
		Options{Out: io.Discard, Sleep: func(time.Duration) {}})
	e.Compose = &ctypes.Project{Services: ctypes.Services{"web": ctypes.ServiceConfig{Name: "web", Image: image}}}
	return e
}
