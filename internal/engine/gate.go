package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/release"
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
// $YEET_RESULT_FILE protocol (design §06). A step with a same-named hook runs
// that hook's command (a custom migrate invocation); otherwise yeet auto-runs
// `docker compose run --rm --no-deps <job>` — so `jobs: [migrate]` needs no
// hook at all. The rollback gate opens only if EVERY step declared
// changed=false; anything else (a real change, no result written, a local
// hook, or a step skipped on resume) fails safe — auto-rollback is withheld
// unless migrations:expand-only is asserted.
func (e *Engine) runJobs(ctx context.Context, jw *journal.Writer, done map[string]bool, remoteDir, remoteCompose string) error {
	steps := e.gateSteps()
	if len(steps) == 0 {
		e.gateOpen = true
		return nil
	}
	allSafe := true
	for _, job := range steps {
		key := "job:" + job
		if done[key] || done["migrate"] { // "migrate" = pre-jobs journal key
			e.logf("%s: already complete (resume) — gate stays closed (result unverifiable)", key)
			allSafe = false
			continue
		}
		_ = jw.Append(ctx, journal.Record{Phase: "pre-release", SubStep: key, Event: "intent"})
		st := e.ui.Step("job "+job, true)
		safe, detail, err := e.runOneJob(ctx, job, remoteDir, remoteCompose)
		if err == nil {
			e.logf("job %s: %s", job, detail)
		}
		st(err)
		if err != nil {
			_ = jw.Append(ctx, journal.Record{Phase: "pre-release", SubStep: key, Event: "result", Status: "fail", Detail: err.Error()})
			return err
		}
		if !safe {
			allSafe = false
		}
		_ = jw.Append(ctx, journal.Record{Phase: "pre-release", SubStep: key, Event: "result", Status: "ok", Detail: detail})
	}
	e.gateOpen = allSafe
	return nil
}

// runOneJob runs a single gate step and reports whether it declared itself
// rollback-safe (changed=false). Returns (safe, detail, err).
func (e *Engine) runOneJob(ctx context.Context, job, remoteDir, remoteCompose string) (bool, string, error) {
	runCmd := e.composeCmd(remoteCompose) + " run --rm --no-deps " + job
	if h, ok := e.Cfg.Hooks[job]; ok && h.Run != "" {
		if h.Local {
			// a local hook can't reach the host result file — run it, fail safe
			e.logf("job %s (local hook): %s", job, h.Run)
			if err := e.RunHook(ctx, job, remoteDir, remoteCompose); err != nil {
				return false, "", err
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
		" YEET_RESULT_FILE=" + q(resultFile) + " " + runCmd
	res, err := e.mutate(ctx, cmd)
	if err != nil {
		return false, "", err
	}
	if res.ExitCode != 0 {
		return false, "", fmt.Errorf("job %s failed (exit %d): %s", job, res.ExitCode, strings.TrimSpace(res.Stderr))
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
// operator's informed promise (migrations: expand-only), and not
// --no-rollback.
func (e *Engine) onVerifyFailure(ctx context.Context, jw *journal.Writer, releaseID, prev string, verr error) error {
	_ = jw.Append(ctx, journal.Record{Phase: "verify", Event: "result", Status: "fail", Detail: verr.Error()})
	expandOnly := e.Cfg.Migrations == "expand-only"
	switch {
	case e.Opts.NoRollback:
		return fmt.Errorf("verify: %w — halting (--no-rollback); release NOT activated", verr)
	case prev == "":
		return fmt.Errorf("verify: %w — first deploy, nothing to roll back to; release NOT activated", verr)
	case !e.gateOpen && !expandOnly:
		return fmt.Errorf("verify: %w — HALT-AND-PAGE: the migrate step did not declare changed=false and migrations is not expand-only, so auto-rollback could put old code against a new schema. The release is NOT activated. Investigate, then fix-forward + `yeet resume`, or `yeet abort --force`", verr)
	}
	e.logf("verify failed — auto-rollback to %s (gate open: migrate no-op or expand-only asserted)", prev)
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

// removeNewcomers stops and removes every container of the given release
// (identified by the yeet.release label the render injected).
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
