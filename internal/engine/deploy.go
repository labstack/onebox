package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
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
// release transfer or workload mutation. The host-scoped managed proxy may
// still converge: application no-op means the release payload is unchanged,
// not that an older runner's security boundary may remain in service.
func (e *Engine) ValidateDeployNoOp(ctx context.Context, operationID string) error {
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	epoch, err := e.acquireLock(ctx, operationID, e.Opts.ForceLock, pinnedScheduleLeasePolicy{allow: true})
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return err
	}
	stopHeartbeat := e.StartHeartbeat(ctx)
	defer stopHeartbeat()
	if e.Opts.DeployPrecondition != nil {
		if err := e.Opts.DeployPrecondition(ctx, e); err != nil {
			return fmt.Errorf("deploy precondition under lock: %w", err)
		}
	}
	if err := e.preflight(ctx, false); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if e.Spec.Proxy.Managed {
		if err := e.EnsureProxy(ctx, operationID, e.Opts.ForceLock); err != nil {
			return fmt.Errorf("managed proxy: %w", err)
		}
	}
	return nil
}

// deployCore: done != nil means resume — completed steps skip; staging may be
// empty (the release dir already lives on the host).
func (e *Engine) deployCore(ctx context.Context, releaseID, localStagingDir string, done map[string]bool) error {
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	e.ui.Header("deploy " + releaseID)
	t0 := time.Now()
	leasePolicy := pinnedScheduleLeasePolicy{allow: true}
	if conflict := e.pinnedScheduleDeployConflict(); conflict != "" {
		leasePolicy = pinnedScheduleLeasePolicy{conflict: conflict}
	}
	epoch, err := e.acquireLock(ctx, releaseID, e.Opts.ForceLock, leasePolicy)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, releaseID, epoch); err != nil {
		return err
	}
	stopHB := e.StartHeartbeat(ctx)
	defer stopHB()
	// The plan binding is the mutation boundary. Check it under the application
	// lock before converging even host-scoped support components; a stale plan
	// must leave both the application and proxy untouched.
	if e.Opts.DeployPrecondition != nil {
		if err := e.Opts.DeployPrecondition(ctx, e); err != nil {
			return fmt.Errorf("deploy precondition under lock: %w", err)
		}
	}
	pf := e.ui.Step("preflight", false)
	if err := e.preflight(ctx, false); err != nil {
		pf(err)
		return fmt.Errorf("preflight: %w", err)
	}
	pf(nil)
	if err := e.validateRetainedWorkloads(ctx); err != nil {
		return fmt.Errorf("deploy precondition under lock: %w", err)
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
	// A released runner may change the managed proxy's security/runtime
	// contract. All plan, host, workload, and rollback preconditions above are
	// read-only and must pass first; only then may the deploy move an existing
	// installation to the new proxy boundary internally.
	if e.Spec.Proxy.Managed {
		proxyStep := e.ui.Step("managed proxy", false)
		if err := e.EnsureProxy(ctx, releaseID, e.Opts.ForceLock); err != nil {
			proxyStep(err)
			return fmt.Errorf("managed proxy: %w", err)
		}
		proxyStep(nil)
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
	if done == nil {
		if err := e.recordWorkloadPlans(ctx, jw); err != nil {
			return err
		}
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

// pinnedScheduleDeployConflict reports lifecycle effects that cannot safely
// overlap a read-only pinned job. Container replacement itself is compatible:
// the scheduled container and all release-owned inputs are already pinned.
// Jobs that change shared data and untyped hooks are not compatible because a
// running export or indexer may still depend on the schema or data they change.
func (e *Engine) pinnedScheduleDeployConflict() string {
	var conflicts []string
	for _, phase := range []string{"pre_release", "post_release"} {
		for _, name := range e.Spec.JobOrderFor(phase) {
			effect := e.Spec.Workloads[name].DataEffect
			if effect != app.DataEffectNone {
				conflicts = append(conflicts, fmt.Sprintf("job %s (%s)", name, effect))
			}
		}
	}
	for _, name := range []string{"pre_release", "post_release", "post_deploy"} {
		if hook, ok := e.Spec.Hooks[name]; ok && strings.TrimSpace(hook.Run) != "" {
			conflicts = append(conflicts, "hook "+name+" (unknown data effect)")
		}
	}
	return strings.Join(conflicts, ", ")
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

	// A serving manifest means activation already completed durably, so this
	// operation is past the seam: nothing before activation may run again, and
	// only the post-activation steps remain. A fresh deploy always stages its
	// own manifest, so this is reachable from resume alone — and finalize
	// refuses anyway unless the journal proves this operation activated it.
	if manifest.State == release.StateServing {
		return e.finalizeActivated(ctx, jw, &manifest, done, remoteDir, remoteCompose)
	}

	// Anything else the host has already settled — superseded by a manual
	// rollback, or terminally failed or aborted — is refused HERE, before a job
	// runs or a role rolls. Activation would refuse it anyway, but by then this
	// deploy would have started containers of a release the host moved past.
	if !activationResumable(manifest.State) {
		return &ActivationRefusedError{ReleaseID: releaseID, State: manifest.State}
	}

	// The generated runtime declares its default network external so release
	// teardown cannot remove a long-lived proxy endpoint. Establish and verify
	// ownership before any job or workload can join it.
	if err := e.EnsureApplicationNetwork(ctx); err != nil {
		return fmt.Errorf("application network: %w", err)
	}

	// Before any job runs: a job can need a database as readily as an
	// application can, and both read a file that only exists once it is
	// written.
	if e.Spec.HasServiceExtensions() {
		if err := e.ApplyExtensionServices(ctx); err != nil {
			return fmt.Errorf("service extensions: %w", err)
		}
	}
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
		workloadPlan, planned := e.Opts.WorkloadPlans[roleName]
		if planned && workloadPlan.Retained() {
			label := roleName + " retain"
			st := e.ui.Step(label, false)
			e.logf("retain %s (%s)", roleName, workloadPlan.Reason)
			err := jw.Append(ctx, journal.Record{
				Phase: "release", Role: roleName, Event: "result", Status: "ok",
				Detail: "action=retain revision=" + workloadPlan.Revision + " reason=" + workloadPlan.Reason,
			})
			st(err)
			if err != nil {
				return fmt.Errorf("journal retain %s result: %w", roleName, err)
			}
			continue
		}
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
	if !activationResumable(manifest.State) {
		return &ActivationRefusedError{ReleaseID: releaseID, State: manifest.State}
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
		// The checkpoint stays open on purpose: recovery reconciles an open
		// checkpoint, and clearing it here would strand a live release whose
		// activation nothing recorded.
		//
		// Through failedOutcome like every other exit past activation:
		// Transition(StateServing) already stamped the manifest succeeded,
		// and an interrupt here would otherwise report "cancelled, nothing
		// was changed" for a generation that is live.
		return e.failedOutcome(ctx, &manifest, fmt.Errorf("journal activation result: %w", err))
	}
	// Evidence is durable, so the checkpoint has done its job. Typed, because
	// the release is already live: an interrupt here returns ctx.Err()
	// verbatim, and an untyped one ships as "cancelled, nothing was changed"
	// for a deploy whose new generation is serving.
	if err := e.clearActivationCheckpoint(ctx); err != nil {
		// Activation itself succeeded — symlink switched, manifest serving,
		// journal recorded. Reporting phase=activation status=failed would
		// have a stream consumer conclude the new generation never took
		// effect and roll back a healthy, live release.
		fin(nil)
		e.progress("activation", "succeeded", "")
		e.progress("cleanup", "failed", "the release is serving; its activation checkpoint could not be cleared")
		// Through failedOutcome like every other post-activation exit:
		// Transition(StateServing) set the outcome to succeeded, and leaving
		// it there would have the manifest claim a finished operation for one
		// that failed.
		return e.failedOutcome(ctx, &manifest, fmt.Errorf("clear activation checkpoint: %w", err))
	}
	fin(nil)
	e.progress("activation", "succeeded", "")
	return e.runPostActivation(ctx, jw, &manifest, done, remoteDir, remoteCompose)
}

func (e *Engine) recordWorkloadPlans(ctx context.Context, jw *journal.Writer) error {
	names := make([]string, 0, len(e.Opts.WorkloadPlans))
	for name := range e.Opts.WorkloadPlans {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		plan := e.Opts.WorkloadPlans[name]
		if err := jw.Append(ctx, journal.Record{
			Phase: "workload-plan", Role: name, Event: "result", Status: "ok",
			WorkloadAction: string(plan.Action), WorkloadRevision: plan.Revision, Reason: plan.Reason,
		}); err != nil {
			return fmt.Errorf("journal workload plan %s: %w", name, err)
		}
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

// retentionSkipped is the durable, value-free note that retention declined to
// act. The reason can carry host stderr, which the journal contract keeps off
// durable evidence, so the detail stays on the trusted local path and only this
// stable phrase is journaled.
const retentionSkipped = "release-store cleanup skipped: retention evidence is incomplete"

// pruneRetention removes releases beyond retain and journals beyond twice
// that window because a journal outlives its release. It returns the skip note
// when it deliberately declined to delete anything, so that the journal records
// a skip rather than an unqualified success.
func (e *Engine) pruneRetention(ctx context.Context) (string, error) {
	journalIDs, err := journal.List(ctx, e.T, e.names())
	if err != nil {
		return "", err
	}
	policy := release.DefaultRetentionPolicy(e.Spec.Deployment.RetainReleases, e.Opts.Now())
	policy.EvidenceIDs = make(map[string]bool, len(journalIDs))
	for _, id := range journalIDs {
		policy.EvidenceIDs[id] = true
	}
	decision, err := release.RetentionCandidates(ctx, e.T, e.names(), policy)
	var evidence *release.RetentionEvidenceError
	switch {
	case errors.As(err, &evidence):
		// Refusing to delete on incomplete evidence is the contract. Failing the
		// operation over it is not: the release is healthy and serving, and a
		// deploy that cannot reach a terminal state is a worse outcome than a
		// release store that keeps one directory too many.
		//
		// Nothing else in this run may delete either. Journals are the evidence
		// that protects release directories with no readable manifest, so
		// pruning them during the one run that just said the evidence is
		// incomplete would leave the store LESS protected than a clean run.
		// The whole of retention declines together and a later run does both.
		e.warnf("release-store cleanup skipped — %v", evidence)
		return retentionSkipped, nil
	case err != nil:
		return "", err
	default:
		if len(decision.Reported) > 0 {
			e.warnf("%d release-store entr(ies) were preserved for inspection: %v", len(decision.Reported), decision.Reported)
		}
		for _, id := range decision.Victims {
			if err := e.mutateChecked(ctx, "prune release "+id, "rm -rf "+q(release.PathsFor(e.names()).Releases+"/"+id)); err != nil {
				return "", err
			}
		}
		if len(decision.Victims) > 0 {
			e.logf("pruned %d expired release-store entries", len(decision.Victims))
		}
	}
	jvictims, err := journal.PruneCandidates(ctx, e.T, e.names(), e.Spec.Deployment.RetainReleases*2)
	if err != nil {
		return "", err
	}
	for _, id := range jvictims {
		if err := e.mutateChecked(ctx, "prune journal "+id, "rm -f "+q(release.PathsFor(e.names()).Base+"/journal/"+id+".jsonl")); err != nil {
			return "", err
		}
	}
	return "", nil
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
	if err := e.RequireHostOwner(ctx); err != nil {
		return "", err
	}
	observedCurrent, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return "", err
	}
	if observedCurrent == "" {
		return "", &release.RollbackTargetMissingError{Reason: "there is no current release"}
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
	// Same reasoning as the deploy path: the rollback target is already
	// serving by the time this runs.
	if err := e.clearActivationCheckpoint(ctx); err != nil {
		if outcomeErr := e.recordOutcome(ctx, &target, release.OutcomeFailed); outcomeErr != nil {
			// Carries the same override as the branch below: without it
			// GuidanceCommand falls back to `ob resume`, which fixes forward
			// against the release this rollback just undid.
			return errors.Join(outcomeErr, &PostActivationFailedError{
				ReleaseID: target.ID,
				Err:       fmt.Errorf("rollback activation: clear activation checkpoint: %w", err),
				Guidance:  "ob status --output json",
			})
		}
		return &PostActivationFailedError{
			ReleaseID: target.ID,
			Err:       fmt.Errorf("rollback activation: clear activation checkpoint: %w", err),
			// Not `ob resume`: the rollback target is serving, and resume
			// would try to finalize the deploy this rollback undid.
			Guidance: "ob status --output json",
		}
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
