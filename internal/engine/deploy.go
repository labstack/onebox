package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/ui"
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
	e.ui.Header("deploy " + releaseID)
	t0 := time.Now()
	pf := e.ui.Step("preflight", false)
	if err := e.Preflight(ctx); err != nil {
		pf(err)
		return fmt.Errorf("preflight: %w", err)
	}
	pf(nil)
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
	if err == nil {
		hint := ""
		if prev != "" {
			hint = " (prev " + prev + " — `ob rollback`)"
		}
		e.ui.Successf("deployed %s in %s%s", releaseID, ui.FmtDur(time.Since(t0)), hint)
	}

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
		e.logf("transfer: already complete (resume)")
	} else {
		tr := e.ui.Step("transfer", false)
		if localStagingDir == "" {
			res, err := e.T.Run(ctx, "test -d "+q(remoteDir))
			if err != nil || res.ExitCode != 0 {
				tr(err)
				return fmt.Errorf("resume: release dir %s missing on host and no local staging", remoteDir)
			}
		} else {
			if _, err := release.Push(ctx, e.T, localStagingDir, e.Cfg.App, releaseID); err != nil {
				tr(err)
				return fmt.Errorf("transfer: %w", err)
			}
		}
		tr(nil)
		_ = jw.Append(ctx, journal.Record{Phase: "transfer", Event: "result", Status: "ok"})
	}

	// Jobs run first, gated (migrations before new code). runJobs journals each
	// step and sets the rollback gate.
	if err := e.runJobs(ctx, jw, done, remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}
	if err := e.RunHook(ctx, "pre_release", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}

	for _, roleName := range e.Cfg.Order {
		if done["release:"+roleName] {
			e.logf("release %s: already complete (resume)", roleName)
			continue
		}
		role := e.Cfg.Roles[roleName]
		label := roleName + " " + role.Mode
		if n := role.Count(); n > 1 {
			label = fmt.Sprintf("%s ×%d", label, n)
		}
		st := e.ui.Step(label, true)
		_ = jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "intent"})
		var err error
		if role.Mode == "rolling" {
			err = e.RollRole(ctx, roleName, remoteCompose)
		} else {
			err = e.RecreateRole(ctx, roleName, remoteCompose)
		}
		st(err)
		if err != nil {
			_ = jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "result", Status: "fail", Detail: err.Error()})
			return fmt.Errorf("release %s: %w (deploy halted — `ob resume` after fixing, or `ob abort`)", roleName, err)
		}
		_ = jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "result", Status: "ok"})
	}
	if err := e.RunHook(ctx, "post_release", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("post-release: %w", err)
	}

	vf := e.ui.Step("verify", false)
	if err := e.Verify(ctx); err != nil {
		vf(err)
		return e.onVerifyFailure(ctx, jw, releaseID, prev, err)
	}
	vf(nil)
	_ = jw.Append(ctx, journal.Record{Phase: "verify", Event: "result", Status: "ok"})

	fin := e.ui.Step("activate", false)
	if err := e.activate(ctx, releaseID); err != nil {
		fin(err)
		return fmt.Errorf("finalize: %w", err)
	}
	if err := e.pruneRetention(ctx); err != nil {
		fin(err)
		return fmt.Errorf("prune: %w", err)
	}
	fin(nil)
	if err := e.RunHook(ctx, "post_deploy", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("post-deploy: %w", err)
	}
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
			e.warnf("snapshot unusable (%v) — replaying with CURRENT yeet.yml choreography", serr)
		} else {
			cp := *e
			cp.Cfg = snapCfg
			replay = &cp
		}
	} else {
		e.warnf("no yeet.snapshot.yml in %s (pre-M1 release?) — replaying with CURRENT yeet.yml choreography", prev)
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
