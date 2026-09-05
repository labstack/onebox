package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A schedule that silently never fires looks exactly like one that works,
// until someone needs what it was supposed to produce. Each of these is a form
// whose meaning must be identical on both sides.
func TestCronTranslatesExactly(t *testing.T) {
	for _, tt := range []struct{ cron, want string }{
		{"0 2 * * *", "*-*-* 02:00:00"},
		{"30 4 1 * *", "*-*-01 04:30:00"},
		{"0 0 1 1 *", "*-01-01 00:00:00"},
		{"*/15 * * * *", "*-*-* *:00/15:00"},
		{"0 */6 * * *", "*-*-* 00/6:00:00"},
		{"0 4 * * 0", "Sun *-*-* 04:00:00"},
		{"0 4 * * 7", "Sun *-*-* 04:00:00"},
		{"0 9 * * 1-5", "Mon..Fri *-*-* 09:00:00"},
		{"0 9 * * 1,3,5", "Mon,Wed,Fri *-*-* 09:00:00"},
		{"15,45 * * * *", "*-*-* *:15,45:00"},
		{"0 8-18/2 * * *", "*-*-* 08..18/2:00:00"},
	} {
		got, err := CronToCalendar(tt.cron)
		if err != nil {
			t.Errorf("%q: %v", tt.cron, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%q → %q, want %q", tt.cron, got, tt.want)
		}
	}
}

// Cron runs a job when EITHER the day-of-month or the day-of-week matches. A
// calendar expression cannot say that, and approximating it would move the job
// to days nobody chose.
func TestBothDayFieldsIsRefused(t *testing.T) {
	if _, err := CronToCalendar("0 2 1 * 0"); err == nil {
		t.Fatal("a day-of-month and a day-of-week together must be refused")
	}
}

func TestMalformedCronIsRefused(t *testing.T) {
	for _, bad := range []string{
		"0 2 * *",     // four fields
		"0 2 * * * *", // six
		"60 2 * * *",  // minute out of range
		"0 24 * * *",  // hour out of range
		"0 2 * * 8",   // weekday out of range
		"*/0 * * * *", // zero step
		"5-1 * * * *", // backwards range
		"@daily",      // a form this contract does not accept
		"0 2 * * MON", // names, which cron dialects differ on
	} {
		if _, err := CronToCalendar(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// A job's schedule reaches the host with its timezone; a backup at 2am means
// 2am where the operator lives, not wherever the box was imaged.
func TestScheduledJobsCarryTimezone(t *testing.T) {
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web:   {role: application, image: x:1}
  prune: {role: job, image: x:1, data_effect: none, schedule: {cron: "0 3 * * *", timezone: "Europe/Berlin"}}
  once:  {role: job, image: x:1, data_effect: none}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := spec.ScheduledJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "prune" {
		t.Fatalf("only scheduled jobs belong here: %#v", jobs)
	}
	if jobs[0].Timezone != "Europe/Berlin" || jobs[0].Calendar != "*-*-* 03:00:00" ||
		jobs[0].Timeout != "1h" || !jobs[0].CatchUp || jobs[0].DeployLock != "exclusive" {
		t.Fatalf("schedule lost its meaning: %#v", jobs[0])
	}
}

func TestPinnedScheduleEligibilityFailsClosed(t *testing.T) {
	valid := `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  refresh: {role: job, image: x:1, data_effect: none, schedule: {cron: "0 3 * * *", deploy_lock: pinned}}
`
	spec, err := LoadBytes([]byte(valid), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := spec.ScheduledJobs()
	if err != nil || len(jobs) != 1 || jobs[0].DeployLock != "pinned" {
		t.Fatalf("pinned policy was not preserved: jobs=%#v err=%v", jobs, err)
	}

	for name, project := range map[string]string{
		"migration":       strings.Replace(valid, "data_effect: none", "data_effect: migration", 1),
		"adopted compose": strings.Replace(valid, "image: x:1", "compose: docker-compose.yml#refresh", 1),
		"unknown policy":  strings.Replace(valid, "deploy_lock: pinned", "deploy_lock: shared", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadBytes([]byte(project), "ob.yml"); err == nil {
				t.Fatal("ineligible pinned schedule was accepted")
			}
		})
	}
}

func TestScheduledJobRunPolicyIsExplicitAndValidated(t *testing.T) {
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  prune:
    role: job
    image: x:1
    data_effect: none
    schedule: {cron: "0 3 * * *", timeout: 20m, catch_up: false}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := spec.ScheduledJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Timeout != "20m" || jobs[0].CatchUp {
		t.Fatalf("authored run policy was not preserved: %#v", jobs)
	}

	bad := `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  prune: {role: job, image: x:1, data_effect: none, schedule: {cron: "0 3 * * *", timeout: forever}}
`
	if _, err := LoadBytes([]byte(bad), "ob.yml"); err == nil {
		t.Fatal("an invalid scheduled-job timeout was accepted")
	}
}

func TestScheduledJobRetryAndNotifyResolveWithDefaults(t *testing.T) {
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  plain:
    role: job
    image: x:1
    data_effect: none
    schedule: {cron: "0 3 * * *"}
  retrying:
    role: job
    image: x:1
    data_effect: none
    schedule:
      cron: "0 * * * *"
      timeout: 45m
      retry: {attempts: 3, backoff: 30s, max_backoff: 10m}
      notify: [failure, timeout, skipped]
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := spec.ScheduledJobs()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ScheduledJob{}
	for _, job := range jobs {
		byName[job.Name] = job
	}
	plain := byName["plain"]
	if plain.RetryAttempts != 1 || plain.RetryBackoff != 30*time.Second || plain.RetryMaxBackoff != 10*time.Minute ||
		strings.Join(plain.Notify, ",") != "failure,timeout" {
		t.Fatalf("defaults did not resolve: %#v", plain)
	}
	retrying := byName["retrying"]
	if retrying.RetryAttempts != 3 || strings.Join(retrying.Notify, ",") != "failure,timeout,skipped" {
		t.Fatalf("declared retry did not resolve: %#v", retrying)
	}
}

func TestScheduledJobRetryIsBoundedByTheTimeout(t *testing.T) {
	for name, tc := range map[string]struct {
		schedule string
		code     string
	}{
		"too many attempts":       {`{cron: "0 * * * *", retry: {attempts: 11}}`, "project_invalid"},
		"zero attempts":           {`{cron: "0 * * * *", retry: {attempts: 0}}`, "project_invalid"},
		"backoff over max":        {`{cron: "0 * * * *", retry: {attempts: 2, backoff: 20m, max_backoff: 10m}}`, "project_invalid"},
		"backoff exceeds timeout": {`{cron: "0 * * * *", timeout: 5m, retry: {attempts: 3, backoff: 2m, max_backoff: 30m}}`, "project_invalid"},
		"unknown notify":          {`{cron: "0 * * * *", notify: [warning]}`, "project_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  j: {role: job, image: x:1, data_effect: none, schedule: `+tc.schedule+`}
`), "ob.yml")
			var e *Error
			if !errors.As(err, &e) || e.Code != tc.code {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
		})
	}
	if got := scheduleRetryWorstCase(3, 2*time.Minute, 30*time.Minute); got != 6*time.Minute {
		t.Fatalf("worst case = %s, want 6m", got)
	}
	if got := scheduleRetryWorstCase(4, 30*time.Second, time.Minute); got != 150*time.Second {
		t.Fatalf("capped worst case = %s, want 2m30s", got)
	}
}
