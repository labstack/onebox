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
	rows, err := e.AuditSnapshot(ctx, n)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Opts.Out, "no journals — nothing deployed through ob yet")
		return nil
	}

	width := len("RELEASE")
	for _, r := range rows {
		if len(r.ReleaseID) > width {
			width = len(r.ReleaseID)
		}
	}
	// The action cell names a job run's job, so its width is not knowable in
	// advance the way a fixed column would need. "schedule resume" already
	// overflowed the old fixed 14. Built once and indexed alongside rows: the
	// width scan and the print loop want the same string.
	cells := make([]string, len(rows))
	action := len("ACTION")
	for i, r := range rows {
		cells[i] = auditActionCell(r)
		if len(cells[i]) > action {
			action = len(cells[i])
		}
	}
	format := fmt.Sprintf("%%-%ds %%-%ds %%-20s %%-9s %%-12s %%s\n", width, action)
	fmt.Fprintf(e.Opts.Out, format, "RELEASE", "ACTION", "OPERATOR", "GIT", "OUTCOME", "STARTED")
	for i, r := range rows {
		git := r.GitSHA
		if git == "" {
			git = "-"
		}
		fmt.Fprintf(e.Opts.Out, format, r.ReleaseID, cells[i], r.Operator, git, r.Outcome, r.StartedAt)
		if r.Action == "exec" {
			fmt.Fprintf(e.Opts.Out, "  target=%s (%s) command_digest=%s reason=%s\n", r.Target, r.TargetKind, r.CommandDigest, r.Reason)
		}
	}
	return nil
}

type AuditRecord struct {
	ReleaseID     string `json:"release_id"`
	Epoch         int    `json:"epoch"`
	Action        string `json:"action"`
	Service       string `json:"service,omitempty"`
	Operator      string `json:"operator"`
	GitSHA        string `json:"git_sha,omitempty"`
	Outcome       string `json:"outcome"`
	StartedAt     string `json:"started_at"`
	Target        string `json:"target,omitempty"`
	TargetKind    string `json:"target_kind,omitempty"`
	CommandDigest string `json:"command_digest,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

func (e *Engine) AuditSnapshot(ctx context.Context, n int) ([]AuditRecord, error) {
	ids, err := journal.List(ctx, e.T, e.names())
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []AuditRecord{}, nil
	}

	var rows []auditRow
	for _, id := range ids {
		recs, err := journal.Read(ctx, e.T, e.names(), id)
		if err != nil {
			return nil, err
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

	records := make([]AuditRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, AuditRecord{
			ReleaseID: row.deployID, Epoch: row.epoch, Action: row.action, Service: row.service,
			Operator: row.operator, GitSHA: row.gitSHA, Outcome: row.outcome, StartedAt: row.startedAt,
			Target: row.target, TargetKind: row.targetKind, CommandDigest: row.commandDigest, Reason: row.reason,
		})
	}
	return records, nil
}

type auditRow struct {
	deployID      string
	epoch         int
	action        string
	service       string
	operator      string
	gitSHA        string
	outcome       string
	startedAt     string
	target        string
	targetKind    string
	commandDigest string
	reason        string
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
			if r.Service != "" {
				row.service = r.Service
			}
			if r.Target != "" {
				row.target = r.Target
				row.targetKind = r.TargetKind
				row.commandDigest = r.CommandDigest
				row.reason = r.Reason
			}
			if r.Event == "start" && r.Phase != "" {
				row.action = auditAction(r.Phase)
			}
			switch {
			// Ahead of the failure arm, which this record also matches: an
			// interrupted run is not a failure of the job. The client went away
			// and the outcome is unknown, which is a different thing to tell an
			// operator than "it failed" or "it never finished".
			case r.ErrorCode == "interrupted":
				row.outcome = "interrupted"
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
	case "service-apply":
		return "service apply"
	case "schedule-run":
		return "schedule run"
	case "schedule-pause":
		return "schedule pause"
	case "schedule-resume":
		return "schedule resume"
	case "job":
		return "job run"
	case "":
		return "deploy"
	default:
		return phase
	}
}

// auditActionCell is the ACTION column. A job run names its job: the journal
// carries it on Service, and without it every sealed job run reads as an
// anonymous "job run" — which is the one thing a reader is looking for.
func auditActionCell(r AuditRecord) string {
	if r.Service == "" {
		return r.Action
	}
	return r.Action + " " + r.Service
}

func auditOutcome(action string) string {
	switch action {
	case "rollback":
		return "rolled back"
	case "bootstrap":
		return "bootstrapped"
	case "service apply":
		return "applied"
	case "exec":
		return "succeeded"
	case "schedule run":
		return "started"
	case "schedule pause":
		return "paused"
	case "schedule resume":
		return "resumed"
	case "job run":
		return "succeeded"
	default:
		return "deployed"
	}
}
