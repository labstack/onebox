package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/proxy"
)

const pendingComposeDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// EnsureProxy converges the HOST-scoped managed proxy (design: one Traefik
// per host, owned by its sole Onebox application). Idempotent and ACME-safe: an
// unchanged proxy is never touched; a compose change recreates the container
// (`up -d`); a config-only change uploads + restarts (static config reloads
// only on restart).
func (e *Engine) EnsureProxy(ctx context.Context, deployID string, breakLock bool) (err error) {
	if !e.Spec.Proxy.Managed {
		return nil
	}
	hp := proxy.HostPaths(e.names())
	staging, err := os.MkdirTemp("", "onebox-proxy")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	id, err := e.stageProxy(staging)
	if err != nil {
		return err
	}
	hash := id.String()
	rendered, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
	if err != nil {
		return err
	}

	// Lock order is safe by construction: every acquirer holds either the
	// host lock alone (proxy apply) or its own app lock first (bootstrap). A
	// host has one application, so no two acquirers contend on an app lock and
	// no cycle exists.
	if err := e.acquireHostLock(ctx, breakLock); err != nil {
		return err
	}
	defer e.releaseHostLock(ctx)
	applied := false
	defer func() {
		if err != nil || !applied {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.hostMutate(cleanupContext, "find "+q(hp.Dir)+" -mindepth 1 -maxdepth 1 -type d -name '.staged-*' -exec rm -rf -- {} + 2>/dev/null || true")
	}()
	jw := &journal.Writer{T: e.T, Dir: e.names().HostJournalDir(), DeployID: deployID, Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner}
	if err := jw.Append(ctx, journal.Record{Phase: "proxy-apply", Event: "start", Detail: "hash=" + hash}); err != nil {
		return fmt.Errorf("journal proxy apply start: %w", err)
	}
	defer func() {
		finish := journal.Record{Phase: "proxy-apply", Event: "finish", Status: "ok"}
		if err != nil {
			finish.Status = "fail"
			finish.Detail = err.Error()
		}
		if journalErr := jw.Append(ctx, finish); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal proxy apply finish: %w", journalErr))
			return
		}
		e.pruneHostJournal(ctx)
	}()
	acmeJSON := hp.Acme + "/acme.json"
	prepareDirs := "mkdir -p " + q(hp.Acme) + " " + q(hp.Dynamic) +
		" && { test -f " + q(acmeJSON) + " || (touch " + q(acmeJSON) + " && chmod 600 " + q(acmeJSON) + "); }" +
		" && chown 65532:65532 " + q(hp.Acme) + " " + q(acmeJSON) +
		" && chmod 700 " + q(hp.Acme) + " && chmod 600 " + q(acmeJSON)
	if res, err := e.hostMutate(ctx, prepareDirs); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("host proxy dirs: %s", res.Stderr)
	}

	o, err := e.observeProxy(ctx)
	if err != nil {
		return err
	}
	action := decideProxy(o, id)
	switch action {
	case proxyUnchanged:
		e.logf("proxy: unchanged and running — not touched")
		return nil
	}

	res, err := e.T.Run(ctx, "cat "+q(hp.Compose)+" 2>/dev/null || true")
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
	if action == proxyConverge {
		if err := e.commitProxyIdentity(ctx, proxy.Identity{Config: o.Applied.Config, Compose: pendingComposeDigest}); err != nil {
			return err
		}
		composeStaging, err := os.MkdirTemp("", "onebox-proxy-compose")
		if err != nil {
			return err
		}
		defer os.RemoveAll(composeStaging)
		if err := os.WriteFile(filepath.Join(composeStaging, "compose.yaml"), rendered, 0o600); err != nil {
			return err
		}
		staging = composeStaging
	} else {
		if res, err := e.hostMutate(ctx, "rm -f "+q(hp.Hash)); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("invalidate proxy hash: %s", strings.TrimSpace(res.Stderr))
		}
	}
	// Retire the live directories without deleting their contents: Traefik's
	// bind mounts keep serving them until the restart succeeds.
	stagedDir := hp.Dir + "/.staged-" + e.hostLockToken
	res, err = e.hostMutate(ctx, "rm -rf "+q(stagedDir))
	if err != nil {
		return fmt.Errorf("clear proxy staging: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("clear proxy staging: %s", strings.TrimSpace(res.Stderr))
	}
	if err := e.T.Upload(ctx, staging, stagedDir); err != nil {
		return err
	}
	swap := "mkdir -p " + q(stagedDir+"/retired") +
		" && { if [ -d " + q(hp.ConfigDir) + " ]; then mv " + q(hp.ConfigDir) + " " + q(stagedDir+"/retired/config") + "; fi; }" +
		" && { if [ -d " + q(hp.Dynamic) + " ]; then mv " + q(hp.Dynamic) + " " + q(stagedDir+"/retired/dynamic") + "; fi; }" +
		" && mv " + q(stagedDir+"/config") + " " + q(hp.ConfigDir) +
		" && mv " + q(stagedDir+"/dynamic") + " " + q(hp.Dynamic) +
		" && mv -f " + q(stagedDir+"/compose.yaml") + " " + q(hp.Compose) +
		" && chown -R 0:65532 " + q(hp.ConfigDir) + " " + q(hp.Dynamic) +
		" && find " + q(hp.ConfigDir) + " " + q(hp.Dynamic) + " -type d -exec chmod 750 {} +" +
		" && find " + q(hp.ConfigDir) + " " + q(hp.Dynamic) + " -type f -exec chmod 640 {} +"
	if action == proxyConverge {
		swap = "mv -f " + q(stagedDir+"/compose.yaml") + " " + q(hp.Compose)
	}
	res, err = e.hostMutate(ctx, swap)
	if err != nil {
		return fmt.Errorf("swap proxy config: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("swap proxy config: %s", strings.TrimSpace(res.Stderr))
	}

	before := idSet(o.ProxyIDs)
	e.logf("proxy: converging")
	compose := "docker compose -p " + proxy.Project + " -f " + q(hp.Compose)
	// A config swap replaces discovery's bind-mounted directory inode.
	forceDiscovery := action == proxyApply || len(o.DiscoveryIDs) == 0 || o.OutputInode == ""
	previousInode := ""
	if action == proxyConverge {
		if res, err := e.hostMutate(ctx, compose+" stop discovery"); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("proxy discovery stop: %s", strings.TrimSpace(res.Stderr))
		}
		res, err := e.T.Run(ctx, outputInodeCommand(hp.Dynamic+"/onebox.yml"))
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("proxy discovery output: %s", strings.TrimSpace(res.Stderr))
		}
		previousInode = strings.TrimSpace(res.Stdout)
	}
	discoveryUp := compose + " up -d discovery"
	if forceDiscovery {
		discoveryUp = compose + " up -d --force-recreate discovery"
	}
	if res, err := e.hostMutate(ctx, discoveryUp); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("proxy discovery up: %s", strings.TrimSpace(res.Stderr))
	}
	if err := e.waitForPublication(ctx, previousInode); err != nil {
		return err
	}
	if res, err := e.hostMutate(ctx, compose+" up -d proxy"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("proxy up: %s", strings.TrimSpace(res.Stderr))
	}
	ids, err := e.proxyContainerIDs(ctx)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("proxy: no container after converge")
	}
	if action == proxyApply && before[ids[0]] {
		e.logf("proxy: container not recreated — restarting %s to load the new config", proxy.ContainerName)
		if res, err := e.hostMutate(ctx, "docker restart "+proxy.ContainerName); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("proxy restart: %s", strings.TrimSpace(res.Stderr))
		}
	}
	if action == proxyApply || !before[ids[0]] {
		if err := e.waitHealth(ctx, ids[0], "healthy", 180*time.Second, 5*time.Second); err != nil {
			return fmt.Errorf("proxy never became healthy (the Traefik static configuration must enable ping: {}): %w", err)
		}
	}
	if err := e.commitProxyIdentity(ctx, id); err != nil {
		return err
	}
	applied = true
	e.logf("proxy: healthy at config %s compose %s", shortDigest(id.Config), shortDigest(id.Compose))
	return nil
}

