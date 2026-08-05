package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/proxy"

	"github.com/labstack/onebox/internal/app"
)

// appNameRe mirrors config's app-name rule: registry entries under
// _host/proxy/apps/ are app names; anything else is never interpolated back
// into a shell command (injection rule).
var appNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// EnsureProxy converges the HOST-scoped managed proxy (design: one Traefik
// per host, shared by every ob app on it). Idempotent and ACME-safe: an
// unchanged proxy is never touched; a compose change recreates the container
// (`up -d`); a config-only change uploads + restarts (static config reloads
// only on restart). Divergent config across registered apps is a named
// conflict, not last-writer-wins.
func (e *Engine) EnsureProxy(ctx context.Context, deployID string, force bool) error {
	if !e.Spec.Proxy.Managed {
		return nil
	}
	hp := proxy.HostPaths(e.names())
	// Empty stays empty: joining it with the project directory would point at
	// the repository root and ask it to be Traefik's config.
	localCfg := e.Spec.Proxy.Config
	if localCfg != "" && !filepath.IsAbs(localCfg) {
		localCfg = filepath.Join(e.Opts.LocalDir, localCfg)
	}
	staging, err := os.MkdirTemp("", "ob-proxy")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	hash, err := proxy.Stage(localCfg, staging, e.Spec.Proxy.Image, e.Spec.Proxy.Network)
	if err != nil {
		return err
	}
	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return err
	}

	// Lock order is safe by construction: every acquirer holds either the
	// host lock alone (proxy apply) or its OWN app lock first (bootstrap) —
	// two apps never contend on an app lock, so no cycle exists.
	if err := e.acquireHostLock(ctx, force); err != nil {
		return err
	}
	defer e.releaseHostLock(ctx)
	if res, err := e.hostMutate(ctx, "find "+q(hp.Dir)+" -mindepth 1 -maxdepth 1 -type d -name '.staged-*' -exec rm -rf -- {} + 2>/dev/null || true"); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("clean stale proxy staging: %v %s", err, strings.TrimSpace(res.Stderr))
	}

	acmeJSON := hp.Acme + "/acme.json"
	if res, err := e.hostMutate(ctx, "mkdir -p "+q(hp.Apps)+" "+q(hp.Acme)+" && { test -f "+q(acmeJSON)+" || (touch "+q(acmeJSON)+" && chmod 600 "+q(acmeJSON)+"); }"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("host proxy dirs: %s", res.Stderr)
	}

	// cross-app conflict check: every registered app must agree on the config
	if err := e.proxyConflict(ctx, hp, hash, force); err != nil {
		return err
	}

	res, err := e.T.Run(ctx, "cat "+q(hp.Hash)+" 2>/dev/null || true")
	if err != nil {
		return err
	}
	remoteHash := strings.TrimSpace(res.Stdout)
	ids, err := e.proxyContainerIDs(ctx)
	if err != nil {
		return err
	}

	jw := &journal.Writer{T: e.T, Names: app.Names{App: app.HostNamespace, BasePath: e.names().BasePath}, DeployID: deployID, Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner}

	if remoteHash == hash && len(ids) > 0 {
		e.logf("proxy: unchanged and running — not touched")
		return e.registerProxyApp(ctx, hp, hash)
	}

	res, err = e.T.Run(ctx, "cat "+q(hp.Compose)+" 2>/dev/null || true")
	if err != nil {
		return err
	}
	remoteCompose := res.Stdout
	composeChanged := strings.TrimSpace(remoteCompose) != strings.TrimSpace(string(rendered))
	if composeChanged && remoteCompose != "" {
		diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: difflib.SplitLines(remoteCompose), B: difflib.SplitLines(string(rendered)),
			FromFile: "live proxy compose", ToFile: "planned", Context: 2,
		})
		e.ui.Diff(diff)
	}
	if remoteHash != "" && remoteHash != hash {
		// config content is never diffed or printed — .env may hold secrets;
		// only hashes travel
		e.logf("proxy: config changed (%.8s → %.8s)", remoteHash, hash)
	}

	_ = jw.Append(ctx, journal.Record{Phase: "proxy-apply", Event: "start", Detail: "hash=" + hash})
	fail := func(detail string) {
		_ = jw.Append(ctx, journal.Record{Phase: "proxy-apply", Event: "finish", Status: "fail", Detail: detail})
	}

	if remoteHash != hash || remoteCompose == "" {
		// Stage remotely, then swap: the live container bind-mounts ConfigDir,
		// so it must never see a half-written dir for the (seconds-long) upload
		// window — only for the instant of the rm+mv. Stale files can't linger
		// either (upload is additive tar; the swap replaces the whole dir).
		stagedDir := hp.Dir + "/.staged-" + e.hostLockToken
		defer func() {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = e.hostMutate(cleanupContext, "rm -rf "+q(stagedDir))
		}()
		if res, err := e.hostMutate(ctx, "rm -rf "+q(stagedDir)); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("clear proxy staging: %v %s", err, res.Stderr)
		}
		if err := e.T.Upload(ctx, staging, stagedDir); err != nil {
			return err
		}
		swap := "rm -rf " + q(hp.ConfigDir) +
			" && mv " + q(stagedDir+"/config") + " " + q(hp.ConfigDir) +
			" && mv -f " + q(stagedDir+"/compose.yaml") + " " + q(hp.Compose) +
			" && rm -rf " + q(stagedDir)
		if res, err := e.hostMutate(ctx, swap); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("swap proxy config: %v %s", err, res.Stderr)
		}
	}

	// Converge by observation, not by disk state (a crash may have left the
	// files new but the container old): `up -d` recreates only when the
	// running container diverges from the compose file; if it didn't recreate,
	// the change was config-only — restart so the static config reloads.
	before := idSet(ids)
	e.logf("proxy: converging")
	if res, err := e.hostMutate(ctx, "docker compose -p "+proxy.Project+" -f "+q(hp.Compose)+" up -d"); err != nil {
		fail("")
		return err
	} else if res.ExitCode != 0 {
		fail(res.Stderr)
		return fmt.Errorf("proxy up: %s", strings.TrimSpace(res.Stderr))
	}
	ids, err = e.proxyContainerIDs(ctx)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fail("no container after converge")
		return fmt.Errorf("proxy: no container after converge")
	}
	if before[ids[0]] {
		e.logf("proxy: container not recreated — restarting %s to load the new config", proxy.ContainerName)
		if res, err := e.hostMutate(ctx, "docker restart "+proxy.ContainerName); err != nil {
			fail("")
			return err
		} else if res.ExitCode != 0 {
			fail(res.Stderr)
			return fmt.Errorf("proxy restart: %s", strings.TrimSpace(res.Stderr))
		}
	}
	if err := e.waitHealth(ctx, ids[0], "healthy", 180*time.Second, 5*time.Second); err != nil {
		fail(err.Error())
		return fmt.Errorf("proxy never became healthy (traefik.yml must enable ping: {}): %w", err)
	}
	// the applied-state marker — written ONLY after health confirms, so an
	// interrupted converge is retried, never mistaken for "unchanged"
	if res, err := e.hostMutate(ctx, "echo "+q(hash)+" > "+q(hp.Hash)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("write proxy hash: %v %s", err, res.Stderr)
	}
	if err := e.registerProxyApp(ctx, hp, hash); err != nil {
		return err
	}
	_ = jw.Append(ctx, journal.Record{Phase: "proxy-apply", Event: "finish", Status: "ok"})
	e.logf("proxy: healthy at config %.8s", hash)
	return nil
}

