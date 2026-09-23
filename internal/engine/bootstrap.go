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
	// Refuse a foreign owner before anything else, check the state directory
	// before claiming the host, and create the directory only after: a refused
	// directory leaves the host unclaimed, and a refused claim leaves nothing
	// behind. The lock acquisition below creates the directory.
	owner, err := e.readHostOwner(ctx)
	if err != nil {
		return err
	}
	if err := e.ownerConflict(owner); err != nil {
		return err
	}
	if err := e.checkAppDir(ctx); err != nil {
		return err
	}
	if err := e.claimHostOwner(ctx, owner); err != nil {
		return err
	}
	e.logf("bootstrap: base dirs")
	p := release.PathsFor(e.names())

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
	return e.runAppDirCommand(ctx, claimAppDirCommand(e.names(), e.Spec.Name))
}

// checkAppDir refuses a state directory Onebox may not adopt, changing nothing.
func (e *Engine) checkAppDir(ctx context.Context) error {
	return e.runAppDirCommand(ctx, checkAppDirCommand(e.names(), e.Spec.Name))
}

func (e *Engine) runAppDirCommand(ctx context.Context, command string) error {
	n := e.names()
	res, err := e.T.Run(ctx, command)
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

// checkAppDirCommand accepts a state directory that is absent, provably empty,
// or marked as this application's, and changes nothing. An existing directory
// it cannot read counts as not empty.
func checkAppDirCommand(n app.Names, application string) string {
	dir, marker := q(n.AppDir()), q(n.AppMarker())
	return "if [ -e " + marker + " ]; then owner=$(cat " + marker + ") || exit 1; " +
		"[ \"$owner\" = " + q(application) + " ] || { printf '%s' \"$owner\"; exit " + fmt.Sprint(appDirForeign) + "; }; " +
		"elif [ -e " + dir + " ] || [ -L " + dir + " ]; then " +
		"[ -d " + dir + " ] && [ -r " + dir + " ] && [ -x " + dir + " ] || exit " + fmt.Sprint(appDirUnmarked) + "; " +
		"entries=$(ls -A " + dir + ") || exit " + fmt.Sprint(appDirUnmarked) + "; " +
		"[ -z \"$entries\" ] || exit " + fmt.Sprint(appDirUnmarked) + "; fi"
}

// claimAppDirCommand runs the same check, then creates the directory. The
// marker is written only when it is missing, through a temporary file and a
// rename: it is the one proof of ownership destroy trusts, so no interrupted
// write may ever leave it empty.
func claimAppDirCommand(n app.Names, application string) string {
	marker := q(n.AppMarker())
	staged := q(n.AppMarker()+".tmp.") + "$$"
	// The marker first, then everything else: a claim cut short must never
	// leave a non-empty directory without it.
	return checkAppDirCommand(n, application) + "; mkdir -p " + q(n.AppDir()) + " || exit 1; " +
		"[ -e " + marker + " ] || { printf '%s\\n' " + q(application) + " > " + staged + " && mv -f " + staged + " " + marker + "; } || exit 1; " +
		"mkdir -p " + q(n.ReleasesDir())
}
