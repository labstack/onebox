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

func (e *Engine) readHostOwner(ctx context.Context) (string, error) {
	path := proxy.HostPaths(e.names()).Owner
	result, err := e.T.Run(ctx, app.HostOwnerProbe(path))
	if err != nil {
		return "", err
	}
	if result.ExitCode == app.ProbeAbsent {
		return "", nil
	}
	// Exits 2, 4, 5 and 6 are deliberate refusals, not failed reads: the probe
	// writes nothing to stderr, so falling through would report them as
	// empty errors while preflight names each and offers a remedy. Keep this
	// list in step with the probe — a refusal that falls through here reads
	// as an unexplained failure on the path that decides host ownership.
	if result.ExitCode == app.ProbeUnreadable {
		return "", fmt.Errorf("host owner record %s exists but could not be read; verify the record's permissions, then retry", path)
	}
	if result.ExitCode == app.ProbeStatePathNotDirectory {
		return "", fmt.Errorf("the path that should hold host owner record %s is not a directory; inspect the host state directory", path)
	}
	if result.ExitCode == app.ProbeNotRegular {
		return "", fmt.Errorf("host owner record %s is not a regular file; inspect the host state directory, only a regular file is a valid owner record", path)
	}
	if result.ExitCode == app.ProbeUndetermined {
		return "", fmt.Errorf("the host state directory holding %s cannot be searched, so an owner record cannot be ruled out; verify access, then retry", path)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("read host owner record %s failed (exit %d): %s", path, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	owner := strings.TrimSpace(result.Stdout)
	if !appNameRe.MatchString(owner) {
		// An empty record is the reachable case: a claim interrupted between
		// the noclobber open and the write leaves a zero-byte file, and from
		// then on every command fails here — including bootstrap, which would
		// otherwise re-claim. No ob command clears it, so the remedy has to
		// say what does.
		if owner == "" {
			return "", fmt.Errorf("host owner record %s is present but empty, which no ob command can repair; remove it on the host, then run `ob bootstrap`", path)
		}
		return "", fmt.Errorf("host owner record %s is invalid; inspect it before retrying", path)
	}
	return owner, nil
}

func (e *Engine) RequireHostOwner(ctx context.Context) error {
	owner, err := e.readHostOwner(ctx)
	if err != nil {
		return err
	}
	if owner == "" {
		return fmt.Errorf("host has no Onebox application owner; run `ob bootstrap` for %q first", e.Spec.Name)
	}
	if owner != e.Spec.Name {
		return &HostOwnerMismatchError{Requesting: e.Spec.Name, Owner: owner}
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
	if owner != "" && owner != e.Spec.Name {
		return &HostOwnerMismatchError{Requesting: e.Spec.Name, Owner: owner}
	}
	if owner == e.Spec.Name {
		return nil
	}
	if err := e.acquireHostLock(ctx, e.Opts.ForceLock); err != nil {
		return err
	}
	defer e.releaseHostLock(ctx)
	owner, err = e.readHostOwner(ctx)
	if err != nil {
		return err
	}
	if owner != "" && owner != e.Spec.Name {
		return &HostOwnerMismatchError{Requesting: e.Spec.Name, Owner: owner}
	}
	if owner == e.Spec.Name {
		return nil
	}
	path := proxy.HostPaths(e.names()).Owner
	result, err := e.hostMutate(ctx, "umask 077 && set -C && printf '%s\\n' "+q(e.Spec.Name)+" > "+q(path))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("record host owner: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}
