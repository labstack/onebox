package app

import (
	"strings"
	"time"
)

// validateJobExecution keeps persisted definitions small and limits recovery to
// jobs whose data-effect policy already permits operator-initiated execution.
func validateJobExecution(w Workload, path string) error {
	if w.Execution == nil {
		return nil
	}
	p := path + ".execution"
	if !w.IsJob() || w.Schedule == nil || (w.When != "" && w.When != "manual") || w.DataEffect != DataEffectNone || w.Compose != "" {
		return errf("project_invalid", p, "", "durable execution requires a native scheduled manual job with data_effect none")
	}
	if w.Execution.Retention != "" {
		d, ok := ParseDuration(w.Execution.Retention)
		if !ok || d <= 0 || d > 30*24*time.Hour {
			return errf("project_invalid", p+".retention", "", "retention must be a positive duration at most 30d")
		}
	}
	if len(w.Inputs) > 32 {
		return errf("project_invalid", path+".inputs", "", "durable execution supports at most 32 original inputs")
	}
	if len(w.Execution.Steps) > 32 {
		return errf("project_invalid", p+".steps", "", "durable execution supports at most 32 steps")
	}
	previous := map[string]map[string]bool{}
	var backoffTotal time.Duration
	for i, step := range w.Execution.Steps {
		sp := indexed(p+".steps", i)
		if err := gIdent.check(sp+".id", step.ID); err != nil {
			return err
		}
		if previous[step.ID] != nil {
			return errf("project_invalid", sp+".id", "", "step IDs must be unique")
		}
		if len(step.Command) == 0 || len(step.Command) > 128 || step.Command[0] == "" {
			return errf("project_invalid", sp+".command", "", "command requires a nonempty first argument and at most 128 arguments")
		}
		total := 0
		for _, arg := range step.Command {
			total += len(arg)
			if strings.ContainsRune(arg, 0) || len(arg) > 4096 || total > 16384 {
				return errf("project_invalid", sp+".command", "", "command arguments must contain no NUL and fit within 4096 bytes each and 16384 bytes total")
			}
		}
		if len(step.Inputs) > 32 || len(step.Outputs) > 32 {
			return errf("project_invalid", sp, "", "steps support at most 32 inputs and 32 outputs")
		}
		for _, name := range sortedKeys(step.Inputs) {
			ip := sp + ".inputs." + name
			if err := validateExecutionValueName(ip, name); err != nil {
				return err
			}
			_, envClash := w.Env[name]
			_, inputClash := w.Inputs[name]
			if envClash || inputClash {
				return errf("project_invalid", ip, "", "step input collides with a job env key or original input")
			}
			from, output, ok := strings.Cut(step.Inputs[name], ".")
			if !ok || !previous[from][output] {
				return errf("project_invalid", ip, "", "reference must be precedingStep.OUTPUT naming a declared output of an earlier step")
			}
		}
		outputs := map[string]bool{}
		for j, name := range step.Outputs {
			op := indexed(sp+".outputs", j)
			if err := validateExecutionValueName(op, name); err != nil {
				return err
			}
			if outputs[name] {
				return errf("project_invalid", op, "", "output names must be unique within a step")
			}
			outputs[name] = true
		}
		previous[step.ID] = outputs
		policy := &JobSchedule{Timeout: w.Schedule.Timeout, Retry: step.Retry}
		if policy.Retry == nil {
			policy.Retry = w.Schedule.Retry
		}
		if err := validateJobRetry(policy, sp); err != nil {
			return err
		}
		attempts, backoff, max := policy.retryPolicy()
		backoffTotal += scheduleRetryWorstCase(attempts, backoff, max)
	}
	if backoffTotal >= w.Schedule.scheduleTimeout() {
		return errf("project_invalid", p+".steps", "", "combined worst-case step backoff %s must be smaller than schedule timeout %s", backoffTotal, w.Schedule.scheduleTimeout())
	}
	return nil
}

func validateExecutionValueName(path, name string) error {
	if err := gInputName.check(path, name); err != nil {
		return err
	}
	if len(name) > 128 || strings.HasPrefix(name, ReservedInputPrefix) {
		return errf("project_invalid", path, "", "names must be at most 128 bytes and outside the %s namespace", ReservedInputPrefix)
	}
	return nil
}
