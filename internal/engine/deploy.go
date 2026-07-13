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

// ValidateDeployNoOp establishes the same lock/fence authority as Deploy and
// re-runs preflight plus the adapter's bound-state precondition. It performs no
// release transfer or workload mutation. This makes a no-op result an
// authoritative point-in-time decision rather than a stale pre-lock guess.
func (e *Engine) ValidateDeployNoOp(ctx context.Context, operationID string) error {
	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return err
	}
	stopHeartbeat := e.StartHeartbeat(ctx)
	defer stopHeartbeat()
	if err := e.Preflight(ctx); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if e.Opts.DeployPrecondition != nil {
		if err := e.Opts.DeployPrecondition(ctx, e); err != nil {
			return fmt.Errorf("deploy precondition under lock: %w", err)
		}
	}
	return nil
}

// deployCore: done != nil means resume — completed steps skip; staging may be
// empty (the release dir already lives on the host).
func (e *Engine) deployCore(ctx context.Context, releaseID, localStagingDir string, done map[string]bool) error {
	e.ui.Header("deploy " + releaseID)
	t0 := time.Now()
	epoch, err := e.AcquireLock(ctx, releaseID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, releaseID, epoch); err != nil {
		return err
	}
	stopHB := e.StartHeartbeat(ctx)
	defer stopHB()
	pf := e.ui.Step("preflight", false)
	if err := e.Preflight(ctx); err != nil {
		pf(err)
		return fmt.Errorf("preflight: %w", err)
	}
	pf(nil)
	if e.Opts.DeployPrecondition != nil {
		if err := e.Opts.DeployPrecondition(ctx, e); err != nil {
			return fmt.Errorf("deploy precondition under lock: %w", err)
		}
	}
	prev, err := release.Current(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	rollbackDebt := false
	if done == nil {
		rollbackDebt, err = e.rollbackEffectDebt(ctx, prev)
		if err != nil {
			return fmt.Errorf("rollback effect history: %w", err)
		}
	}

	jw := &journal.Writer{
		T: e.T, App: e.Cfg.App, DeployID: releaseID, Epoch: epoch,
		Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash,
	}
	if err := jw.Append(ctx, journal.Record{Phase: "deploy", Event: "start", Detail: "prev=" + prev}); err != nil {
		return fmt.Errorf("journal deploy start: %w", err)
	}
	if done == nil {
		// Persist a safe baseline before transfer. If the runner dies during
		// upload, no rollback-relevant effect could have started yet; later
		// job/hook intents join this aggregate and may close it. An uncovered
		// effect from a prior failed deploy is carried forward until compatible
		// code activates, so a new deploy cannot erase rollback risk.
		detail := "no effects started"
		if rollbackDebt {
			detail = "inherits uncovered effects from an earlier failed deploy"
		}
		if err := jw.Append(ctx, journal.Record{
			Phase: "pre-release", SubStep: journal.EffectBaselineSubStep,
			Event: "result", Status: "ok", Detail: detail, RollbackSafe: !rollbackDebt,
		}); err != nil {
			_ = jw.Append(ctx, journal.Record{Phase: "deploy", Event: "finish", Status: "fail", Detail: "effect baseline: " + err.Error()})
			return fmt.Errorf("journal effect baseline: %w", err)
		}
		e.gateOpen = !rollbackDebt
		e.rollbackCovered = !rollbackDebt
	}

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
	return err
}

// rollbackEffectDebt carries rollback-unknown effects across deploy IDs. A
// failed deploy can mutate data even though its runner exits cleanly and writes
// finish:fail; a later successful activation/current release or an explicit
// abort clears that historical debt.
func (e *Engine) rollbackEffectDebt(ctx context.Context, current string) (bool, error) {
	ids, byID, err := journal.Journals(ctx, e.T, e.Cfg.App)
	if err != nil {
		return false, err
	}
	debt := false
	for _, id := range ids {
		summary := journal.Summarize(byID[id])
		if id == current || summary.DeploySucceeded {
			debt = false
			continue
		}
		if summary.Aborted {
			continue
		}
		if summary.Done[journal.DoneGateRecorded] && !summary.RollbackCovered {
			debt = true
		}
	}
	return debt, nil
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
	if err := e.runRollbackEffectHook(ctx, jw, done, "pre_release", remoteDir, remoteCompose); err != nil {
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
	if err := e.runRollbackEffectHook(ctx, jw, done, "post_release", remoteDir, remoteCompose); err != nil {
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

// runRollbackEffectHook journals untyped lifecycle hooks as rollback-unknown
// effects. Abort must decide from the interrupted deploy's history, not from a
// possibly edited working-tree config. Successful hooks are also skipped on
// resume so a recovered deploy does not repeat their side effects.
func (e *Engine) runRollbackEffectHook(ctx context.Context, jw *journal.Writer, done map[string]bool, name, remoteDir, remoteCompose string) error {
	hook, ok := e.Cfg.Hooks[name]
	if !ok || hook.Run == "" {
		return nil
	}
	key := "hook:" + name
	if done[key] {
		e.logf("%s: already complete (resume)", key)
		return nil
	}
	e.rollbackCovered = false // lifecycle hooks have no typed rollback-effect contract
	if err := jw.Append(ctx, journal.Record{Phase: name, SubStep: key, Event: "intent"}); err != nil {
		return fmt.Errorf("journal %s intent: %w", key, err)
	}
	err := e.RunHook(ctx, name, remoteDir, remoteCompose)
	result := journal.Record{Phase: name, SubStep: key, Event: "result", Status: "ok"}
	if err != nil {
		result.Status = "fail"
		result.Detail = err.Error()
	}
	if journalErr := jw.Append(ctx, result); journalErr != nil {
		return errors.Join(err, fmt.Errorf("journal %s result: %w", key, journalErr))
	}
	return err
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
// old image locally (design §06 "rollback never pulls"), and its own ob.yml
// snapshot drives the choreography — old release, old config, old modes.
func (e *Engine) Rollback(ctx context.Context) error {
	_, err := e.RollbackWithJournalID(ctx)
	return err
}

// RollbackWithJournalID rolls back to the previous release and returns the
// journal identity used for the rollback evidence. The identity is returned
// even when execution fails after the target release has been resolved.
func (e *Engine) RollbackWithJournalID(ctx context.Context) (string, error) {
	prev, err := release.Previous(ctx, e.T, e.Cfg.App)
	if err != nil {
		return "", err
	}
	return prev, e.rollbackTo(ctx, prev)
}

func (e *Engine) rollbackTo(ctx context.Context, prev string) error {
	prevDir := release.PathsFor(e.Cfg.App).Releases + "/" + prev
	remoteCompose := prevDir + "/compose.yaml"

	// replay engine: the snapshot's choreography when available
	replay := e
	res, err := e.T.Run(ctx, "cat "+q(prevDir+"/ob.snapshot.yml"))
	if err != nil {
		return err
	}
	if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
		snapCfg, serr := config.LoadBytes([]byte(res.Stdout), prev+"/ob.snapshot.yml")
		if serr == nil {
			serr = snapCfg.Validate()
		}
		if serr != nil {
			e.warnf("snapshot unusable (%v) — replaying with CURRENT ob.yml choreography", serr)
		} else {
			cp := *e
			cp.Cfg = snapCfg
			replay = &cp
		}
	} else {
		e.warnf("no snapshot in %s (pre-M1 release?) — replaying with CURRENT ob.yml choreography", prev)
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
