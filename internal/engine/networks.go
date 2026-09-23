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
	return e.ensureOwnedNetwork(ctx, n.ApplicationNetwork())
}

// ensureServiceNetwork establishes the long-lived network shared by workloads
// and supporting services.
func (e *Engine) ensureServiceNetwork(ctx context.Context, n app.Names) error {
	return e.ensureOwnedNetwork(ctx, n.ServiceNetwork())
}

// ensureOwnedNetwork creates a labelled network or accepts one this
// application provably owns: it carries the application's label, or
// app.Names.ComposeCreatedApplicationNetwork says Compose created it for the
// application's own project. A derived name alone is never evidence: silently
// adopting a hand-created network is the bug this boundary exists to prevent.
func (e *Engine) ensureOwnedNetwork(ctx context.Context, name string) error {
	exists, err := e.ownedNetworkExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	created, createErr := e.mutate(ctx, "docker network create --label "+q("onebox.app="+e.Spec.Name)+" "+q(name))
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
	networks := []string{n.ApplicationNetwork()}
	// `onebox_services` is reserved only when the app has services. A project that
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
		networks = append(networks, n.ServiceNetwork())
	}
	for _, network := range networks {
		exists, err := e.ownedNetworkExists(ctx, network)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		removed, removeErr := e.mutate(ctx, "docker network rm "+q(network))
		if removeErr != nil {
			return removeErr
		}
		if removed.ExitCode != 0 {
			return fmt.Errorf("network %s: cannot remove owned network: %s; detach its remaining endpoints, then retry destroy", network, strings.TrimSpace(removed.Stderr))
		}
	}
	return nil
}

// ownedNetworkExists reports absence and otherwise proves that an existing
// network belongs to this application before a caller creates, uses, or removes
// it. The same proof must guard every lifecycle transition.
func (e *Engine) ownedNetworkExists(ctx context.Context, name string) (bool, error) {
	// `docker network inspect --format` prints a backslash-t literally on some
	// Docker releases (unlike the list formatter). Use a delimiter that the
	// formatter does not have to interpret; none of these validated identities
	// can contain a pipe.
	inspect := "docker network inspect --format '{{.Id}}|{{index .Labels \"onebox.app\"}}|{{index .Labels \"com.docker.compose.project\"}}' " + q(name)
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
	if owner == "" && len(fields) > 2 && e.names().ComposeCreatedApplicationNetwork(name, networkLabel(fields[2])) {
		owner = e.Spec.Name
	}
	switch owner {
	case "":
		return false, fmt.Errorf("network %s exists without Onebox ownership; refusing to adopt it", name)
	case e.Spec.Name:
		return true, nil
	default:
		return false, fmt.Errorf("network %s is owned by application %s; refusing to adopt it", name, owner)
	}
}

func networkLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "<no value>" {
		return ""
	}
	return value
}
