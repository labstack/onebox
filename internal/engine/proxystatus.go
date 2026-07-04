package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/yeet/internal/proxy"
)

// renewalFloor: lego renews 30 days before expiry; a cert inside 21 days
// means renewal has been failing for over a week — an incident, not a warning.
const renewalFloorDays = 21

// proxyStatus reports the managed proxy under the same recorded-vs-actual
// contract as roles: recorded = the locally staged config hash, actual = what
// the host applied + the container's health + each cert's runway. Divergence
// (absent, unhealthy, drifted config, overdue renewal) is an error exit.
// Only domain + expiry are read out of acme.json — the keys never leave the
// parse (design §07).
func (e *Engine) proxyStatus(ctx context.Context) (bool, error) {
	hp := proxy.HostPaths()
	diverged := false

	ids, err := e.proxyContainerIDs(ctx)
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		e.ui.Println(fmt.Sprintf("proxy %-12s %s", proxy.ContainerName, e.ui.Warn("NOT RUNNING ⚠ — `yeet proxy apply`")))
		return true, nil
	}
	health, err := e.healthOf(ctx, ids[0])
	if err != nil {
		return false, err
	}
	if health != "healthy" {
		diverged = true
	}

	// recorded vs actual config
	localCfg := e.Cfg.Proxy.Config
	if !filepath.IsAbs(localCfg) {
		localCfg = filepath.Join(e.Opts.LocalDir, localCfg)
	}
	staging, err := os.MkdirTemp("", "yeet-proxy-status")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(staging)
	localHash, err := proxy.Stage(localCfg, staging, e.Cfg.Proxy.Image, e.Cfg.Proxy.Network)
	if err != nil {
		return false, err
	}
	res, err := e.T.Run(ctx, "cat "+q(hp.Hash)+" 2>/dev/null || true")
	if err != nil {
		return false, err
	}
	applied := strings.TrimSpace(res.Stdout)
	state := fmt.Sprintf("config %.8s %s", applied, e.ui.OK("(in sync)"))
	if applied != localHash {
		state = e.ui.Warn(fmt.Sprintf("config DRIFTED ⚠ (local %.8s ≠ applied %.8s) — `yeet proxy apply`", localHash, applied))
		diverged = true
	}

	res, err = e.T.Run(ctx, "ls -1 "+q(hp.Apps)+" 2>/dev/null || true")
	if err != nil {
		return false, err
	}
	apps := strings.Join(strings.Fields(res.Stdout), ", ")
	if apps == "" {
		apps = "(none)"
	}
	fmt.Fprintf(e.Opts.Out, "proxy %-12s %-10s %s   apps: %s\n", proxy.ContainerName, health, state, apps)

	// cert runway — the renewal loop is the proxy's one silent failure mode
	res, err = e.T.Run(ctx, "cat "+q(hp.Acme+"/acme.json")+" 2>/dev/null || true")
	if err != nil {
		return false, err
	}
	certs, err := proxy.CertExpiries([]byte(res.Stdout))
	if err != nil {
		fmt.Fprintf(e.Opts.Out, "  cert store unreadable ⚠ (%v)\n", err)
		return true, nil
	}
	for _, c := range certs {
		days := int(c.NotAfter.Sub(e.Opts.Now()).Hours() / 24)
		mark := ""
		if days < renewalFloorDays {
			mark = e.ui.Warn("  RENEWAL OVERDUE ⚠ (lego renews at 30d — check CF_DNS_API_TOKEN / proxy logs)")
			diverged = true
		}
		e.ui.Println(fmt.Sprintf("  cert %-20s expires %s %s%s", c.Domain, c.NotAfter.UTC().Format("2006-01-02"), e.ui.Dim(fmt.Sprintf("(%dd)", days)), mark))
	}
	return diverged, nil
}
