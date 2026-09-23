package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// Bootstrap performs first contact: base directories → the user's bootstrap
// hook (host-specific provisioning — docker install, tailscale, data dirs —
// stays the operator's, config management is a non-goal) → registry login →
// record bootstrap evidence → start supporting services. It never stages or
// activates application workloads; after bootstrap every deploy is a pure
// release.
func (e *Engine) Bootstrap(ctx context.Context, releaseID string) (err error) {
	// Several registries may be declared; a login is attempted for each that
	// carries credentials, and a named-but-empty password variable is an error
	// rather than a silent anonymous pull that fails later on a private image.
	passwords := map[string]string{}
	for _, name := range sortedNames(e.Spec.Registries) {
		r := e.Spec.Registries[name]
		if r.PasswordEnv == "" {
			continue
		}
		password := os.Getenv(r.PasswordEnv)
		if password == "" {
			return fmt.Errorf("registry %s: env var %s is empty — export the registry password first", name, r.PasswordEnv)
		}
		passwords[name] = password
	}
	// Refuse a foreign host before touching anything, then claim the state
	// directory before the host: refusing a directory Onebox did not create
	// must not leave the host claimed.
	if err := e.refuseForeignHostOwner(ctx); err != nil {
		return err
	}
	e.logf("bootstrap: base dirs")
	p := release.PathsFor(e.names())
	if err := e.claimAppDir(ctx); err != nil {
		return err
	}
	if err := e.claimHostOwner(ctx); err != nil {
		return err
	}

	// one regime for every mutation: bootstrap locks, fences,
	// and journals like a deploy
	epoch, err := e.AcquireLock(ctx, releaseID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, releaseID, epoch); err != nil {
		return err
	}
	jw := &journal.Writer{T: e.T, Dir: journal.Dir(e.names()), DeployID: releaseID, Epoch: epoch, Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner}
	if err := jw.Append(ctx, journal.Record{Phase: "bootstrap", Event: "start"}); err != nil {
		return fmt.Errorf("journal bootstrap start: %w", err)
	}
	defer func() {
		finish := journal.Record{Phase: "bootstrap", Event: "finish", Status: "ok"}
		if err != nil {
			finish.Status = "fail"
			finish.Detail = err.Error()
		}
		if journalErr := jw.Append(ctx, finish); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal bootstrap finish: %w", journalErr))
		}
	}()

	if err := e.RunHook(ctx, "bootstrap", p.Base, ""); err != nil {
		return fmt.Errorf("bootstrap hook: %w", err)
	}

	// Docker is an explicit host prerequisite, never an implicit network
	// installer. The authored hook runs first so an operator may deliberately
	// provision a pinned runtime inside the lock, fence, and journal boundary.
	if err := app.RequireHostPrerequisites(ctx, e.T); err != nil {
		// A transport failure is not a missing prerequisite. Framing an SSH
		// reset as one would answer "the connection dropped" with "declare an
		// installer hook", which is the wrong repair entirely.
		var unmet *app.Error
		if !errors.As(err, &unmet) {
			return fmt.Errorf("cannot reach %s to check host prerequisites: %w", e.T.Host(), err)
		}
		return app.HostPrerequisiteRefusal("host is not deployable after the bootstrap hook: %s. To have Onebox run a pinned installer inside the lock, fence and journal boundary, declare it as a remote bootstrap hook", unmet.Message)
	}
	if err := e.EnsureApplicationNetwork(ctx); err != nil {
		return fmt.Errorf("application network: %w", err)
	}

	for _, name := range sortedNames(e.Spec.Registries) {
		r, password := e.Spec.Registries[name], passwords[name]
		if password == "" {
			continue
		}
		e.logf("bootstrap: registry login %s", r.Server)
		res, err := e.T.RunInput(ctx, "docker login "+q(r.Server)+" -u "+q(r.Username)+" --password-stdin", password+"\n")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("docker login %s failed: %s", r.Server, strings.TrimSpace(res.Stderr))
		}
	}

	// managed proxy before services: role containers join its network, and
	// preflight asserts it healthy from the first deploy on. EnsureProxy takes
	// the HOST lock internally (own-app lock is already held — safe order).
	if e.Spec.Proxy.Managed {
		if err := e.EnsureProxy(ctx, releaseID, e.Opts.ForceLock); err != nil {
			return fmt.Errorf("managed proxy: %w", err)
		}
	}

	e.logf("bootstrap: recording evidence %s", releaseID)
	evidenceDir := p.Releases + "/" + releaseID
	// mkdir without -p makes the identity a create-only boundary: an operation
	// may never overwrite evidence already stored under the same identity.
	if res, err := e.mutate(ctx, "mkdir -m 700 "+q(evidenceDir)); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("bootstrap evidence directory: %s", strings.TrimSpace(res.Stderr))
	}
	manifest, err := release.NewManifest(releaseID, release.KindBootstrap, e.Opts.Now())
	if err != nil {
		return fmt.Errorf("bootstrap manifest: %w", err)
	}
	if err := e.writeReleaseManifest(ctx, manifest); err != nil {
		return fmt.Errorf("write bootstrap manifest: %w", err)
	}
	defer func() {
		if err == nil || manifest.State != release.StateStaged {
			return
		}
		if transitionErr := manifest.Transition(release.StateFailed, e.Opts.Now(), ""); transitionErr != nil {
			err = errors.Join(err, fmt.Errorf("fail bootstrap manifest: %w", transitionErr))
			return
		}
		if writeErr := e.writeReleaseManifest(ctx, manifest); writeErr != nil {
			err = errors.Join(err, fmt.Errorf("write failed bootstrap manifest: %w", writeErr))
		}
	}()

	if len(e.Spec.ServiceNames()) > 0 {
		e.logf("bootstrap: starting services %v", e.Spec.ServiceNames())
		if err := e.ApplyServices(ctx); err != nil {
			return fmt.Errorf("services: %w", err)
		}
	}
	if err := manifest.Transition(release.StateVerified, e.Opts.Now(), ""); err != nil {
		return fmt.Errorf("verify bootstrap manifest: %w", err)
	}
	if err := e.writeReleaseManifest(ctx, manifest); err != nil {
		return fmt.Errorf("write verified bootstrap manifest: %w", err)
	}
	e.logf("bootstrap complete — run `ob deploy` for the first release")
	return nil
}

