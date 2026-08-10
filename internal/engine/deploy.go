package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/ui"
)

// Deploy runs the lifecycle under the full trust regime:
// lock → fence → journal every phase → finish. Every mutating command is
// fence-guarded; a zombie runner dies host-side with ErrFenced.
func (e *Engine) Deploy(ctx context.Context, releaseID, localStagingDir string) error {
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	return e.deployCore(ctx, releaseID, localStagingDir, nil)
}

// ValidateDeployNoOp establishes the same lock/fence authority as Deploy and
// re-runs preflight plus the adapter's bound-state precondition. It performs no
// release transfer or workload mutation. This makes a no-op result an
// authoritative point-in-time decision rather than a stale pre-lock guess.
func (e *Engine) ValidateDeployNoOp(ctx context.Context, operationID string) error {
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
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
	prev, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return err
	}
	if err := e.requireServingApplicationManifest(ctx, prev); err != nil {
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
		T: e.T, Names: e.names(), DeployID: releaseID, Epoch: epoch,
		Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash,
		ApprovalDigest: e.Opts.ApprovalDigest, ApprovalClass: e.Opts.ApprovalClass,
		ApprovedBy: e.Opts.ApprovedBy, ApprovalSource: e.Opts.ApprovalSource,
		AllowUnknownMigration:   e.Opts.AllowUnknownMigration,
		Runner:                  &e.Opts.Runner,
		MigrationBackupRequired: e.Opts.MigrationBackupWasRequired,
		MigrationBackup:         e.Opts.MigrationBackup,
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
			baselineErr := fmt.Errorf("journal effect baseline: %w", err)
			finishErr := jw.Append(ctx, journal.Record{Phase: "deploy", Event: "finish", Status: "fail", Detail: baselineErr.Error()})
			if finishErr != nil {
				return errors.Join(baselineErr, fmt.Errorf("journal deploy finish: %w", finishErr))
			}
			return baselineErr
		}
		e.gateOpen = !rollbackDebt
		e.rollbackCovered = !rollbackDebt
	}

	err = e.runPhases(ctx, jw, releaseID, localStagingDir, prev, done)
	finish := journal.Record{Phase: "deploy", Event: "finish", Status: "ok"}
	if err != nil {
		finish.Status = "fail"
		finish.Detail = err.Error()
	}
	if finishErr := jw.Append(ctx, finish); finishErr != nil {
		return errors.Join(err, fmt.Errorf("journal deploy finish: %w", finishErr))
	}
	if err == nil {
		hint := ""
		if prev != "" {
			hint = " (prev " + prev + " — `ob rollback`)"
		}
		e.ui.Successf("deployed %s in %s%s", releaseID, ui.FmtDur(time.Since(t0)), hint)
	}
	return err
}

