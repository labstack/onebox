package engine

import (
	"context"
	"fmt"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// FindIncomplete returns the newest journal that started but never finished
// or aborted — the deploy a crashed runner left behind.
func (e *Engine) FindIncomplete(ctx context.Context) (journal.Summary, error) {
	ids, err := journal.List(ctx, e.T, e.Cfg.App)
	if err != nil {
		return journal.Summary{}, err
	}
	for i := len(ids) - 1; i >= 0; i-- {
		recs, err := journal.Read(ctx, e.T, e.Cfg.App, ids[i])
		if err != nil {
			return journal.Summary{}, err
		}
		s := journal.Summarize(recs)
		if s.Started && !s.Finished && !s.Aborted {
			return s, nil
		}
	}
	return journal.Summary{}, fmt.Errorf("no incomplete deploy found in the journal")
}

// Resume continues an interrupted deploy from the journal: completed phases
// and roles skip; the half-rolled role is adopted via its ob.release label.
// A NEW lock epoch is taken, which fences the old runner if it still lives.
func (e *Engine) Resume(ctx context.Context) error {
	s, err := e.FindIncomplete(ctx)
	if err != nil {
		return err
	}
	e.logf("resuming %s (started %s by %s; done: transfer=%v migrate=%v)",
		s.DeployID, s.StartedAt, s.Operator, s.Done["transfer"], s.Done["migrate"])
	e.gateOpen = s.GateOpen // the journal remembers what migrate declared
	return e.deployCore(ctx, s.DeployID, "", s.Done)
}

// Abort reverts an interrupted deploy to the previous release. The migration
// gate governs abort exactly like auto-rollback (design §06 rev 4): aborting
// after a schema change is the same hazard.
func (e *Engine) Abort(ctx context.Context, force bool) error {
	s, err := e.FindIncomplete(ctx)
	if err != nil {
		return err
	}
	if !s.GateOpen && e.Cfg.Migrations != "expand-only" && !force {
		return fmt.Errorf("abort refused — HALT-AND-PAGE: deploy %s ran a migrate step that did not declare changed=false, so reverting could put old code against a new schema. Fix-forward + `ob resume`, or `ob abort --force` if you know the schema is compatible", s.DeployID)
	}
	epoch, err := e.AcquireLock(ctx, s.DeployID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, s.DeployID, epoch); err != nil {
		return err
	}
	jw := &journal.Writer{T: e.T, App: e.Cfg.App, DeployID: s.DeployID, Epoch: epoch, Operator: journal.DefaultOperator()}
	_ = jw.Append(ctx, journal.Record{Phase: "abort", Event: "intent", Detail: "to=" + s.PrevRelease})

	if s.PrevRelease == "" {
		e.logf("abort: first deploy — removing its containers, nothing to restore")
		if err := e.removeNewcomers(ctx, s.DeployID); err != nil {
			return err
		}
		_ = jw.Append(ctx, journal.Record{Phase: "abort", Event: "abort", Status: "ok"})
		return nil
	}

	// Replay the previous release through the normal choreography: RollRole
	// adopts prev-labeled containers as no-ops and drains this deploy's
	// newcomers as "old" — a zero-downtime abort for rolling roles. Recreate
	// roles are skipped when they already run the previous release.
	prevCompose := release.PathsFor(e.Cfg.App).Releases + "/" + s.PrevRelease + "/compose.yaml"
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		if role.Mode == "recreate" {
			prevIDs, err := e.newcomerIDs(ctx, role.Service, s.PrevRelease)
			if err != nil {
				return err
			}
			if len(prevIDs) > 0 {
				e.logf("abort: %s already runs %s — untouched", roleName, s.PrevRelease)
				continue
			}
		}
		e.logf("abort: reverting %s to %s", roleName, s.PrevRelease)
		var rerr error
		if role.Mode == "rolling" {
			rerr = e.RollRole(ctx, roleName, prevCompose)
		} else {
			rerr = e.RecreateRole(ctx, roleName, prevCompose)
		}
		if rerr != nil {
			_ = jw.Append(ctx, journal.Record{Phase: "abort", Event: "abort", Status: "fail", Detail: rerr.Error()})
			return fmt.Errorf("abort %s: %w — intervene manually", roleName, rerr)
		}
	}
	// straggler sweep: failed-join leftovers of the aborted release
	if err := e.removeNewcomers(ctx, s.DeployID); err != nil {
		return err
	}
	if err := e.Verify(ctx); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "abort", Event: "abort", Status: "fail", Detail: err.Error()})
		return fmt.Errorf("abort verify: %w", err)
	}
	_ = jw.Append(ctx, journal.Record{Phase: "abort", Event: "abort", Status: "ok"})
	e.logf("aborted %s — %s serving", s.DeployID, s.PrevRelease)
	return nil
}
