package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/journal"
)

// ScheduleApply explicitly converges the host units generated for scheduled
// jobs without deploying a release. Package installation stays local and
// side-effect free; an operator chooses when a newer runner may rewrite remote
// units.
//
// The application lock also serializes against deploys. AcquireLock takes the
// schedule flock while publishing that lock, so a timer cannot begin between
// the ownership check and the fence write.
func (e *Engine) ScheduleApply(ctx context.Context, operationID string) (err error) {
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	jobs, err := e.Spec.ScheduledJobs()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		names = append(names, job.Name)
	}
	detail := strings.Join(names, ",")
	if detail == "" {
		detail = "none"
	}

	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if len(jobs) > 0 {
		current, readErr := e.T.Run(ctx, "test -f "+q(e.names().CurrentLink()+"/compose.yaml")+" && echo ok || true")
		if readErr != nil {
			return readErr
		}
		if strings.TrimSpace(current.Stdout) != "ok" {
			return errors.New("cannot apply schedules before the first release; deploy the application first")
		}
	}
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return err
	}

	jw := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: operationID, Epoch: epoch,
		Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA,
		ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner,
	}
	if err := jw.Append(ctx, journal.Record{Phase: "schedule-apply", Event: "start", Detail: detail}); err != nil {
		return fmt.Errorf("journal schedule apply start: %w", err)
	}
	defer func() {
		finish := journal.Record{Phase: "schedule-apply", Event: "finish", Status: "ok"}
		if err != nil {
			finish.Status = "fail"
			finish.Detail = err.Error()
		}
		if journalErr := jw.Append(ctx, finish); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal schedule apply finish: %w", journalErr))
		}
	}()

	return e.SyncSchedules(ctx)
}
