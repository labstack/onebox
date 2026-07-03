package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/release"
)

// Deploy runs the lifecycle under the full trust regime (design §05):
// lock → fence → journal every phase → finish. Every mutating command is
// fence-guarded; a zombie runner dies host-side with ErrFenced.
func (e *Engine) Deploy(ctx context.Context, releaseID, localStagingDir string) error {
	return e.deployCore(ctx, releaseID, localStagingDir, nil)
}

// deployCore: done != nil means resume — completed steps skip; staging may be
// empty (the release dir already lives on the host).
func (e *Engine) deployCore(ctx context.Context, releaseID, localStagingDir string, done map[string]bool) error {
	e.logf("phase preflight")
	if err := e.Preflight(ctx); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	prev, err := release.Current(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}

	epoch, err := e.AcquireLock(ctx, releaseID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	if err := e.WriteFence(ctx, releaseID, epoch); err != nil {
		return err
	}
	stopHB := e.StartHeartbeat(ctx)
	defer stopHB()

	jw := &journal.Writer{
		T: e.T, App: e.Cfg.App, DeployID: releaseID, Epoch: epoch,
		Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash,
	}
	_ = jw.Append(ctx, journal.Record{Phase: "deploy", Event: "start", Detail: "prev=" + prev})

	err = e.runPhases(ctx, jw, releaseID, localStagingDir, prev, done)

	status := "ok"
	if err != nil {
		status = "fail"
		_ = jw.Append(ctx, journal.Record{Phase: "deploy", Event: "finish", Status: status, Detail: err.Error()})
	} else {
		_ = jw.Append(ctx, journal.Record{Phase: "deploy", Event: "finish", Status: status})
	}
	// a fenced runner's lock belongs to the NEW deploy — never remove it
	if !errors.Is(err, ErrFenced) {
		e.ReleaseLock(ctx)
	}
	return err
}

func (e *Engine) runPhases(ctx context.Context, jw *journal.Writer, releaseID, localStagingDir, prev string, done map[string]bool) error {
	remoteDir := release.PathsFor(e.Cfg.App).Releases + "/" + releaseID
	remoteCompose := remoteDir + "/compose.yaml"

	if done["transfer"] {
		e.logf("phase transfer: already complete (resume)")
	} else {
		e.logf("phase transfer (%s)", releaseID)
		if localStagingDir == "" {
			res, err := e.T.Run(ctx, "test -d "+q(remoteDir))
			if err != nil || res.ExitCode != 0 {
				return fmt.Errorf("resume: release dir %s missing on host and no local staging", remoteDir)
			}
		} else {
			if _, err := release.Push(ctx, e.T, localStagingDir, e.Cfg.App, releaseID); err != nil {
				return fmt.Errorf("transfer: %w", err)
			}
		}
		_ = jw.Append(ctx, journal.Record{Phase: "transfer", Event: "result", Status: "ok"})
	}

	e.logf("phase pre-release")
	if done["migrate"] {
		e.logf("migrate: already complete (resume)")
	} else {
		_ = jw.Append(ctx, journal.Record{Phase: "pre-release", SubStep: "migrate", Event: "intent"})
		gateDetail, err := e.runMigrate(ctx, remoteDir, remoteCompose)
		if err != nil {
			_ = jw.Append(ctx, journal.Record{Phase: "pre-release", SubStep: "migrate", Event: "result", Status: "fail", Detail: err.Error()})
			return fmt.Errorf("pre-release: %w", err)
		}
		_ = jw.Append(ctx, journal.Record{Phase: "pre-release", SubStep: "migrate", Event: "result", Status: "ok", Detail: gateDetail})
	}
	if err := e.RunHook(ctx, "pre_release", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}

	e.logf("phase release")
	for _, roleName := range e.Cfg.Order {
		if done["release:"+roleName] {
			e.logf("release %s: already complete (resume)", roleName)
			continue
		}
		role := e.Cfg.Roles[roleName]
		e.logf("release %s (%s, %s)", roleName, role.Service, role.Mode)
		_ = jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "intent"})
		var err error
		if role.Mode == "rolling" {
			err = e.RollRole(ctx, roleName, remoteCompose)
		} else {
			err = e.RecreateRole(ctx, roleName, remoteCompose)
		}
		if err != nil {
			_ = jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "result", Status: "fail", Detail: err.Error()})
			return fmt.Errorf("release %s: %w (deploy halted — `yeet resume` after fixing, or `yeet abort`)", roleName, err)
		}
		_ = jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "result", Status: "ok"})
	}
	if err := e.RunHook(ctx, "post_release", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("post-release: %w", err)
	}

	e.logf("phase verify")
	if err := e.Verify(ctx); err != nil {
		return e.onVerifyFailure(ctx, jw, releaseID, prev, err)
	}
	_ = jw.Append(ctx, journal.Record{Phase: "verify", Event: "result", Status: "ok"})

	e.logf("phase finalize")
	if err := e.activate(ctx, releaseID); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	if err := e.pruneRetention(ctx); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	if err := e.RunHook(ctx, "post_deploy", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("post-deploy: %w", err)
	}
	e.logf("deployed %s", releaseID)
	return nil
}

