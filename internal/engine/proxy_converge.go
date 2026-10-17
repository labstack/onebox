package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/onebox/internal/proxy"
)

type proxyObservation struct {
	Applied                proxy.Identity
	AppliedKnown           bool
	ProxyIDs, DiscoveryIDs []string
	OutputInode            string
}

type proxyAction int

const (
	proxyUnchanged proxyAction = iota
	proxyConverge
	proxyApply
)

func decideProxy(o proxyObservation, staged proxy.Identity) proxyAction {
	if !o.AppliedKnown || o.Applied.Config != staged.Config {
		return proxyApply
	}
	if o.Applied.Compose != staged.Compose || len(o.ProxyIDs) == 0 || len(o.DiscoveryIDs) == 0 || o.OutputInode == "" {
		return proxyConverge
	}
	return proxyUnchanged
}

func (a proxyAction) String() string {
	switch a {
	case proxyUnchanged:
		return "unchanged"
	case proxyConverge:
		return "update changed containers only, no restart"
	case proxyApply:
		return "restart Traefik (configuration changed)"
	default:
		return "unknown"
	}
}

func (e *Engine) stageProxy(dir string) (proxy.Identity, error) {
	// Empty stays empty: the project directory is not Traefik's config.
	localCfg := e.Spec.Proxy.Config
	if localCfg != "" && !filepath.IsAbs(localCfg) {
		localCfg = filepath.Join(e.Opts.LocalDir, localCfg)
	}
	discoveryImage := proxy.DiscoveryImage(e.Opts.Runner.Version)
	return proxy.StageForAppManaged(localCfg, dir, e.Spec.Proxy.Image, discoveryImage,
		e.Spec.Name, e.Spec.Proxy.Network, e.Spec.Proxy.Entrypoints, e.Spec.HasExactTerminatingTLS(),
		e.Spec.HasWildcardTerminatingTLS(), e.Spec.Proxy.DNSChallenge)
}

func (e *Engine) observeProxy(ctx context.Context) (proxyObservation, error) {
	var o proxyObservation
	hp := proxy.HostPaths(e.names())
	marker, err := e.T.Run(ctx, "cat "+q(hp.Hash))
	if err != nil {
		return o, err
	}
	if marker.ExitCode == 0 {
		o.Applied, o.AppliedKnown = proxy.ParseIdentity(marker.Stdout)
	}
	o.ProxyIDs, err = e.proxyContainerIDs(ctx)
	if err != nil {
		return o, err
	}
	o.DiscoveryIDs, err = e.discoveryContainerIDs(ctx)
	if err != nil {
		return o, err
	}
	output, err := e.T.Run(ctx, outputInodeCommand(hp.Dynamic+"/onebox.yml"))
	if err != nil {
		return o, err
	}
	if output.ExitCode != 0 {
		return o, fmt.Errorf("read proxy output inode: exit %d: %s", output.ExitCode, strings.TrimSpace(output.Stderr))
	}
	o.OutputInode = strings.TrimSpace(output.Stdout)
	return o, nil
}

func outputInodeCommand(path string) string {
	return "if [ -s " + q(path) + " ]; then stat -c %i " + q(path) + "; fi"
}

func (e *Engine) PlanProxy(ctx context.Context) (string, error) {
	if !e.Spec.Proxy.Managed {
		return "", nil
	}
	staging, err := os.MkdirTemp("", "onebox-proxy")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	staged, err := e.stageProxy(staging)
	if err != nil {
		return "", err
	}
	observed, err := e.observeProxy(ctx)
	if err != nil {
		return "", err
	}
	return decideProxy(observed, staged).String(), nil
}