// proxyConflict enforces the host-scoped agreement rule: every app registered
// in _host/proxy/apps/ must declare the same proxy config. A mismatch names
// the apps and refuses (force proceeds — the operator owns the divergence).
func (e *Engine) proxyConflict(ctx context.Context, hp proxy.Paths, hash string, force bool) error {
	res, err := e.T.Run(ctx, "ls -1 "+q(hp.Apps)+" 2>/dev/null || true")
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(res.Stdout) {
		if name == e.Spec.Name || !appNameRe.MatchString(name) {
			continue
		}
		r, err := e.T.Run(ctx, "cat "+q(hp.Apps+"/"+name)+" 2>/dev/null || true")
		if err != nil {
			return err
		}
		if other := strings.TrimSpace(r.Stdout); other != "" && other != hash {
			if force {
				e.warnf("proxy config diverges from app %q (%.8s vs %.8s) — proceeding (--force); redeploy %q with matching config", name, other, hash, name)
				continue
			}
			return fmt.Errorf("proxy config conflict: app %q registered %.8s, this apply is %.8s — the host proxy is SHARED; align both apps' proxy.config, or --force to make %q the loser",
				name, other, hash, e.Spec.Name)
		}
	}
	return nil
}

func (e *Engine) registerProxyApp(ctx context.Context, hp proxy.Paths, hash string) error {
	res, err := e.hostMutate(ctx, "echo "+q(hash)+" > "+q(hp.Apps+"/"+e.Spec.Name))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("register app with proxy: %s", res.Stderr)
	}
	return nil
}

func (e *Engine) proxyContainerIDs(ctx context.Context) ([]string, error) {
	res, err := e.T.Run(ctx, "docker ps -q --filter label=com.docker.compose.project="+q(proxy.Project))
	if err != nil {
		return nil, err
	}
	return splitIDs(res.Stdout)
}

// ProxyApply is the CLI verb: converge the shared proxy outside any deploy.
func (e *Engine) ProxyApply(ctx context.Context, deployID string, force bool) error {
	if !e.Spec.Proxy.Managed {
		return fmt.Errorf("proxy is not managed (proxy.managed: true enables ob-owned Traefik)")
	}
	return e.EnsureProxy(ctx, deployID, force)
}