func (e *Engine) activate(ctx context.Context, id string) error {
	p := release.PathsFor(e.Cfg.App)
	res, err := e.mutate(ctx, "ln -sfn "+q("releases/"+id)+" "+q(p.Current))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("activate: %s", res.Stderr)
	}
	return nil
}

// pruneRetention removes releases beyond retain and journals beyond twice
// that window (a journal outlives its release, design §05).
func (e *Engine) pruneRetention(ctx context.Context) error {
	victims, err := release.PruneCandidates(ctx, e.T, e.Cfg.App, e.Cfg.Retain)
	if err != nil {
		return err
	}
	for _, id := range victims {
		if _, err := e.mutate(ctx, "rm -rf "+q(release.PathsFor(e.Cfg.App).Releases+"/"+id)); err != nil {
			return err
		}
	}
	if len(victims) > 0 {
		e.logf("pruned %d old releases", len(victims))
	}
	jvictims, err := journal.PruneCandidates(ctx, e.T, e.Cfg.App, e.Cfg.Retain*2)
	if err != nil {
		return err
	}
	for _, id := range jvictims {
		if _, err := e.mutate(ctx, "rm -f "+q(release.PathsFor(e.Cfg.App).Base+"/journal/"+id+".jsonl")); err != nil {
			return err
		}
	}
	return nil
}

// Rollback re-releases the previous release dir: its compose.yaml pins the
// old image locally (design §06 "rollback never pulls"), and its own yeet.yml
// snapshot drives the choreography — old release, old config, old modes.
func (e *Engine) Rollback(ctx context.Context) error {
	prev, err := release.Previous(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	prevDir := release.PathsFor(e.Cfg.App).Releases + "/" + prev
	remoteCompose := prevDir + "/compose.yaml"

	// replay engine: the snapshot's choreography when available
	replay := e
	res, err := e.T.Run(ctx, "cat "+q(prevDir+"/yeet.snapshot.yml"))
	if err != nil {
		return err
	}
	if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
		snapCfg, serr := config.LoadBytes([]byte(res.Stdout), prev+"/yeet.snapshot.yml")
		if serr == nil {
			serr = snapCfg.Validate()
		}
		if serr != nil {
			e.logf("warn: snapshot unusable (%v) — replaying with CURRENT yeet.yml choreography", serr)
		} else {
			cp := *e
			cp.Cfg = snapCfg
			replay = &cp
		}
	} else {
		e.logf("warn: no yeet.snapshot.yml in %s (pre-M1 release?) — replaying with CURRENT yeet.yml choreography", prev)
	}

	epoch, err := e.AcquireLock(ctx, prev, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, prev, epoch); err != nil {
		return err
	}
	replay.fenceVal = e.fenceVal
	jw := &journal.Writer{T: e.T, App: e.Cfg.App, DeployID: prev, Epoch: epoch, Operator: journal.DefaultOperator()}
	_ = jw.Append(ctx, journal.Record{Phase: "rollback", Event: "start"})

	e.logf("rolling back to %s", prev)
	if err := replay.releaseRoles(ctx, remoteCompose); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "rollback", Event: "finish", Status: "fail", Detail: err.Error()})
		return fmt.Errorf("rollback: %w", err)
	}
	if err := replay.Verify(ctx); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "rollback", Event: "finish", Status: "fail", Detail: err.Error()})
		return fmt.Errorf("rollback verify: %w", err)
	}
	if err := e.activate(ctx, prev); err != nil {
		return err
	}
	_ = jw.Append(ctx, journal.Record{Phase: "rollback", Event: "finish", Status: "ok"})
	return nil
}

func (e *Engine) releaseRoles(ctx context.Context, remoteCompose string) error {
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		e.logf("release %s (%s, %s)", roleName, role.Service, role.Mode)
		var err error
		if role.Mode == "rolling" {
			err = e.RollRole(ctx, roleName, remoteCompose)
		} else {
			err = e.RecreateRole(ctx, roleName, remoteCompose)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", roleName, err)
		}
	}
	return nil
}
