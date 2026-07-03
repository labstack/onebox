package engine

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/release"
)

// Bootstrap is first contact (design §03): base dirs → the user's bootstrap
// hook (host-specific provisioning — docker install, tailscale, data dirs —
// stays the operator's, config management is a non-goal) → registry login →
// push a release dir → start accessories from it. Never activates; after
// bootstrap every deploy is a pure release.
func (e *Engine) Bootstrap(ctx context.Context, releaseID, localStagingDir string) error {
	var password string
	if r := e.Cfg.Registry; r != nil {
		password = os.Getenv(r.PasswordEnv)
		if password == "" {
			return fmt.Errorf("registry: env var %s is empty — export the registry password first", r.PasswordEnv)
		}
	}

	e.logf("bootstrap: base dirs")
	p := release.PathsFor(e.Cfg.App)
	if res, err := e.T.Run(ctx, "mkdir -p "+q(p.Releases)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("mkdir %s: %v %s", p.Releases, err, res.Stderr)
	}

	// one regime for every mutation (design §05): bootstrap locks, fences,
	// and journals like a deploy
	epoch, err := e.AcquireLock(ctx, releaseID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, releaseID, epoch); err != nil {
		return err
	}
	jw := &journal.Writer{T: e.T, App: e.Cfg.App, DeployID: releaseID, Epoch: epoch, Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash}
	_ = jw.Append(ctx, journal.Record{Phase: "bootstrap", Event: "start"})
	defer func() { _ = jw.Append(ctx, journal.Record{Phase: "bootstrap", Event: "finish", Status: "ok"}) }()

	remoteDir := p.Releases + "/" + releaseID
	if err := e.RunHook(ctx, "bootstrap", p.Base, remoteDir+"/compose.yaml"); err != nil {
		return fmt.Errorf("bootstrap hook: %w", err)
	}

	if r := e.Cfg.Registry; r != nil {
		e.logf("bootstrap: registry login %s", r.Server)
		res, err := e.T.RunInput(ctx, "docker login "+r.Server+" -u "+r.Username+" --password-stdin", password+"\n")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("docker login %s failed: %s", r.Server, strings.TrimSpace(res.Stderr))
		}
	}

	e.logf("bootstrap: pushing release payload %s", releaseID)
	pushed, err := release.Push(ctx, e.T, localStagingDir, e.Cfg.App, releaseID)
	if err != nil {
		return err
	}

	if len(e.Cfg.Accessories) > 0 {
		e.logf("bootstrap: starting accessories %v", e.Cfg.Accessories)
		cc := e.composeCmd(pushed + "/compose.yaml")
		args := strings.Join(e.Cfg.Accessories, " ")
		if res, err := e.mutate(ctx, cc+" up -d --no-deps --no-recreate "+args); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("accessories up: %s", res.Stderr)
		}
	}
	e.logf("bootstrap complete — run `yeet deploy` for the first release")
	return nil
}
