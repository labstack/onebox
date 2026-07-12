package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// gateSteps is the ordered set of jobs run at the pre-release gate: every
// `jobs` service, plus a legacy `migrate` hook that isn't itself a job (older
// configs ran the migrate hook whether or not migrate was classified a job).
func (e *Engine) gateSteps() []string {
	steps := append([]string{}, e.Cfg.Jobs...)
	if h, ok := e.Cfg.Hooks["migrate"]; ok && h.Run != "" {
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
// $OB_RESULT_FILE protocol (design §06). A step with a same-named hook runs
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
		// the reconstructed aggregate unchanged, including fail-closed legacy
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
				e.logf("%s: already complete (resume) — aggregate gate stays closed", key)
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
			_ = jw.Append(ctx, journal.Record{
				Phase: "pre-release", SubStep: key, Event: "result", Status: "fail", Detail: err.Error(),
				RollbackPolicySafe: policySafe,
			})
			return err
		}
		if !safe {
			allSafe = false
		}
		if !safe && !policySafe {
			allCovered = false
		}
		if err := jw.Append(ctx, journal.Record{
			Phase: "pre-release", SubStep: key, Event: "result", Status: "ok", Detail: detail,
			RollbackSafe: safe, RollbackPolicySafe: policySafe,
		}); err != nil {
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
	runCmd := e.composeCmd(remoteCompose) + " run --rm --no-deps " + job
	if h, ok := e.Cfg.Hooks[job]; ok && h.Run != "" {
		if h.Local {
			// a local hook can't reach the host result file — run it, fail safe
			e.logf("job %s (local hook): %s", job, h.Run)
			if err := e.RunHook(ctx, job, remoteDir, remoteCompose); err != nil {
				return false, "", err
			}
			if safeByDeclaration {
				return true, "rollback-safe by data_effect=none declaration", nil
			}
			return false, "changed=unknown (local hook — gate closed, fail-safe)", nil
		}
		runCmd = h.Run // custom command for this job (e.g. extra flags)
	}
	e.ui.Cmd("job", runCmd) // verbose only — the plan lists it
	resultFile := remoteDir + "/.job-" + job + "-result"
	cmd := "cd " + q(remoteDir) +
		" && rm -f " + q(resultFile) +
		" && COMPOSE_PROJECT_NAME=" + e.Cfg.App +
		" COMPOSE_FILE=" + q(remoteCompose) +
		" OB_RESULT_FILE=" + q(resultFile) +
		" " + runCmd
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
	rres, err := e.T.Run(ctx, "cat "+q(resultFile)+" 2>/dev/null || true")
	if err != nil {
		return false, "", err
	}
	if strings.Contains(rres.Stdout, "changed=false") {
		return true, "changed=false", nil
	}
	return false, "changed=unknown (no result declared — gate closed, fail-safe)", nil
}

// onVerifyFailure decides between auto-rollback and halt-and-page (design
// §06). Auto-rollback needs an OPEN gate (migrate declared no-op) or the
// operator's informed promise (migration_policy: expand-only), and not
// --no-rollback.
func (e *Engine) onVerifyFailure(ctx context.Context, jw *journal.Writer, releaseID, prev string, verr error) error {
	_ = jw.Append(ctx, journal.Record{Phase: "verify", Event: "result", Status: "fail", Detail: verr.Error()})
	switch {
	case e.Opts.NoRollback:
		return fmt.Errorf("verify: %w — halting (--no-rollback); release NOT activated", verr)
	case prev == "":
		return fmt.Errorf("verify: %w — first deploy, nothing to roll back to; release NOT activated", verr)
	case !e.rollbackCovered:
		return fmt.Errorf("verify: %w — HALT-AND-PAGE: a job or lifecycle hook has rollback-unknown data effects not covered by a safe result or migration_policy. The release is NOT activated. Investigate, then fix-forward + `ob resume`, or `ob abort --force`", verr)
	}
	e.logf("verify failed — auto-rollback to %s (gate open: effects declared safe, changed=false, or expand-only migrations)", prev)
	_ = jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "intent", Detail: "to=" + prev})
	if err := e.removeNewcomers(ctx, releaseID); err != nil {
		return fmt.Errorf("verify failed (%v) AND auto-rollback could not remove new containers: %w", verr, err)
	}
	prevCompose := release.PathsFor(e.Cfg.App).Releases + "/" + prev + "/compose.yaml"
	if err := e.releaseRoles(ctx, prevCompose); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "result", Status: "fail", Detail: err.Error()})
		return fmt.Errorf("verify failed (%v) AND auto-rollback failed: %w — intervene manually", verr, err)
	}
	if err := e.Verify(ctx); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "result", Status: "fail", Detail: err.Error()})
		return fmt.Errorf("verify failed (%v) AND the rolled-back release also fails verify: %w — intervene manually", verr, err)
	}
	_ = jw.Append(ctx, journal.Record{Phase: "auto-rollback", Event: "result", Status: "ok"})
	return fmt.Errorf("verify: %w — auto-rolled back to %s (healthy); new release NOT activated", verr, prev)
}

func (e *Engine) jobDataEffect(service string) string {
	for _, component := range e.Cfg.Components {
		if component.Type == "job" && component.Service == service {
			return component.DataEffect
		}
	}
	return "unknown"
}

func (e *Engine) jobRollbackPolicySafe(service string) bool {
	effect := e.jobDataEffect(service)
	return effect == "none" || e.Cfg.Migrations == "expand-only" && effect == "migration"
}

// removeNewcomers stops and removes every container of the given release
// (identified by the ob.release label the render injected).
func (e *Engine) removeNewcomers(ctx context.Context, releaseID string) error {
	for _, role := range e.Cfg.Roles {
		ids, err := e.newcomerIDs(ctx, role.Service, releaseID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := e.mutate(ctx, "docker stop -t 10 "+id+" && docker rm "+id); err != nil {
				return err
			}
		}
	}
	return nil
}
