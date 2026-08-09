package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

const rollbackUnavailableConsequence = "automatic rollback is unavailable after this step; if a later step fails, halt, fix-forward, then run `ob resume`"

// gateSteps is the ordered set of jobs run at the pre-release gate: every
// `jobs` service, plus an untyped `migrate` hook that isn't itself a job (some
// configs ran the migrate hook whether or not migrate was classified a job).
func (e *Engine) gateSteps() []string {
	steps := append([]string{}, e.Spec.JobOrder()...)
	if h, ok := e.Spec.Hooks["migrate"]; ok && h.Run != "" {
		for _, s := range steps {
			if s == "migrate" {
				return steps
			}
		}
		steps = append(steps, "migrate")
	}
	return steps
}

// runJobs runs every gate step once, before the roll, each under the
// $OB_RESULT_FILE protocol. A step with a same-named hook runs
// that hook's command (a custom migrate invocation); otherwise ob auto-runs
// `docker compose run --rm --no-deps <job>` — so `jobs: [migrate]` needs no
// hook at all. The rollback gate opens only if EVERY step declared
// changed=false; anything else (a real change, no result written, a local
// hook, or a step skipped on resume) fails safe. A v1 data_effect:none job is
// rollback-safe by operator declaration. expand-only covers migration jobs,
// never unknown jobs or untyped lifecycle hooks.
func (e *Engine) runJobs(ctx context.Context, jw *journal.Writer, done map[string]bool, remoteDir, remoteCompose string) error {
	steps := e.gateSteps()
	if len(steps) == 0 {
		// Fresh deploys persist the safe baseline before transfer. Resumes keep
		// the reconstructed aggregate unchanged, including fail-closed sparse
		// journals that have no baseline record.
		return nil
	}
	// Start from the durable baseline restored/created by deployCore. It may
	// already be closed by rollback debt inherited from an earlier deploy.
	allSafe := e.gateOpen
	allCovered := e.rollbackCovered
	for _, job := range steps {
		key := "job:" + job
		if done[key] || done["migrate"] { // "migrate" = pre-jobs journal key
			if e.jobDataEffect(job) == "none" {
				e.logf("%s: already complete (resume) — rollback-safe by data_effect=none declaration", key)
			} else if e.gateOpen {
				e.logf("%s: already complete (resume) — rollback-safe result recovered from journal", key)
			} else {
				e.logf("%s: already complete (resume) — %s", key, rollbackUnavailableConsequence)
			}
			continue
		}
		policySafe := e.jobRollbackPolicySafe(job)
		if err := jw.Append(ctx, journal.Record{
			Phase: "pre-release", SubStep: key, Event: "intent",
			RollbackPolicySafe: policySafe,
		}); err != nil {
			return fmt.Errorf("journal %s intent: %w", key, err)
		}
		st := e.ui.Step("job "+job, true)
		safe, detail, err := e.runOneJob(ctx, job, remoteDir, remoteCompose)
		if err == nil {
			e.logf("job %s: %s", job, detail)
		}
		st(err)
		if err != nil {
			journalErr := jw.Append(ctx, journal.Record{
				Phase: "pre-release", SubStep: key, Event: "result", Status: "fail", Detail: err.Error(),
				RollbackPolicySafe: policySafe,
			})
			if journalErr != nil {
				return errors.Join(err, fmt.Errorf("journal %s result: %w", key, journalErr))
			}
			return err
		}
		if !safe {
			allSafe = false
		}
		if !safe && !policySafe {
			allCovered = false
		}
		resultRecord := journal.Record{
			Phase: "pre-release", SubStep: key, Event: "result", Status: "ok", Detail: detail,
			RollbackSafe: safe, RollbackPolicySafe: policySafe,
		}
		if evidence, ok := e.jobResults[job]; ok {
			evidence := evidence
			resultRecord.JobResult = &evidence
		}
		if err := jw.Append(ctx, resultRecord); err != nil {
			return fmt.Errorf("journal %s result: %w", key, err)
		}
	}
	e.gateOpen = allSafe
	e.rollbackCovered = allCovered
	return nil
}