func (e *Engine) waitForPublication(ctx context.Context, previousInode string) error {
	path := proxy.HostPaths(e.names()).Dynamic + "/onebox.yml"
	ready := "i=0; while [ $i -lt 30 ]; do inode=$(" + outputInodeCommand(path) +
		") && [ -n \"$inode\" ] && [ \"$inode\" != " + q(previousInode) +
		" ] && exit 0; i=$((i+1)); sleep 1; done; exit 1"
	if res, err := e.T.Run(ctx, ready); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("proxy discovery did not publish routing state; inspect `docker logs %s`", proxy.DiscoveryContainerName)
	}
	return nil
}

func (e *Engine) commitProxyIdentity(ctx context.Context, id proxy.Identity) error {
	path := proxy.HostPaths(e.names()).Hash
	command := "printf '%s\\n' " + q(id.String()) + " > " + q(path+".tmp") +
		" && mv -f " + q(path+".tmp") + " " + q(path)
	if res, err := e.hostMutate(ctx, command); err != nil {
		return fmt.Errorf("write proxy hash: %w", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("write proxy hash: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// pruneHostJournal keeps the host journal to the application journal's window.
// Every proxy check writes to it — an unchanged proxy included — so it runs on
// every one, after the finish record. The host journal holds only proxy
// applies, which nothing recovers from, so it keeps the newest files by name
// and needs no record to be readable: a torn file ages out like any other. It
// is housekeeping, in one round trip: a failure is reported and never turns
// an applied proxy into a failed one.
func (e *Engine) pruneHostJournal(ctx context.Context) {
	keep := e.Spec.Deployment.RetainReleases * 2
	if keep < 1 {
		return
	}
	res, err := e.hostMutate(ctx, pruneHostJournalCommand(e.names().HostJournalDir(), keep))
	if err == nil && res.ExitCode != 0 {
		err = errors.New(strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		e.logf("proxy: host journal not pruned: %v", err)
	}
}

// pruneHostJournalCommand removes all but the newest keep journal files.
// Journal ids begin with a timestamp, so name order is age order.
func pruneHostJournalCommand(dir string, keep int) string {
	return "if [ -d " + q(dir) + " ]; then cd " + q(dir) + " || exit 1; " +
		"ls -1 | grep '\\.jsonl$' | sort -r | sed '1," + strconv.Itoa(keep) + "d' | " +
		"while IFS= read -r f; do rm -f -- \"$f\" || exit 1; done; fi"
}

func (e *Engine) proxyContainerIDs(ctx context.Context) ([]string, error) {
	res, err := e.T.Run(ctx, "docker ps -q --filter label=com.docker.compose.project="+q(proxy.Project)+
		" --filter label=com.docker.compose.service=proxy")
	if err != nil {
		return nil, err
	}
	return splitIDs(res.Stdout)
}

func (e *Engine) discoveryContainerIDs(ctx context.Context) ([]string, error) {
	res, err := e.T.Run(ctx, "docker ps -q --filter label=com.docker.compose.project="+q(proxy.Project)+
		" --filter label=com.docker.compose.service=discovery")
	if err != nil {
		return nil, err
	}
	return splitIDs(res.Stdout)
}

// ProxyApply is the CLI verb: converge the host proxy outside any deploy.
func (e *Engine) ProxyApply(ctx context.Context, deployID string) error {
	if !e.Spec.Proxy.Managed {
		return fmt.Errorf("proxy is not managed (proxy.managed: true enables Onebox-owned Traefik)")
	}
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	return e.EnsureProxy(ctx, deployID, e.Opts.ForceLock)
}