// rollbackEffectDebt carries rollback-unknown effects across deploy IDs. A
// failed deploy can mutate data even though its runner exits cleanly and writes
// finish:fail; a later successful activation/current release or an explicit
// abort clears that historical debt.
func (e *Engine) rollbackEffectDebt(ctx context.Context, current string) (bool, error) {
	ids, byID, err := journal.Journals(ctx, e.T, e.names())
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
	remoteDir := release.PathsFor(e.names()).Releases + "/" + releaseID
	remoteCompose := remoteDir + "/compose.yaml"
	var manifest release.Manifest

	if done["transfer"] {
		e.logf("transfer: already complete (resume)")
	} else {
		tr := e.ui.Step("transfer", false)
		if localStagingDir == "" {
			res, err := e.T.Run(ctx, "test -d "+q(remoteDir))
			if err != nil || res.ExitCode != 0 {
				tr(err)
				// Uploads are atomic, so an interrupted transfer leaves nothing
				// here at all — which is the point, but it also means resume has
				// no payload and no way to obtain one: it never carries a local
				// staging directory. Say what the operator has to do instead of
				// leaving them to infer it from a missing path.
				return fmt.Errorf("resume: release dir %s is not on the host and resume has no local staging to send. "+
					"The transfer was interrupted before it completed, so this release cannot be resumed; "+
					"run `ob abort` and deploy again", remoteDir)
			}
		} else {
			if _, err := release.Push(ctx, e.T, localStagingDir, e.names(), releaseID); err != nil {
				tr(err)
				return fmt.Errorf("transfer: %w", err)
			}
		}
		tr(nil)
		if err := jw.Append(ctx, journal.Record{Phase: "transfer", Event: "result", Status: "ok"}); err != nil {
			return fmt.Errorf("journal transfer result: %w", err)
		}
	}
	var manifestErr error
	if done["transfer"] {
		manifest, manifestErr = e.resumeApplicationManifest(ctx, releaseID)
	} else {
		manifest, manifestErr = e.newApplicationManifest(ctx, releaseID)
	}
	if manifestErr != nil {
		return fmt.Errorf("release manifest: %w", manifestErr)
	}

	// Before any job runs: a job can need a database as readily as an
	// application can, and both read a file that only exists once it is
	// written.
	if err := e.EnsureServiceConnections(ctx); err != nil {
		return fmt.Errorf("service connections: %w", err)
	}

	if err := e.enforceMigrationBackup(ctx, jw, done); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}

	// Jobs run first, gated (migrations before new code). runJobs journals each
	// step and sets the rollback gate.
	if err := e.runJobs(ctx, jw, done, remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}
	if err := e.runRollbackEffectHook(ctx, jw, done, "pre_release", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}

	for _, roleName := range e.Spec.ReleaseOrder() {
		if done["release:"+roleName] {
			e.logf("release %s: already complete (resume)", roleName)
			continue
		}
		role := e.Spec.Workloads[roleName]
		label := roleName + " " + role.Mode()
		if n := role.Count(); n > 1 {
			label = fmt.Sprintf("%s ×%d", label, n)
		}
		st := e.ui.Step(label, true)
		if err := jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "intent"}); err != nil {
			st(err)
			return fmt.Errorf("journal release %s intent: %w", roleName, err)
		}
		var err error
		if role.Mode() == "rolling" {
			err = e.RollRole(ctx, roleName, remoteCompose)
		} else {
			err = e.RecreateRole(ctx, roleName, remoteCompose)
		}
		st(err)
		if err != nil {
			releaseErr := fmt.Errorf("release %s: %w (deploy halted — `ob resume` after fixing, or `ob abort`)", roleName, err)
			if journalErr := jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "result", Status: "fail", Detail: err.Error()}); journalErr != nil {
				return errors.Join(releaseErr, fmt.Errorf("journal release %s result: %w", roleName, journalErr))
			}
			return releaseErr
		}
		if err := jw.Append(ctx, journal.Record{Phase: "release", Role: roleName, Event: "result", Status: "ok"}); err != nil {
			return fmt.Errorf("journal release %s result: %w", roleName, err)
		}
	}
	if err := e.runPostReleaseJobs(ctx, jw, done, remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("post-release: %w", err)
	}
	if err := e.runRollbackEffectHook(ctx, jw, done, "post_release", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("post-release: %w", err)
	}

	e.progress("verification", "started", "")
	vf := e.ui.Step("verify", false)
	if err := e.Verify(ctx); err != nil {
		vf(err)
		e.progress("verification", "failed", "verification failed; inspect journal evidence")
		return e.onVerifyFailure(ctx, jw, releaseID, prev, err)
	}
	vf(nil)
	if err := jw.Append(ctx, journal.Record{Phase: "verify", Event: "result", Status: "ok"}); err != nil {
		return fmt.Errorf("journal verify result: %w", err)
	}
	if manifest.State != release.StateStaged {
		return fmt.Errorf("release %s cannot activate from manifest state %s", releaseID, manifest.State)
	}
	e.progress("verification", "succeeded", "")

	e.progress("activation", "started", "")
	fin := e.ui.Step("activate", false)
	if err := jw.Append(ctx, journal.Record{
		Phase: "activation", Event: "intent", Detail: "release=" + releaseID,
	}); err != nil {
		fin(err)
		e.progress("activation", "failed", "activation evidence could not be persisted")
		return fmt.Errorf("journal activation intent: %w", err)
	}
	if err := e.activateManifest(ctx, &manifest, prev); err != nil {
		fin(err)
		activationErr := fmt.Errorf("finalize: %w", err)
		journalErr := jw.Append(ctx, journal.Record{
			Phase: "activation", Event: "result", Status: "fail", Detail: "release=" + releaseID,
		})
		e.progress("activation", "failed", "activation failed; inspect journal evidence")
		if journalErr != nil {
			return errors.Join(activationErr, fmt.Errorf("journal activation result: %w", journalErr))
		}
		return activationErr
	}
	if err := jw.Append(ctx, journal.Record{
		Phase: "activation", Event: "result", Status: "ok", Detail: "release=" + releaseID,
	}); err != nil {
		fin(err)
		e.progress("activation", "failed", "activation succeeded but its evidence could not be persisted")
		return fmt.Errorf("journal activation result: %w", err)
	}
	fin(nil)
	e.progress("activation", "succeeded", "")
	e.progress("cleanup", "started", "")
	if err := e.pruneRetention(ctx); err != nil {
		e.progress("cleanup", "failed", "retention cleanup failed after activation; inspect journal evidence")
		return fmt.Errorf("prune: %w", err)
	}
	e.progress("cleanup", "succeeded", "")
	// After activation, because a timer invokes the job through `current` and
	// that pointer has only just moved. Before the post-deploy hook, so a hook
	// that inspects the schedule sees the one this release declares.
	if err := e.SyncSchedules(ctx); err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
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
	hook, ok := e.Spec.Hooks[name]
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
	p := release.PathsFor(e.names())
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
// that window because a journal outlives its release.
func (e *Engine) pruneRetention(ctx context.Context) error {
	journalIDs, err := journal.List(ctx, e.T, e.names())
	if err != nil {
		return err
	}
	policy := release.DefaultRetentionPolicy(e.Spec.Deployment.RetainReleases, e.Opts.Now())
	policy.EvidenceIDs = make(map[string]bool, len(journalIDs))
	for _, id := range journalIDs {
		policy.EvidenceIDs[id] = true
	}
	decision, err := release.RetentionCandidates(ctx, e.T, e.names(), policy)
	if err != nil {
		return err
	}
	if len(decision.Reported) > 0 {
		e.warnf("%d release-store entr(ies) were preserved for inspection: %v", len(decision.Reported), decision.Reported)
	}
	for _, id := range decision.Victims {
		if err := e.mutateChecked(ctx, "prune release "+id, "rm -rf "+q(release.PathsFor(e.names()).Releases+"/"+id)); err != nil {
			return err
		}
	}
	if len(decision.Victims) > 0 {
		e.logf("pruned %d expired release-store entries", len(decision.Victims))
	}
	jvictims, err := journal.PruneCandidates(ctx, e.T, e.names(), e.Spec.Deployment.RetainReleases*2)
	if err != nil {
		return err
	}
	for _, id := range jvictims {
		if err := e.mutateChecked(ctx, "prune journal "+id, "rm -f "+q(release.PathsFor(e.names()).Base+"/journal/"+id+".jsonl")); err != nil {
			return err
		}
	}
	return nil
}