const (
	appDirForeign  = 3
	appDirUnmarked = 4
)

// claimAppDir creates the application's state directory, or accepts one that
// carries this application's marker. It is the only place the directory is
// created — bootstrap and every lock acquisition go through it — so nothing
// ever writes into a directory Onebox has not marked as this application's.
func (e *Engine) claimAppDir(ctx context.Context) error {
	n := e.names()
	res, err := e.T.Run(ctx, claimAppDirCommand(n, e.Spec.Name))
	if err != nil {
		return fmt.Errorf("claim %s: %w", n.AppDir(), err)
	}
	switch res.ExitCode {
	case 0:
		return nil
	case appDirForeign:
		return fmt.Errorf("%s holds another application's state (%s says %q); choose another basePath", n.AppDir(), app.AppMarkerFile, strings.TrimSpace(res.Stdout))
	case appDirUnmarked:
		return fmt.Errorf("%s already exists and was not created by Onebox, or cannot be read; move it aside or choose another basePath — Onebox will not adopt a directory it may later delete", n.AppDir())
	default:
		return fmt.Errorf("claim %s: %s", n.AppDir(), strings.TrimSpace(res.Stderr))
	}
}

// claimAppDirCommand creates the directory and its marker, or accepts this
// application's marker. An existing directory without the marker is refused
// unless it is provably empty: unreadable counts as not empty.
func claimAppDirCommand(n app.Names, application string) string {
	dir, marker := q(n.AppDir()), q(n.AppMarker())
	return "if [ -e " + marker + " ]; then owner=$(cat " + marker + ") || exit 1; " +
		"[ \"$owner\" = " + q(application) + " ] || { printf '%s' \"$owner\"; exit " + fmt.Sprint(appDirForeign) + "; }; " +
		"elif [ -e " + dir + " ] || [ -L " + dir + " ]; then " +
		"[ -d " + dir + " ] && [ -r " + dir + " ] && [ -x " + dir + " ] || exit " + fmt.Sprint(appDirUnmarked) + "; " +
		"entries=$(ls -A " + dir + ") || exit " + fmt.Sprint(appDirUnmarked) + "; " +
		"[ -z \"$entries\" ] || exit " + fmt.Sprint(appDirUnmarked) + "; fi; " +
		"mkdir -p " + q(n.ReleasesDir()) + " && printf '%s\\n' " + q(application) + " > " + marker
}
