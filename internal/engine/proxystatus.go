package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/onebox/internal/proxy"
)

// renewalFloor: lego renews 30 days before expiry; a cert inside 21 days
// means renewal has been failing for over a week — an incident, not a warning.
const renewalFloorDays = 21

// proxyRaw is the managed proxy's status reads, gathered concurrently with the
// app-side reads.
type proxyRaw struct {
	ids       []string
	health    string // proxy container health (single-id inspect, not batched)
	applied   string // config hash the host applied
	apps      string // raw `ls` of the registered-apps dir
	acme      string // raw acme.json (parsed at render; keys never leave, design §07)
	localHash string // hash of the locally staged config (computed offline)
}

// proxyReads returns the managed proxy's independent status reads as thunks for
// gather — the host round trips (container+health, config hash, apps, acme) run
// concurrently with the app-side reads, and the local config hash is computed
// in the same wave. Each thunk writes a distinct proxyRaw field, so they share
// no state. The proxy is a single container, so its health uses a plain
// single-id inspect (unlike the app side, which reads health from docker ps).
func (e *Engine) proxyReads(ctx context.Context, px *proxyRaw) []func() error {
	hp := proxy.HostPaths()
	return []func() error{
		func() error {
			ids, err := e.proxyContainerIDs(ctx)
			if err != nil {
				return err
			}
			px.ids = ids
			if len(ids) > 0 {
				px.health, err = e.healthOf(ctx, ids[0])
			}
			return err
		},
		func() error {
			res, err := e.T.Run(ctx, "cat "+q(hp.Hash)+" 2>/dev/null || true")
			if err == nil {
				px.applied = strings.TrimSpace(res.Stdout)
			}
			return err
		},
		func() error {
			res, err := e.T.Run(ctx, "ls -1 "+q(hp.Apps)+" 2>/dev/null || true")
			if err == nil {
				px.apps = res.Stdout
			}
			return err
		},
		func() error {
			res, err := e.T.Run(ctx, "cat "+q(hp.Acme+"/acme.json")+" 2>/dev/null || true")
			if err == nil {
				px.acme = res.Stdout
			}
			return err
		},
		func() error {
			localCfg := e.Cfg.Proxy.Config
			if !filepath.IsAbs(localCfg) {
				localCfg = filepath.Join(e.Opts.LocalDir, localCfg)
			}
			staging, err := os.MkdirTemp("", "ob-proxy-status")
			if err != nil {
				return err
			}
			defer os.RemoveAll(staging)
			px.localHash, err = proxy.Stage(localCfg, staging, e.Cfg.Proxy.Image, e.Cfg.Proxy.Network)
			return err
		},
	}
}

// renderProxy prints the managed proxy under the same recorded-vs-actual
// contract as roles: recorded = the locally staged config hash, actual = what
// the host applied + the container's health + each cert's runway. Divergence
// (absent, unhealthy, drifted config, overdue renewal) returns true. Pure — it
// issues no round trips; everything was gathered concurrently upfront.
func (e *Engine) renderProxy(px proxyRaw) (bool, error) {
	if len(px.ids) == 0 {
		e.ui.Println(fmt.Sprintf("proxy %-12s %s", proxy.ContainerName, e.ui.Warn("NOT RUNNING ⚠ — `ob proxy apply`")))
		return true, nil
	}
	diverged := false
	health := px.health
	if health != "healthy" {
		diverged = true
	}

	state := fmt.Sprintf("config %.8s %s", px.applied, e.ui.OK("(in sync)"))
	if px.applied != px.localHash {
		state = e.ui.Warn(fmt.Sprintf("config DRIFTED ⚠ (local %.8s ≠ applied %.8s) — `ob proxy apply`", px.localHash, px.applied))
		diverged = true
	}
	apps := strings.Join(strings.Fields(px.apps), ", ")
	if apps == "" {
		apps = "(none)"
	}
	fmt.Fprintf(e.Opts.Out, "proxy %-12s %-10s %s   apps: %s\n", proxy.ContainerName, health, state, apps)

	// cert runway — the renewal loop is the proxy's one silent failure mode
	certs, err := proxy.CertExpiries([]byte(px.acme))
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