// Rollback re-releases the previous release dir: its compose.yaml pins the
// old image locally because rollback never pulls, and its own ob.yml
// snapshot drives the choreography — old release, old config, old modes.
func (e *Engine) Rollback(ctx context.Context) error {
	_, err := e.RollbackWithJournalID(ctx)
	return err
}

// RollbackWithJournalID rolls back to the previous release and returns the
// journal identity used for the rollback evidence. The identity is returned
// even when execution fails after the target release has been resolved.
func (e *Engine) RollbackWithJournalID(ctx context.Context) (string, error) {
	observedCurrent, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return "", err
	}
	if observedCurrent == "" {
		return "", fmt.Errorf("no rollback target: there is no current release")
	}
	epoch, err := e.AcquireLock(ctx, observedCurrent, e.Opts.ForceLock)
	if err != nil {
		return "", err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, observedCurrent, epoch); err != nil {
		return "", err
	}
	current, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return "", err
	}
	prev, err := release.Previous(ctx, e.T, e.names())
	if err != nil {
		return "", err
	}
	return prev, e.rollbackTo(ctx, prev, current, epoch)
}

func (e *Engine) rollbackTo(ctx context.Context, prev, current string, epoch int) (err error) {
	prevDir := release.PathsFor(e.names()).Releases + "/" + prev
	remoteCompose := prevDir + "/compose.yaml"

	replay, err := e.engineFromReleaseSnapshot(ctx, prev)
	if err != nil {
		return err
	}

	replay.fenceVal = e.fenceVal
	jw := &journal.Writer{T: e.T, Names: e.names(), DeployID: prev, Epoch: epoch, Operator: journal.DefaultOperator(), Runner: &e.Opts.Runner}
	if err := jw.Append(ctx, journal.Record{Phase: "rollback", Event: "start"}); err != nil {
		return fmt.Errorf("journal rollback start: %w", err)
	}
	defer func() {
		finish := journal.Record{Phase: "rollback", Event: "finish", Status: "ok"}
		if err != nil {
			finish.Status = "fail"
			finish.Detail = err.Error()
		}
		if journalErr := jw.Append(ctx, finish); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal rollback finish: %w", journalErr))
		}
	}()

	e.logf("rolling back to %s", prev)
	if err := replay.releaseRoles(ctx, remoteCompose); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	if err := replay.Verify(ctx); err != nil {
		return fmt.Errorf("rollback verify: %w", err)
	}
	target, err := release.ReadManifest(ctx, e.T, e.names(), prev)
	if err != nil {
		return fmt.Errorf("rollback target manifest: %w", err)
	}
	if err := e.reactivateManifest(ctx, &target, current); err != nil {
		return fmt.Errorf("rollback activation: %w", err)
	}
	return nil
}

func (e *Engine) releaseRoles(ctx context.Context, remoteCompose string) error {
	for _, roleName := range e.Spec.ReleaseOrder() {
		role := e.Spec.Workloads[roleName]
		e.logf("release %s (%s)", roleName, role.Mode())
		var err error
		if role.Mode() == "rolling" {
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
