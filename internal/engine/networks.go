package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

// EnsureApplicationNetwork establishes the external default network every
// release joins. Compose must not own its lifecycle: an unmanaged proxy can
// remain attached while one release is torn down.
func (e *Engine) EnsureApplicationNetwork(ctx context.Context) error {
	n := e.names()
	return e.ensureOwnedNetwork(ctx, n.ApplicationNetwork(), n.ComposeProject(), "")
}

// ensureServiceNetwork establishes the long-lived network shared by workloads
// and supporting services. A legacy state directory is accepted as migration
// evidence because older Onebox versions created this network without labels.
func (e *Engine) ensureServiceNetwork(ctx context.Context, n app.Names) error {
	return e.ensureOwnedNetwork(ctx, n.ServiceNetwork(), "", n.ServiceDir())
}

// ensureOwnedNetwork creates a labelled network or accepts a network whose
// legacy ownership is independently provable. Docker cannot add labels to an
// existing network, and recreating one would sever live endpoints, so legacy
// networks remain intact. A derived name alone is never
// evidence: silently adopting a hand-created network is the bug this boundary
// exists to prevent.
func (e *Engine) ensureOwnedNetwork(ctx context.Context, name, legacyComposeProject, legacyStateDir string) error {
	exists, err := e.ownedNetworkExists(ctx, name, legacyComposeProject, legacyStateDir)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	created, createErr := e.mutate(ctx, "docker network create --label "+q("ob.app="+e.Spec.Name)+" "+q(name))
	if createErr != nil {
		return createErr
	}
	if created.ExitCode != 0 {
		return fmt.Errorf("network %s: cannot create owned network: %s", name, strings.TrimSpace(created.Stderr))
	}
	return nil
}

// removeOwnedNetworks removes the two app-scoped external networks during a
// full destroy. A release teardown leaves them alone; full destruction must
// either remove them or stop before deleting state and releasing host ownership.
func (e *Engine) removeOwnedNetworks(ctx context.Context) error {
	n := e.names()
	networks := []struct {
		name, legacyComposeProject, legacyStateDir string
	}{
		{n.ApplicationNetwork(), n.ComposeProject(), ""},
	}
	// `ob_<app>` is reserved only when the app has services. A project that
	// never declared one must not have full destroy blocked by an unrelated,
	// unlabelled network at that otherwise-unused name. Durable service state
	// also includes the network for projects that removed services from the
	// working declaration before destroying an older installation.
	includeServiceNetwork := len(e.Spec.Services) > 0
	if !includeServiceNetwork {
		state, err := e.T.Run(ctx, "test -d "+q(n.ServiceDir()))
		if err != nil {
			return err
		}
		includeServiceNetwork = state.ExitCode == 0
	}
	if includeServiceNetwork {
		networks = append(networks, struct {
			name, legacyComposeProject, legacyStateDir string
		}{n.ServiceNetwork(), "", n.ServiceDir()})
	}
	for _, network := range networks {
		exists, err := e.ownedNetworkExists(ctx, network.name, network.legacyComposeProject, network.legacyStateDir)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		removed, removeErr := e.mutate(ctx, "docker network rm "+q(network.name))
		if removeErr != nil {
			return removeErr
		}
		if removed.ExitCode != 0 {
			return fmt.Errorf("network %s: cannot remove owned network: %s; detach its remaining endpoints, then retry destroy", network.name, strings.TrimSpace(removed.Stderr))
		}
	}
	return nil
}

// ownedNetworkExists reports absence and otherwise proves that an existing
// network belongs to this application before a caller creates, uses, or removes
// it. The same proof must guard every lifecycle transition.
func (e *Engine) ownedNetworkExists(ctx context.Context, name, legacyComposeProject, legacyStateDir string) (bool, error) {
	// `docker network inspect --format` prints a backslash-t literally on some
	// Docker releases (unlike the list formatter). Use a delimiter that the
	// formatter does not have to interpret; none of these validated identities
	// can contain a pipe.
	inspect := "docker network inspect --format '{{.Id}}|{{index .Labels \"ob.app\"}}|{{index .Labels \"com.docker.compose.project\"}}' " + q(name)
	res, err := e.T.Run(ctx, inspect)
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		message := strings.ToLower(strings.TrimSpace(res.Stderr))
		missing := strings.Contains(message, "no such network") ||
			strings.Contains(message, "network "+strings.ToLower(name)+" not found")
		if missing {
			return false, nil
		}
		return false, fmt.Errorf("network %s: cannot inspect ownership (exit %d): %s", name, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	fields := strings.SplitN(strings.TrimSpace(res.Stdout), "|", 3)
	if len(fields) == 0 || !validID.MatchString(strings.TrimSpace(fields[0])) {
		return false, fmt.Errorf("network %s: inspect returned no valid identity", name)
	}
	owner := ""
	if len(fields) > 1 {
		owner = networkLabel(fields[1])
	}
	if owner != "" {
		if owner != e.Spec.Name {
			return false, fmt.Errorf("network %s is owned by application %s; refusing to adopt it", name, owner)
		}
		return true, nil
	}

	legacyOwned := false
	if len(fields) > 2 && legacyComposeProject != "" {
		legacyOwned = networkLabel(fields[2]) == legacyComposeProject
	}
	if !legacyOwned && legacyStateDir != "" {
		state, stateErr := e.T.Run(ctx, "test -d "+q(legacyStateDir))
		if stateErr != nil {
			return false, stateErr
		}
		legacyOwned = state.ExitCode == 0
	}
	if !legacyOwned {
		return false, fmt.Errorf("network %s exists without Onebox ownership; refusing to adopt it", name)
	}

	return true, nil
}

func networkLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "<no value>" {
		return ""
	}
	return value
}
