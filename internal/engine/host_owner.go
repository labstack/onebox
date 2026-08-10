package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/proxy"
)

type HostOwnerMismatchError struct {
	Expected string
	Actual   string
}

func (e *HostOwnerMismatchError) Error() string {
	return fmt.Sprintf("host is owned by application %q; application %q cannot mutate it", e.Actual, e.Expected)
}

func (e *HostOwnerMismatchError) Code() string { return "host_owner_mismatch" }

func (e *Engine) readHostOwner(ctx context.Context) (string, error) {
	path := proxy.HostPaths(e.names()).Owner
	result, err := e.T.Run(ctx, "cat "+q(path)+" 2>/dev/null || true")
	if err != nil {
		return "", err
	}
	owner := strings.TrimSpace(result.Stdout)
	if owner != "" && !appNameRe.MatchString(owner) {
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
		return &HostOwnerMismatchError{Expected: e.Spec.Name, Actual: owner}
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
		return &HostOwnerMismatchError{Expected: e.Spec.Name, Actual: owner}
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
		return &HostOwnerMismatchError{Expected: e.Spec.Name, Actual: owner}
	}
	if owner == e.Spec.Name {
		return nil
	}
	path := proxy.HostPaths(e.names()).Owner
	result, err := e.hostMutate(ctx, "install -m 600 /dev/null "+q(path)+" && printf '%s\\n' "+q(e.Spec.Name)+" > "+q(path))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("record host owner: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}