// runOneJob runs a single gate step and reports whether it declared itself
// rollback-safe (changed=false). Returns (safe, detail, err).
func (e *Engine) runOneJob(ctx context.Context, job, remoteDir, remoteCompose string) (bool, string, error) {
	safeByDeclaration := e.jobDataEffect(job) == "none"
	resultDir := remoteDir + "/.job-" + job + "-result"
	resultFile := resultDir + "/result"
	const containerResultFile = "/run/onebox/job-result"
	containerized := true
	runCmd := e.composeCmd(remoteCompose) + " run --rm --no-deps" +
		" -e OB_RESULT_FILE=" + containerResultFile +
		" -v " + q(resultFile+":"+containerResultFile+":rw") + " " + job
	if h, ok := e.Spec.Hooks[job]; ok && h.Run != "" {
		if h.Local {
			// a local hook can't reach the host result file — run it, fail safe
			e.logf("job %s (local hook): %s", job, h.Run)
			if err := e.RunHook(ctx, job, remoteDir, remoteCompose); err != nil {
				return false, "", err
			}
			if safeByDeclaration {
				return true, "rollback-safe by data_effect=none declaration", nil
			}
			return e.unknownJobResult(job, "local hook cannot write the result file")
		}
		runCmd = h.Run // custom command for this job (e.g. extra flags)
		var injected bool
		runCmd, injected = injectComposeJobResult(runCmd, resultFile, containerResultFile)
		containerized = injected
	}
	e.ui.Cmd("job", runCmd) // verbose only — the plan lists it
	resultMode := "600"
	if containerized {
		// The job may run as an arbitrary container UID. The bind-mounted file is
		// writable only for the duration of this fenced command. Its 0666 mode is
		// hidden from other host users by the private 0700 result directory, then
		// the file is sealed back to 0600 before it is read or journaled.
		resultMode = "666"
	}
	cmd := "cd " + q(remoteDir) +
		" && rm -rf " + q(resultDir) +
		" && install -d -m 700 " + q(resultDir) +
		" && install -m " + resultMode + " /dev/null " + q(resultFile) +
		" && COMPOSE_PROJECT_NAME=" + e.Spec.Name +
		" COMPOSE_FILE=" + q(remoteCompose) +
		" OB_RESULT_FILE=" + q(resultFile) +
		" " + runCmd
	if containerized {
		cmd += "; job_status=$?; chmod 600 " + q(resultFile) + " || exit 125; exit $job_status"
	}
	res, err := e.mutate(ctx, cmd)
	if err != nil {
		return false, "", err
	}
	if res.ExitCode != 0 {
		return false, "", fmt.Errorf("job %s failed (exit %d): %s", job, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if safeByDeclaration {
		return true, "rollback-safe by data_effect=none declaration", nil
	}
	rres, err := e.T.Run(ctx, "head -c "+fmt.Sprint(maxJobResultBytes+1)+" "+q(resultFile)+" 2>/dev/null || true")
	if err != nil {
		return false, "", err
	}
	evidence, resultErr := parseJobResult([]byte(rres.Stdout))
	if resultErr != nil {
		reason := "result file is missing"
		if !errors.Is(resultErr, errJobResultMissing) {
			reason = "result file is invalid"
		}
		return e.unknownJobResult(job, reason)
	}
	if e.jobResults == nil {
		e.jobResults = make(map[string]journal.JobResultEvidence)
	}
	e.jobResults[job] = evidence
	if evidence.Provider != "" {
		e.progress("migration", "evidence_recorded", "provider revision evidence recorded")
	}
	return !evidence.Changed, jobResultDetail(evidence), nil
}

func injectComposeJobResult(command, hostResultFile, containerResultFile string) (string, bool) {
	runIndex := strings.Index(command, " run ")
	if runIndex < 0 {
		return command, false
	}
	prefix := command[:runIndex]
	if !strings.Contains(prefix, "docker compose") && !strings.Contains(prefix, "docker-compose") {
		return command, false
	}
	flags := " run -e OB_RESULT_FILE=" + containerResultFile +
		" -v " + q(hostResultFile+":"+containerResultFile+":rw") + " "
	return prefix + flags + command[runIndex+len(" run "):], true
}

func (e *Engine) unknownJobResult(job, reason string) (bool, string, error) {
	detail := "changed=unknown (" + reason + "); " + rollbackUnavailableConsequence
	if e.jobDataEffect(job) != "migration" {
		return false, detail, nil
	}
	strongApproval := e.Opts.AllowUnknownMigration && e.Opts.ApprovalDigest != "" &&
		(e.Opts.ApprovalClass == "strong" || e.Opts.ApprovalClass == "break_glass")
	if !strongApproval {
		return false, "", fmt.Errorf(
			"migration job %s completed with changed=unknown (%s); rollout halted before workload replacement because no strong plan-bound approval authorizes that transition — repair the job-result protocol, or abort after compatibility review and re-plan with required approval",
			job, reason,
		)
	}
	e.warnf("migration job %s: %s (authorized by strong plan-bound approval)", job, detail)
	e.progress("migration", "unknown_approved", "changed=unknown transition authorized by strong plan-bound approval")
	return false, detail + " (strong plan-bound approval recorded)", nil
}

func jobResultDetail(evidence journal.JobResultEvidence) string {
	detail := fmt.Sprintf("changed=%t", evidence.Changed)
	if evidence.Provider != "" {
		detail += fmt.Sprintf(" provider=%s before_revisions=%d after_revisions=%d evidence=%s",
			evidence.Provider, len(evidence.BeforeRevisions), len(evidence.AfterRevisions), evidence.Digest)
	}
	return detail
}

// onVerifyFailure decides between auto-rollback and halt-and-page (design
// §06). Auto-rollback needs an OPEN gate (migrate declared no-op) or the
// operator's informed promise (migration_policy: expand-only), and not
// --no-rollback.
func (e *Engine) onVerifyFailure(ctx context.Context, jw *journal.Writer, releaseID, prev string, verr error) error {
	if err := jw.Append(ctx, journal.Record{Phase: "verify", Event: "result", Status: "fail", Detail: verr.Error()}); err != nil {
		return errors.Join(verr, fmt.Errorf("journal verify result: %w", err))
	}
	switch {
	case e.Opts.NoRollback:
		return fmt.Errorf("verify: %w — halting (--no-rollback); release NOT activated", verr)
	case prev == "":
		return fmt.Errorf("verify: %w — first deploy, nothing to roll back to; release NOT activated", verr)
	case !e.rollbackCovered:
		return fmt.Errorf("verify: %w — HALT-AND-PAGE: a job or lifecycle hook has rollback-unknown data effects not covered by a safe result or migration_policy. The release is NOT activated. Investigate, then fix-forward + `ob resume`, or `ob abort --break-migration-gate`", verr)
	}
	replay, err := e.engineFromReleaseSnapshot(ctx, prev)
	if err != nil {
		return fmt.Errorf("verify: %w — automatic rollback refused: %v; release NOT activated", verr, err)
	}
	replay.fenceVal = e.fenceVal
	e.logf("verify failed — auto-rollback to %s (gate open: effects declared safe, changed=false, or expand-only migrations)", prev)
	if err := jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "intent", Detail: "to=" + prev}); err != nil {
		return errors.Join(verr, fmt.Errorf("journal auto-rollback intent: %w", err))
	}
	if err := e.removeNewcomers(ctx, releaseID); err != nil {
		rollbackErr := fmt.Errorf("verify failed (%v) AND auto-rollback could not remove new containers: %w", verr, err)
		if journalErr := jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "result", Status: "fail", Detail: err.Error()}); journalErr != nil {
			return errors.Join(rollbackErr, fmt.Errorf("journal auto-rollback result: %w", journalErr))
		}
		return rollbackErr
	}
	prevCompose := release.PathsFor(e.names()).Releases + "/" + prev + "/compose.yaml"
	if err := replay.releaseRoles(ctx, prevCompose); err != nil {
		rollbackErr := fmt.Errorf("verify failed (%v) AND auto-rollback failed: %w — intervene manually", verr, err)
		if journalErr := jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "result", Status: "fail", Detail: err.Error()}); journalErr != nil {
			return errors.Join(rollbackErr, fmt.Errorf("journal auto-rollback result: %w", journalErr))
		}
		return rollbackErr
	}
	if err := replay.Verify(ctx); err != nil {
		rollbackErr := fmt.Errorf("verify failed (%v) AND the rolled-back release also fails verify: %w — intervene manually", verr, err)
		if journalErr := jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "result", Status: "fail", Detail: err.Error()}); journalErr != nil {
			return errors.Join(rollbackErr, fmt.Errorf("journal auto-rollback result: %w", journalErr))
		}
		return rollbackErr
	}
	rollbackErr := fmt.Errorf("verify: %w — auto-rolled back to %s (healthy); new release NOT activated", verr, prev)
	if err := jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "result", Status: "ok"}); err != nil {
		return errors.Join(rollbackErr, fmt.Errorf("journal auto-rollback result: %w", err))
	}
	return rollbackErr
}

func (e *Engine) jobDataEffect(job string) string {
	if w, ok := e.Spec.Workloads[job]; ok && w.IsJob() && w.DataEffect != "" {
		return w.DataEffect
	}
	return "unknown"
}

func (e *Engine) jobRollbackPolicySafe(service string) bool {
	effect := e.jobDataEffect(service)
	return effect == "none" || e.Spec.Deployment.MigrationPolicy == "expand-only" && effect == "migration"
}

// removeNewcomers stops and removes every container of the given release
// (identified by the ob.release label the render injected).
func (e *Engine) removeNewcomers(ctx context.Context, releaseID string) error {
	for _, roleName := range e.Spec.ReleaseOrder() {
		ids, err := e.newcomerIDs(ctx, roleName, releaseID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := e.mutateChecked(ctx, "remove newcomer "+id, "docker stop -t 10 "+id+" && docker rm "+id); err != nil {
				return err
			}
		}
	}
	return nil
}
