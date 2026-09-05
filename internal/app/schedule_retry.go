package app

import (
	"math"
	"time"
)

// The retry defaults reproduce today's behaviour exactly: one attempt, and the
// backoff values are inert until attempts rises above one.
const (
	defaultRetryAttempts   = 1
	defaultRetryBackoff    = 30 * time.Second
	defaultRetryMaxBackoff = 10 * time.Minute
	maxRetryAttempts       = 10
)

// defaultScheduleNotify is what the failure notifier always did: speak on
// failure and timeout, stay quiet on success, and never mention a skip.
var defaultScheduleNotify = []string{"failure", "timeout"}

// RetryBackoffSeconds is the whole-second form the runner sleeps: `sleep`
// takes seconds, and a fraction rounds up rather than down to a busy loop.
func RetryBackoffSeconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}

// scheduleRetryWorstCase is the longest a run can spend asleep between
// attempts. It reproduces the runner's own arithmetic step for step: whole
// seconds, sleep, double, cap, over the attempts-1 sleeps. Validation keeps it
// under the timeout so the last attempt can always start, and that promise
// only holds if both sides count the same way.
func scheduleRetryWorstCase(attempts int, backoff, max time.Duration) time.Duration {
	total := 0
	sleep, cap := RetryBackoffSeconds(backoff), RetryBackoffSeconds(max)
	for i := 1; i < attempts; i++ {
		total += sleep
		sleep *= 2
		if sleep > cap {
			sleep = cap
		}
	}
	return time.Duration(total) * time.Second
}

// retryPolicy resolves the declared block over the defaults. Unparseable
// durations fall back to the default here; validation has already refused them
// on the load path, so this only softens a struct built by hand.
func (s *JobSchedule) retryPolicy() (attempts int, backoff, max time.Duration) {
	attempts, backoff, max = defaultRetryAttempts, defaultRetryBackoff, defaultRetryMaxBackoff
	if s == nil || s.Retry == nil {
		return attempts, backoff, max
	}
	if s.Retry.Attempts != nil {
		attempts = *s.Retry.Attempts
	}
	if s.Retry.Backoff != "" {
		if d, ok := ParseDuration(s.Retry.Backoff); ok {
			backoff = d
		}
	}
	if s.Retry.MaxBackoff != "" {
		if d, ok := ParseDuration(s.Retry.MaxBackoff); ok {
			max = d
		}
	}
	return attempts, backoff, max
}

// notifyOutcomes resolves the declared list over the default.
func (s *JobSchedule) notifyOutcomes() []string {
	if s == nil || len(s.Notify) == 0 {
		return append([]string(nil), defaultScheduleNotify...)
	}
	return append([]string(nil), s.Notify...)
}

// scheduleTimeout is the run's wall-time bound as a duration, with the
// schema default when the author left it out.
func (s *JobSchedule) scheduleTimeout() time.Duration {
	if s != nil && s.Timeout != "" {
		if d, ok := ParseDuration(s.Timeout); ok {
			return d
		}
	}
	return time.Hour
}

func validateJobRetry(s *JobSchedule, path string) error {
	if s.Retry != nil {
		if s.Retry.Attempts != nil && (*s.Retry.Attempts < 1 || *s.Retry.Attempts > maxRetryAttempts) {
			return errf("project_invalid", path+".retry.attempts", "",
				"attempts must be between 1 and %d, got %d", maxRetryAttempts, *s.Retry.Attempts)
		}
		if err := gDur.checkOptional(path+".retry.backoff", s.Retry.Backoff); err != nil {
			return err
		}
		if err := gDur.checkOptional(path+".retry.max_backoff", s.Retry.MaxBackoff); err != nil {
			return err
		}
		attempts, backoff, max := s.retryPolicy()
		if backoff > max {
			return errf("project_invalid", path+".retry.backoff", "",
				"backoff %s exceeds max_backoff %s", backoff, max)
		}
		if worst, timeout := scheduleRetryWorstCase(attempts, backoff, max), s.scheduleTimeout(); worst >= timeout {
			return errf("project_invalid", path+".retry", "",
				"worst-case backoff %s is not smaller than timeout %s; later attempts could never start", worst, timeout)
		}
	}
	for i, outcome := range s.Notify {
		if err := checkEnum(indexed(path+".notify", i), outcome, eScheduleNotify); err != nil {
			return err
		}
	}
	return nil
}
