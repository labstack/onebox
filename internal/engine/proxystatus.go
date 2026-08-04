package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

// renewalFloor: lego renews 30 days before expiry; a cert inside 21 days
// means renewal has been failing for over a week — an incident, not a warning.
const renewalFloorDays = 21

// proxyRaw is the managed proxy's status reads, gathered concurrently with the
// app-side reads.
type proxyRaw struct {
	ids       []string
	health    string // proxy container health, parsed from docker ps .Status
	applied   string // config hash the host applied
	apps      string // raw `ls` of the registered-apps dir
	acme      string // raw acme.json; parsed at render, and keys never leave
	localHash string // hash of the locally staged config (computed offline)
}

// proxyReads returns the managed proxy's independent status reads as thunks for
// gather — the host round trips (container+health, config hash, apps, acme) run
// concurrently with the app-side reads, and the local config hash is computed
// in the same wave. Each thunk writes a distinct proxyRaw field, so they share
// no state. Health comes from docker ps .Status, same as the app side.
func (e *Engine) proxyReads(ctx context.Context, px *proxyRaw) []func() error {
	hp := proxy.HostPaths()
	return []func() error{
		func() error {
			id, health, err := e.proxyContainer(ctx)
			if err != nil {
				return err
			}
			if id != "" {
				px.ids = []string{id}
			}
			px.health = health
			return nil
		},
		func() error {
			res, err := e.T.Run(ctx, "if [ -r "+q(hp.Hash)+" ]; then cat "+q(hp.Hash)+"; elif [ -e "+q(hp.Hash)+" ]; then exit 2; fi")
			if err == nil {
				px.applied = strings.TrimSpace(res.Stdout)
			}
			return statusReadResult("proxy applied configuration", res, err)
		},
		func() error {
			res, err := e.T.Run(ctx, "if [ -d "+q(hp.Apps)+" ]; then ls -1 "+q(hp.Apps)+"; elif [ -e "+q(hp.Apps)+" ]; then exit 2; fi")
			if err == nil {
				px.apps = res.Stdout
			}
			return statusReadResult("proxy registered applications", res, err)
		},
		func() error {
			path := hp.Acme + "/acme.json"
			res, err := e.T.Run(ctx, "if [ -r "+q(path)+" ]; then cat "+q(path)+"; elif [ -e "+q(path)+" ]; then exit 2; fi")
			if err == nil {
				px.acme = res.Stdout
			}
			return statusReadResult("proxy certificate store", res, err)
		},
		func() error {
			// Empty stays empty: joining it with the project directory would
			// point at the repository root and ask it to be Traefik's config.
			localCfg := e.Spec.Proxy.Config
			if localCfg != "" && !filepath.IsAbs(localCfg) {
				localCfg = filepath.Join(e.Opts.LocalDir, localCfg)
			}
			staging, err := os.MkdirTemp("", "ob-proxy-status")
			if err != nil {
				return err
			}
			defer os.RemoveAll(staging)
			px.localHash, err = proxy.Stage(localCfg, staging, e.Spec.Proxy.Image, e.Spec.Proxy.Network)
			return err
		},
	}
}

func statusReadResult(component string, result transport.Result, err error) error {
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("read %s failed (exit %d): %s", component, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// proxyContainer returns the managed proxy's id and health from ONE docker ps —
// the proxy is a single container, so status doesn't need the id-then-inspect
// two-step (that was the last serial hop in the read wave). Health is parsed
// from .Status like the app side. Empty id when the proxy isn't running.
func (e *Engine) proxyContainer(ctx context.Context) (id, health string, err error) {
	res, err := e.T.Run(ctx, "docker ps --filter label=com.docker.compose.project="+q(proxy.Project)+
		" --format '{{.ID}}|{{.Status}}'")
	if err != nil {
		return "", "", err
	}
	if res.ExitCode != 0 {
		return "", "", fmt.Errorf("proxy docker ps failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	line := strings.SplitN(strings.TrimSpace(res.Stdout), "\n", 2)[0] // single container
	if line == "" {
		return "", "", nil
	}
	id, status, _ := strings.Cut(line, "|")
	if !validID.MatchString(id) {
		return "", "", fmt.Errorf("suspicious proxy container id %q from docker ps", id)
	}
	return id, healthFromStatus(status), nil
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
