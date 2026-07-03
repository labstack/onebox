package engine

import (
	"context"
	"fmt"

	"github.com/labstack/yeet/internal/journal"
)

// Audit prints who deployed what, when, from which SHA — including runs
// whose terminal scrolled away (design §05: that's the point).
func (e *Engine) Audit(ctx context.Context, n int) error {
	ids, err := journal.List(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Fprintln(e.Opts.Out, "no journals — nothing deployed through yeet yet")
		return nil
	}
	if n > 0 && len(ids) > n {
		ids = ids[len(ids)-n:]
	}
	fmt.Fprintf(e.Opts.Out, "%-32s %-20s %-9s %-11s %s\n", "DEPLOY", "OPERATOR", "GIT", "OUTCOME", "STARTED")
	for i := len(ids) - 1; i >= 0; i-- {
		recs, err := journal.Read(ctx, e.T, e.Cfg.App, ids[i])
		if err != nil {
			return err
		}
		s := journal.Summarize(recs)
		outcome := "INCOMPLETE"
		switch {
		case s.Aborted:
			outcome = "aborted"
		case s.Finished && !s.Failed:
			outcome = "deployed"
		case s.Failed:
			outcome = "failed"
		}
		git := s.GitSHA
		if git == "" {
			git = "-"
		}
		fmt.Fprintf(e.Opts.Out, "%-32s %-20s %-9s %-11s %s\n", s.DeployID, s.Operator, git, outcome, s.StartedAt)
	}
	return nil
}
