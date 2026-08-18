package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/proxy"
)

// HostOwnerMismatchError reports an attempt by one application to mutate a
// host claimed by another application.
type HostOwnerMismatchError struct {
	Requesting string
	Owner      string
}

func (e *HostOwnerMismatchError) Error() string {
	return fmt.Sprintf("host is owned by application %q; application %q cannot mutate it", e.Owner, e.Requesting)
}

func (e *HostOwnerMismatchError) Code() string { return "host_owner_mismatch" }

// HostEnvironmentMismatchError reports one environment of an application trying
// to mutate a host claimed by another environment of the same application.
//
// Nothing else catches this. Every runtime name an application derives — the
// Compose project, container names, volume names — is scoped to the application
// and not to the environment, by design: one box runs one application. So
// staging pointed at production's server passes the application check, passes
// preflight (the names it finds are its own, not a stranger's), and then adopts
// production's containers and volumes. The owner record is the only place the
// difference is visible, which is why it records the environment too.
type HostEnvironmentMismatchError struct {
	Application string
	Requesting  string
	Owner       string
}

func (e *HostEnvironmentMismatchError) Error() string {
	return fmt.Sprintf(
		"host is claimed by the %q environment of application %q; the %q environment cannot mutate it — "+
			"they would share container and volume names",
		e.Owner, e.Application, e.Requesting)
}

func (e *HostEnvironmentMismatchError) Code() string { return "host_environment_mismatch" }

// hostOwner is the parsed owner record: an application, and the environment
// that claimed the host.
//
// A record written before the environment was recorded carries the application
// alone. That is not treated as a failure — it predates the field — but it also
// cannot prove which environment owns the host, so it is upgraded in place the
// next time bootstrap runs. Until then the application check still applies.
type hostOwner struct {
	App         string
	Environment string
}

func (o hostOwner) legacy() bool { return o.Environment == "" }

func parseHostOwner(record string) (hostOwner, bool) {
	// One parser, shared with preflight. Two readings of the same file drift,
	// and the drift showed: preflight read the first two fields and ignored the
	// rest, so a three-field record passed there and failed here.
	parsed, ok := app.ParseHostOwnerRecord(record)
	if !ok {
		return hostOwner{}, false
	}
	return hostOwner{App: parsed.Application, Environment: parsed.Environment}, true
}

func (o hostOwner) record() string {
	if o.legacy() {
		return o.App
	}
	return o.App + " " + o.Environment
}

