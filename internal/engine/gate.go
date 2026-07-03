package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/release"
)

// runMigrate executes the migrate hook with the gate protocol (design §06):
// $YEET_RESULT_FILE is exported; a hook that writes changed=false declares a
// no-op and the gate stays OPEN (auto-rollback permitted). Anything else —
// including writing nothing — fails safe: the deploy is treated as having
// applied schema changes.
func (e *Engine) runMigrate(ctx context.Context, remoteDir, remoteCompose string) (string, error) {
	hook, ok := e.Cfg.Hooks["migrate"]
	if !ok || hook.Run == "" {
		e.gateOpen = true
		return "changed=false (no migrate hook)", nil
	}
	if hook.Local {
		// a local migrate hook can't use the host result file — fail safe
		e.gateOpen = false
		if err := e.RunHook(ctx, "migrate", remoteDir, remoteCompose); err != nil {
			return "", err
		}
		return "changed=unknown (local migrate hook — gate closed, fail-safe)", nil
	}
	resultFile := remoteDir + "/.migrate-result"
	e.logf("hook migrate: %s", hook.Run)
	cmd := "cd " + q(remoteDir) +
		" && rm -f " + q(resultFile) +
		" && COMPOSE_PROJECT_NAME=" + e.Cfg.App +
		" COMPOSE_FILE=" + q(remoteCompose) +
		" YEET_RESULT_FILE=" + q(resultFile) + " " + hook.Run
	res, err := e.mutate(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("migrate hook failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	rres, err := e.T.Run(ctx, "cat "+q(resultFile)+" 2>/dev/null || true")
	if err != nil {
		return "", err
	}
	if strings.Contains(rres.Stdout, "changed=false") {
		e.gateOpen = true
		return "changed=false", nil
	}
	e.gateOpen = false
	return "changed=unknown (no result declared — gate closed, fail-safe)", nil
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
