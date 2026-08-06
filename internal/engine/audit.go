package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/labstack/onebox/internal/journal"
)

// Audit prints who did what, when, from which SHA — including runs whose
// terminal scrolled away.
//
// A row is one invocation, not one file. A rollback appends to the journal of
// the release it restores, so a reader that summarises per file reports the
// rollback's timestamp under the original deploy's name and never says a
// rollback happened at all — which loses the one event an operator reading an
// audit trail after an incident is looking for. The epoch separates them: it
// counts invocations against the app, and every record carries it.
func (e *Engine) Audit(ctx context.Context, n int) error {
	ids, err := journal.List(ctx, e.T, e.names())
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Fprintln(e.Opts.Out, "no journals — nothing deployed through ob yet")
		return nil
	}

	var rows []auditRow
	for _, id := range ids {
		recs, err := journal.Read(ctx, e.T, e.names(), id)
		if err != nil {
			return err
		}
		rows = append(rows, auditRows(recs)...)
	}
	// Most recent first, and the epoch breaks ties within a second.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].startedAt != rows[j].startedAt {
			return rows[i].startedAt > rows[j].startedAt
		}
		return rows[i].epoch > rows[j].epoch
	})
	if n > 0 && len(rows) > n {
		rows = rows[:n]
	}

	// Sized to the content: release identifiers carry the deploy kind, so a
	// fixed width that fits a "deploy" pushes every later column out of line
	// on a "service_apply" and the table stops being readable as a table.
	width := len("RELEASE")
	for _, r := range rows {
		if len(r.deployID) > width {
			width = len(r.deployID)
		}
	}
	format := fmt.Sprintf("%%-%ds %%-14s %%-20s %%-9s %%-12s %%s\n", width)
	fmt.Fprintf(e.Opts.Out, format, "RELEASE", "ACTION", "OPERATOR", "GIT", "OUTCOME", "STARTED")
	for _, r := range rows {
		git := r.gitSHA
		if git == "" {
			git = "-"
		}
		fmt.Fprintf(e.Opts.Out, format, r.deployID, r.action, r.operator, git, r.outcome, r.startedAt)
	}
	return nil
}

type auditRow struct {
	deployID  string
	epoch     int
	action    string
	operator  string
	gitSHA    string
	outcome   string
	startedAt string
}

// auditRows splits one journal into the invocations that wrote it.
func auditRows(recs []journal.Record) []auditRow {
	var order []int
	byEpoch := map[int][]journal.Record{}
	for _, r := range recs {
		if _, seen := byEpoch[r.Epoch]; !seen {
			order = append(order, r.Epoch)
		}
		byEpoch[r.Epoch] = append(byEpoch[r.Epoch], r)
	}

	var out []auditRow
	for _, epoch := range order {
		group := byEpoch[epoch]
		row := auditRow{
			deployID:  group[0].DeployID,
			epoch:     epoch,
			action:    auditAction(group[0].Phase),
			startedAt: group[0].TS,
			outcome:   "INCOMPLETE",
		}
		for _, r := range group {
			if r.Operator != "" {
				row.operator = r.Operator
			}
			if r.GitSHA != "" {
				row.gitSHA = r.GitSHA
			}
			if r.Event == "start" && r.Phase != "" {
				row.action = auditAction(r.Phase)
			}
			switch {
			case r.Event == "abort":
				row.outcome = "aborted"
			case r.Status == "fail":
				row.outcome = "failed"
			case r.Event == "finish" && r.Status == "ok" && row.outcome != "failed":
				row.outcome = auditOutcome(row.action)
			}
		}
		out = append(out, row)
	}
	return out
}

// auditAction names the invocation in the operator's vocabulary rather than
// the journal's. "service-apply" is an internal phase name; the person who
// ran the command typed `ob service apply`.
func auditAction(phase string) string {
	switch phase {
	case "accessory-apply":
		return "service apply"
	case "":
		return "deploy"
	default:
		return phase
	}
}

func auditOutcome(action string) string {
	switch action {
	case "rollback":
		return "rolled back"
	case "bootstrap":
		return "bootstrapped"
	case "service apply":
		return "applied"
	default:
		return "deployed"
	}
}