func (e *Engine) readHostOwner(ctx context.Context) (hostOwner, error) {
	path := proxy.HostPaths(e.names()).Owner
	result, err := e.T.Run(ctx, app.HostOwnerProbe(path))
	if err != nil {
		return hostOwner{}, err
	}
	if result.ExitCode == app.ProbeAbsent {
		return hostOwner{}, nil
	}
	// Exits 2, 4, 5 and 6 are deliberate refusals, not failed reads: the probe
	// writes nothing to stderr, so falling through would report them as
	// empty errors while preflight names each and offers a remedy. Keep this
	// list in step with the probe — a refusal that falls through here reads
	// as an unexplained failure on the path that decides host ownership.
	if result.ExitCode == app.ProbeUnreadable {
		return hostOwner{}, fmt.Errorf("host owner record %s exists but could not be read; verify the record's permissions, then retry", path)
	}
	if result.ExitCode == app.ProbeStatePathNotDirectory {
		return hostOwner{}, fmt.Errorf("the path that should hold host owner record %s is not a directory; inspect the host state directory", path)
	}
	if result.ExitCode == app.ProbeNotRegular {
		return hostOwner{}, fmt.Errorf("host owner record %s is not a regular file; inspect the host state directory, only a regular file is a valid owner record", path)
	}
	if result.ExitCode == app.ProbeUndetermined {
		return hostOwner{}, fmt.Errorf("the host state directory holding %s cannot be searched, so an owner record cannot be ruled out; verify access, then retry", path)
	}
	if result.ExitCode != 0 {
		return hostOwner{}, fmt.Errorf("read host owner record %s failed (exit %d): %s", path, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	record := strings.TrimSpace(result.Stdout)
	owner, ok := parseHostOwner(record)
	if !ok {
		// An empty record is the reachable case: a claim interrupted between
		// the noclobber open and the write leaves a zero-byte file, and from
		// then on every command fails here — including bootstrap, which would
		// otherwise re-claim. No ob command clears it, so the remedy has to
		// say what does.
		if record == "" {
			return hostOwner{}, fmt.Errorf("host owner record %s is present but empty, which no ob command can repair; remove it on the host, then run `ob bootstrap`", path)
		}
		// Say what repairs it. An unparseable record refuses every mutation for
		// as long as it exists, and no ob command rewrites one it cannot read —
		// the same dead end the empty record above has, which does say so.
		return hostOwner{}, fmt.Errorf("host owner record %s is not a record Onebox wrote, and no ob command can repair it; remove it on the host, then run `ob bootstrap`", path)
	}
	return owner, nil
}

func (e *Engine) RequireHostOwner(ctx context.Context) error {
	owner, err := e.readHostOwner(ctx)
	if err != nil {
		return err
	}
	if owner.App == "" {
		return fmt.Errorf("host has no Onebox application owner; run `ob bootstrap` for %q first", e.Spec.Name)
	}
	if owner.App != e.Spec.Name {
		return &HostOwnerMismatchError{Requesting: e.Spec.Name, Owner: owner.App}
	}
	// A record from before the environment was written down cannot say which
	// environment owns the host, and refusing on that would strand every host
	// claimed by an older ob. The application check still holds; bootstrap
	// upgrades the record when it next runs.
	if owner.legacy() {
		return nil
	}
	if owner.Environment != e.Opts.Environment {
		return &HostEnvironmentMismatchError{
			Application: e.Spec.Name,
			Requesting:  e.Opts.Environment,
			Owner:       owner.Environment,
		}
	}
	return nil
}

// claimHostOwner is bootstrap's only host-ownership transition. It checks for
// a foreign owner before acquiring a lock, then rechecks under the host lock so
// two first-contact attempts cannot both claim the same machine.
func (e *Engine) claimHostOwner(ctx context.Context) error {
	owner, err := e.readHostOwner(ctx)
	if err != nil {
		return err
	}
	if owner.App != "" && owner.App != e.Spec.Name {
		return &HostOwnerMismatchError{Requesting: e.Spec.Name, Owner: owner.App}
	}
	if owner.App == e.Spec.Name && !owner.legacy() {
		if owner.Environment != e.Opts.Environment {
			return &HostEnvironmentMismatchError{
				Application: e.Spec.Name,
				Requesting:  e.Opts.Environment,
				Owner:       owner.Environment,
			}
		}
		return nil
	}
	// Either unclaimed, or claimed by this application under a record that
	// predates the environment field. Both take the lock: the first to write a
	// full record, the second to upgrade one in place.
	if err := e.acquireHostLock(ctx, e.Opts.ForceLock); err != nil {
		return err
	}
	defer e.releaseHostLock(ctx)
	owner, err = e.readHostOwner(ctx)
	if err != nil {
		return err
	}
	if owner.App != "" && owner.App != e.Spec.Name {
		return &HostOwnerMismatchError{Requesting: e.Spec.Name, Owner: owner.App}
	}
	if owner.App == e.Spec.Name && !owner.legacy() {
		if owner.Environment != e.Opts.Environment {
			return &HostEnvironmentMismatchError{
				Application: e.Spec.Name,
				Requesting:  e.Opts.Environment,
				Owner:       owner.Environment,
			}
		}
		return nil
	}
	claim := hostOwner{App: e.Spec.Name, Environment: e.Opts.Environment}
	path := proxy.HostPaths(e.names()).Owner
	// `set -C` refuses to clobber, which is what makes a first claim a race
	// nobody wins twice. Upgrading a legacy record is a rewrite of a file that
	// already exists, so it cannot use the same guard — it runs under the host
	// lock, having just re-read the record it is replacing.
	write := "umask 077 && set -C && printf '%s\\n' " + q(claim.record()) + " > " + q(path)
	if owner.legacy() {
		write = "umask 077 && printf '%s\\n' " + q(claim.record()) + " > " + q(path)
	}
	result, err := e.hostMutate(ctx, write)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("record host owner: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}
