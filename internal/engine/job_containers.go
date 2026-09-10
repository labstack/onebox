package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
func (e *Engine) refuseForeignJobContainers(ctx context.Context, currentOperationID string, currentEpoch int) error {
	containers, err := e.jobContainers(ctx)
	if err != nil {
		return err
	}
	currentEpochLabel := strconv.Itoa(currentEpoch)
	for _, c := range containers {
		// Operation AND epoch. A sealed job plan is re-runnable and carries one
		// operation id for its whole life, and AcquireLock hands the lock
		// straight back to a caller presenting the id already written in it. So
		// a second run of one plan would reclaim the lock from a live first run
		// and then exempt that run's container as its own — two concurrent
		// data-changing containers, which is the single thing this prevents.
		if c.operation == currentOperationID && c.epoch == currentEpochLabel {
			continue
		}
		if c.operation == currentOperationID {
			// A differing epoch says the container belongs to some other
			// invocation of this operation, not which one or when — epochs are
			// not ordered against each other here — and a missing epoch says
			// only that it cannot be placed at all. Neither supports calling it
			// an earlier run.
			if c.epoch == "" {
				return fmt.Errorf(
					"a job container of operation %s is running on this host (%.12s) carrying no %s label, "+
						"so it cannot be placed against this run; establish what it did and stop it with "+
						"`docker rm -f %s`",
					c.operation, c.id, JobEpochLabel, c.id)
			}
			return fmt.Errorf(
				"another invocation of operation %s (epoch %s, this run is epoch %s) left a job container "+
					"running on this host (%.12s); wait for it to finish, or establish what it did and stop "+
					"it with `docker rm -f %s`",
				c.operation, c.epoch, currentEpochLabel, c.id, c.id)
		}
		if c.operation == "" {
			// The label is present but carries no value, so the container
			// cannot be attributed. Refuse anyway: an unattributable job
			// container is exactly as dangerous as an attributable one.
			return fmt.Errorf(
				"a job container is running on this host (%.12s) with an empty %s label, so the operation that "+
					"started it cannot be identified; establish what it did and stop it with `docker rm -f %s`",
				c.id, JobOperationLabel, c.id)
		}
		return fmt.Errorf(
			"a job container from operation %s is still running on this host (%.12s); "+
				"if that operation is still in progress, wait for it — otherwise establish what it did "+
				"and stop it with `docker rm -f %s`",
			c.operation, c.id, c.id)
	}
	return nil
}

type jobContainer struct {
	id        string
	operation string
	epoch     string
}

// jobContainers lists every running one-off job container, whichever operation
// created it. The label is unvalued in the filter so this finds containers of
// operations this process knows nothing about, which is the point.
func (e *Engine) jobContainers(ctx context.Context) ([]jobContainer, error) {
	res, err := e.T.Run(ctx,
		"docker ps --filter label="+q(JobOperationLabel)+
			" --format "+q("{{.ID}} {{.Label \""+JobOperationLabel+"\"}} {{.Label \""+JobEpochLabel+"\"}}"))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list running job containers (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var out []jobContainer
	for _, line := range strings.Split(res.Stdout, "\n") {
		// Not TrimSpace before the cut: a container whose label carries no value
		// prints "<id> " with nothing after the separator, and trimming the line
		// first removes the separator itself — the container would then be
		// skipped as unparseable, which is precisely the one that most needs
		// refusing.
		id, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		operation, epoch, _ := strings.Cut(rest, " ")
		id, operation, epoch = strings.TrimSpace(id), strings.TrimSpace(operation), strings.TrimSpace(epoch)
		if id == "" {
			continue
		}
		if !validID.MatchString(id) {
			return nil, fmt.Errorf("suspicious container id %q from docker ps — refusing to reuse in a command", id)
		}
		out = append(out, jobContainer{id: id, operation: operation, epoch: epoch})
	}
	return out, nil
}
