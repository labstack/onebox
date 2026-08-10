package engine

import (
	"context"
	"fmt"
	"strings"

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
	result, err := e.T.Run(ctx, "if [ ! -e "+q(path)+" ]; then exit 3; fi; if [ ! -f "+q(path)+" ] || [ -L "+q(path)+" ]; then exit 4; fi; cat "+q(path))
	if err != nil {
		return "", err
	}
	if result.ExitCode == 3 {
		return "", nil
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("read host owner record %s failed (exit %d): %s", path, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	owner := strings.TrimSpace(result.Stdout)
	if !appNameRe.MatchString(owner) {
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
