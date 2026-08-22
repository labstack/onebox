package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// StatusSchedule is the host-observed state of one declared scheduled job.
// systemd keeps Result after a oneshot exits, so a failed or timed-out run stays
// visible until a later successful run clears it.
type StatusSchedule struct {
	Name           string   `json:"name"`
	Unit           string   `json:"unit"`
	TimerState     string   `json:"timer_state"`
	LastResult     string   `json:"last_result"`
	LastExitStatus int      `json:"last_exit_status,omitempty"`
	Diverged       bool     `json:"diverged"`
	Issues         []string `json:"issues,omitempty"`
}

type scheduleUnitObservation struct {
	loadState   string
	activeState string
	result      string
	exitStatus  int
}

func (e *Engine) scheduleStatuses(ctx context.Context) ([]StatusSchedule, error) {
	jobs, err := e.Spec.ScheduledJobs()
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return []StatusSchedule{}, nil
	}

	var commands []string
	for _, job := range jobs {
		unit := e.names().ScheduledJobUnit(job.Name)
		commands = append(commands,
			"printf '%s\\n' "+q("@@"+job.Name+":service"),
			"systemctl show "+q(unit+".service")+" --no-pager --property=LoadState --property=ActiveState --property=Result --property=ExecMainStatus",
			"printf '%s\\n' "+q("@@"+job.Name+":timer"),
			"systemctl show "+q(unit+".timer")+" --no-pager --property=LoadState --property=ActiveState",
		)
	}
	res, err := e.T.Run(ctx, strings.Join(commands, "\n"))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read scheduled-job state (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	observed := map[string]map[string]scheduleUnitObservation{}
	name, kind := "", ""
	values := map[string]string{}
	flush := func() {
		if name == "" || kind == "" {
			return
		}
		exit, _ := strconv.Atoi(values["ExecMainStatus"])
		if observed[name] == nil {
			observed[name] = map[string]scheduleUnitObservation{}
		}
		observed[name][kind] = scheduleUnitObservation{
			loadState: values["LoadState"], activeState: values["ActiveState"],
			result: values["Result"], exitStatus: exit,
		}
		values = map[string]string{}
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "@@") {
			flush()
			marker := strings.TrimPrefix(line, "@@")
			name, kind, _ = strings.Cut(marker, ":")
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	flush()

	statuses := make([]StatusSchedule, 0, len(jobs))
	for _, job := range jobs {
		unit := e.names().ScheduledJobUnit(job.Name)
		service := observed[job.Name]["service"]
		timer := observed[job.Name]["timer"]
		status := StatusSchedule{
			Name: job.Name, Unit: unit, TimerState: timer.activeState,
			LastResult: service.result, LastExitStatus: service.exitStatus,
		}
		if timer.loadState != "loaded" || timer.activeState != "active" {
			status.Issues = append(status.Issues, "timer is not active")
		}
		if service.loadState != "loaded" {
			status.Issues = append(status.Issues, "service unit is not loaded")
		}
		if service.result != "" && service.result != "success" {
			status.Issues = append(status.Issues, fmt.Sprintf("last run failed: %s (exit %d)", service.result, service.exitStatus))
		}
		status.Diverged = len(status.Issues) > 0
		statuses = append(statuses, status)
	}
	return statuses, nil
}
