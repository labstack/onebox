package engine

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// Bootstrap performs first contact: base directories → the user's bootstrap
// hook (host-specific provisioning — docker install, tailscale, data dirs —
// stays the operator's, config management is a non-goal) → registry login →
// push a release dir → start accessories from it. Never activates; after
// bootstrap every deploy is a pure release.
func (e *Engine) Bootstrap(ctx context.Context, releaseID, localStagingDir string) error {
	// Several registries may be declared; a login is attempted for each that
	// carries credentials, and a named-but-empty password variable is an error
	// rather than a silent anonymous pull that fails later on a private image.
	passwords := map[string]string{}
	for _, name := range sortedNames(e.Spec.Registries) {
		r := e.Spec.Registries[name]
		if r.PasswordEnv == "" {
			continue
		}
		password := os.Getenv(r.PasswordEnv)
		if password == "" {
			return fmt.Errorf("registry %s: env var %s is empty — export the registry password first", name, r.PasswordEnv)
		}
		passwords[name] = password
	}

	// the runtime is ob's own precondition — the one universal piece of
	// host provisioning; bootstrap provisions the runtime.
	// Everything vendor-flavored (VPNs, NFS, kernel tuning) stays in the
	// user's bootstrap hook.
	if res, err := e.T.Run(ctx, "docker version -f '{{.Server.Version}}'"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		e.logf("bootstrap: no container runtime — installing docker (get.docker.com)")
		ires, err := e.T.Run(ctx, "curl -fsSL https://get.docker.com | sh && systemctl enable --now docker")
		if err != nil {
			return err
		}
		if ires.ExitCode != 0 {
			return fmt.Errorf("docker install failed: %s", strings.TrimSpace(ires.Stderr))
		}
	}

	e.logf("bootstrap: base dirs")
	p := release.PathsFor(e.Spec.Name)
	if res, err := e.T.Run(ctx, "mkdir -p "+q(p.Releases)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("mkdir %s: %v %s", p.Releases, err, res.Stderr)
	}

	// one regime for every mutation: bootstrap locks, fences,
	// and journals like a deploy
	epoch, err := e.AcquireLock(ctx, releaseID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, releaseID, epoch); err != nil {
		return err
	}
	jw := &journal.Writer{T: e.T, App: e.Spec.Name, DeployID: releaseID, Epoch: epoch, Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner}
	_ = jw.Append(ctx, journal.Record{Phase: "bootstrap", Event: "start"})
	defer func() { _ = jw.Append(ctx, journal.Record{Phase: "bootstrap", Event: "finish", Status: "ok"}) }()

	remoteDir := p.Releases + "/" + releaseID
	if err := e.RunHook(ctx, "bootstrap", p.Base, remoteDir+"/compose.yaml"); err != nil {
		return fmt.Errorf("bootstrap hook: %w", err)
	}

	for _, name := range sortedNames(e.Spec.Registries) {
		r, password := e.Spec.Registries[name], passwords[name]
		if password == "" {
			continue
		}
		e.logf("bootstrap: registry login %s", r.Server)
		res, err := e.T.RunInput(ctx, "docker login "+q(r.Server)+" -u "+q(r.Username)+" --password-stdin", password+"\n")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("docker login %s failed: %s", r.Server, strings.TrimSpace(res.Stderr))
		}
	}

	// managed proxy before accessories: role containers join its network, and
	// preflight asserts it healthy from the first deploy on. EnsureProxy takes
	// the HOST lock internally (own-app lock is already held — safe order).
	if e.Spec.Proxy.Managed {
		if err := e.EnsureProxy(ctx, releaseID, e.Opts.ForceLock); err != nil {
			return fmt.Errorf("managed proxy: %w", err)
		}
	}

	e.logf("bootstrap: pushing release payload %s", releaseID)
	if _, err := release.Push(ctx, e.T, localStagingDir, e.Spec.Name, releaseID); err != nil {
		return err
	}

	if len(e.Spec.ServiceNames()) > 0 {
		e.logf("bootstrap: starting services %v", e.Spec.ServiceNames())
		if err := e.ApplyServices(ctx); err != nil {
			return fmt.Errorf("services: %w", err)
		}
	}
	e.logf("bootstrap complete — run `ob deploy` for the first release")
	return nil
}
