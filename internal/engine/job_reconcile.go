package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/journal"
)

// refuseForeignJobContainers refuses when a one-off job container from another
// operation is still running on the host.
//
// The question "is a job still running" is asked of Docker, not of the journal.
// A journal says what a client managed to write, and the whole failure mode
// here is a client that did not write. An interrupted run that DID record its
// interruption looks finished on paper while its container keeps changing data,
// and a plan re-run appends a second invocation to the same journal — both are
// invisible to any reduction over records, and both are one `docker ps` away.
func (e *Engine) refuseForeignJobContainers(ctx context.Context, currentOperationID string) error {
	containers, err := e.jobContainers(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.operation == currentOperationID {
			continue
		}
		return fmt.Errorf(
			"a job container from operation %s is still running on this host (%.12s) with no process owning it; "+
				"wait for it to finish, or establish what it did and stop it with `docker rm -f %.12s`",
			c.operation, c.id, c.id)
	}
	return nil
}

type jobContainer struct {
	id        string
	operation string
}

// jobContainers lists every running one-off job container, whichever operation
// created it. The label is unvalued in the filter so this finds containers of
// operations this process knows nothing about, which is the point.
func (e *Engine) jobContainers(ctx context.Context) ([]jobContainer, error) {
	res, err := e.T.Run(ctx,
		"docker ps --filter label="+q(JobOperationLabel)+
			" --format "+q("{{.ID}} {{.Label \""+JobOperationLabel+"\"}}"))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list running job containers (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var out []jobContainer
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		id, operation, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found || id == "" {
			continue
		}
		if !validID.MatchString(id) {
			return nil, fmt.Errorf("suspicious container id %q from docker ps — refusing to reuse in a command", id)
		}
		out = append(out, jobContainer{id: id, operation: operation})
	}
	return out, nil
}

// closeInterruptedJobRuns writes the terminal record an interrupted client
// could not, so an operation stops being incomplete forever.
//
// Housekeeping, not safety: the refusal above is what protects a live
// container, and this runs only once nothing of the sort is running. Being
// wrong here is therefore cheap, which is why a journal reduction is good
// enough for it and is not good enough for the refusal.
func (e *Engine) closeInterruptedJobRuns(ctx context.Context) error {
	ids, byID, err := journal.Journals(ctx, e.T, e.names())
	if err != nil {
		return err
	}
	for _, id := range ids {
		for _, run := range unfinishedJobRuns(byID[id]) {
			if err := e.closeJobRun(ctx, id, run); err != nil {
				return err
			}
		}
	}
	return nil
}

// unfinishedJobRun is one invocation that never reached a terminal record.
type unfinishedJobRun struct {
	Epoch int
	Job   string
	// ResultRecorded is true when the client journaled the job's own result
	// before it went away. That is the only proof of the outcome that survives:
	// the container is `--rm`, so nothing about it outlives its exit. A
	// recorded failure is evidence exactly as much as a recorded success — only
	// an absent result means the outcome is unknown.
	ResultRecorded bool
	ResultOK       bool
}

// unfinishedJobRuns groups a journal by epoch, because a journal holds one
// invocation per epoch and a plan may legitimately be run more than once. A
// finish in an earlier epoch says nothing about a later one.
func unfinishedJobRuns(records []journal.Record) []unfinishedJobRun {
	type state struct {
		started, finished bool
		run               unfinishedJobRun
	}
	byEpoch, order := map[int]*state{}, []int{}
	for _, r := range records {
		if r.Phase != "job" {
			continue
		}
		s, seen := byEpoch[r.Epoch]
		if !seen {
			s = &state{run: unfinishedJobRun{Epoch: r.Epoch}}
			byEpoch[r.Epoch], order = s, append(order, r.Epoch)
		}
		switch {
		case r.Event == "start" && r.OperationKind == "job_run":
			s.started, s.run.Job = true, r.Service
		case r.Event == "finish":
			s.finished = true
		case r.Event == "result" && strings.HasPrefix(r.SubStep, "job:"):
			// The result record is written by the shared job phase and carries
			// no operation kind, so it is matched on its own shape.
			s.run.ResultRecorded = true
			s.run.ResultOK = r.Status == "ok"
		}
	}
	var out []unfinishedJobRun
	for _, epoch := range order {
		if s := byEpoch[epoch]; s.started && !s.finished {
			out = append(out, s.run)
		}
	}
	return out
}

// closeJobRun records the outcome, never inventing one. Only a result the
// client itself journaled proves the job succeeded; anything else is recorded
// interrupted, which is what an unknown outcome actually is.
func (e *Engine) closeJobRun(ctx context.Context, operationID string, run unfinishedJobRun) error {
	record := journal.Record{
		Phase: "job", Event: "finish", Status: "fail", ErrorCode: "interrupted",
		OperationKind: "job_run", Service: run.Job,
	}
	if run.ResultRecorded {
		// The outcome was observed and written down. Interrupted describes an
		// unknown outcome, and saying it of a known failure hides that the job
		// ran and failed on its own terms.
		record.ErrorCode = ""
		if run.ResultOK {
			record.Status = "ok"
		}
	}
	// The epoch is what groups a journal into invocations, so a terminal record
	// written without it lands in an invocation of its own and leaves the one it
	// was meant to close still open.
	writer := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: operationID, Epoch: run.Epoch,
		Operator: journal.DefaultOperator(),
	}
	if err := writer.Append(ctx, record); err != nil {
		return fmt.Errorf("close interrupted job run %s: %w", operationID, err)
	}
	e.logf("closed interrupted job run %s (%s) as %s", operationID, run.Job, record.Status)
	return nil
}
