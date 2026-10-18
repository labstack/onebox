package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/labstack/onebox/internal/proxy"
)

type proxyObservation struct {
	Applied                proxy.Identity
	AppliedKnown           bool
	ProxyIDs, DiscoveryIDs []string
	OutputInode            string
	TraefikChanged         bool
}

type proxyAction int

const (
	proxyUnchanged proxyAction = iota
	proxyConverge
	proxyReplace
	proxyApply
)

func decideProxy(o proxyObservation, staged proxy.Identity) proxyAction {
	if !o.AppliedKnown || o.Applied.Config != staged.Config {
		return proxyApply
	}
	if o.TraefikChanged || o.Applied.Compose != staged.Compose || len(o.ProxyIDs) == 0 || len(o.DiscoveryIDs) == 0 || o.OutputInode == "" {
		if o.TraefikChanged {
			return proxyReplace
		}
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
	case proxyReplace:
		return "recreate Traefik (its container definition changed)"
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

func traefikDefinitionChanged(live, rendered []byte) bool {
	if len(strings.TrimSpace(string(live))) == 0 {
		return true
	}
	var before, after struct {
		Services map[string]any
		Networks any
	}
	if yaml.Unmarshal(live, &before) != nil || yaml.Unmarshal(rendered, &after) != nil {
		return true
	}
	return !reflect.DeepEqual(before.Services["proxy"], after.Services["proxy"]) ||
		!reflect.DeepEqual(before.Networks, after.Networks)
}

func (e *Engine) observeProxy(ctx context.Context, rendered []byte) (proxyObservation, error) {
	var o proxyObservation
	hp := proxy.HostPaths(e.names())
	marker, err := e.T.Run(ctx, "cat "+q(hp.Hash))
	if err != nil {
		return o, err
	}
	if marker.ExitCode == 0 {
		o.Applied, o.AppliedKnown = proxy.ParseIdentity(marker.Stdout)
	}
	compose, err := e.T.Run(ctx, "cat "+q(hp.Compose))
	if err != nil {
		return o, err
	}
	o.TraefikChanged = o.Applied.Compose == pendingReplaceDigest || compose.ExitCode != 0 || traefikDefinitionChanged([]byte(compose.Stdout), rendered)
	o.ProxyIDs, err = e.proxyContainerIDs(ctx)
	if err != nil {
		return o, err
	}
	if len(o.ProxyIDs) > 0 {
		image := e.Spec.Proxy.Image
		if image == "" {
			image = proxy.DefaultImage
		}
		running, err := e.T.Run(ctx, "docker inspect -f '{{.Image}}' "+q(o.ProxyIDs[0]))
		if err != nil {
			return o, err
		}
		if running.ExitCode != 0 {
			return o, fmt.Errorf("read proxy image: exit %d: %s", running.ExitCode, strings.TrimSpace(running.Stderr))
		}
		local, err := e.T.Run(ctx, "docker image inspect -f '{{.Id}}' "+q(image))
		if err != nil {
			return o, err
		}
		o.TraefikChanged = o.TraefikChanged || local.ExitCode != 0 || strings.TrimSpace(running.Stdout) != strings.TrimSpace(local.Stdout)
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

func proxyPlanLine(o proxyObservation, a proxyAction) string {
	if len(o.ProxyIDs) == 0 {
		return "start Traefik"
	}
	return a.String()
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
	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return "", err
	}
	observed, err := e.observeProxy(ctx, rendered)
	if err != nil {
		return "", err
	}
	return proxyPlanLine(observed, decideProxy(observed, staged)), nil
}
