# Scheduled Job Runs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give scheduled jobs a per-run record in the host journal, bounded retry with backoff, per-outcome notifications, and declared inputs with an operator-initiated `ob schedule run`, without any resident process or new store on the host.

**Architecture:** The generated runner script gains a state file, a retry loop and manual-input handling; the generated `ExecStopPost` notifier finalises one JSON record per run into the journal via `systemd-cat -t ob-run` and sends notifications per outcome. `ob` reads records back with `journalctl` (`schedule history`, `schedule logs`, `schedule list`, richer `status`) and starts a unit with validated inputs (`schedule run`), journaled as `schedule_run`. Every new project-file field is optional and every default preserves today's behaviour.

**Tech Stack:** Go 1.27, cobra CLI, POSIX sh rendered from Go, systemd (timers, `INVOCATION_ID`, `TRIGGER_UNIT`, `SuccessExitStatus`, `systemd-cat`, `journalctl`), `transport.Fake` for unit tests, Lima Ubuntu 24.04 for `just server-e2e`.

**Spec:** https://github.com/labstack/onebox/issues/155 (revised body, 2026-09-05)

## Global Constraints

- Onebox environment variables use the `ONEBOX_` prefix. `OB_` is retired and `just env-namespace` fails on any tracked-file match of `\bOB_[A-Z0-9_]+`. Reserved input prefix and metadata line are therefore `ONEBOX_`.
- Workload names match `^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`.
- Input names match `^[A-Z][A-Z0-9_]*$`, must not start with `ONEBOX_`, must not equal a key in the workload's `env`.
- Input values (defaults and overrides): at most 256 bytes, no `"`, no `\`, no control characters (`0x00-0x1f`, `0x7f`). Patterns match the whole value.
- `retry.attempts` 1..10 default 1; `retry.backoff` default `30s`; `retry.max_backoff` default `10m`; worst-case total backoff must be strictly less than `schedule.timeout`.
- `schedule.notify` values: `success`, `failure`, `timeout`, `skipped`; default `[failure, timeout]`.
- `inputs` requires `schedule` and `data_effect: none`. `ob schedule run` refuses any other data effect.
- Service unit gains `SuccessExitStatus=75`.
- Run record syslog identifier: `ob-run`. Record fields: `run, job, trigger, operation, release, started_at, finished_at, duration_s, attempts, exit_status, outcome, inputs`.
- Manual trigger detection: `$TRIGGER_UNIT` unset. Host needs systemd 252+ only when a job declares `inputs`; checked in `SyncSchedules` beside the existing `systemd-analyze calendar` check.
- Commit messages end with `Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5`.
- Gates before every commit: `go build ./... && go vet ./... && go test ./...`. Before the final commit of each slice also `just docs-generate`, `go run ./cmd/ob schema --out docs/onebox.run-v1.schema.json`, `just docs-generate-check`, `just env-namespace`.

---

## Slice 1: History

### Task 1: State file in both runner modes, `SuccessExitStatus=75`

**Files:**
- Modify: `internal/engine/schedule.go:172-245` (`scheduleRunnerScript`, `pinnedScheduleRunnerScript`), `internal/engine/schedule.go:262-280` (`scheduleServiceUnit`)
- Test: `internal/engine/schedule_test.go`

**Interfaces:**
- Produces: shell function `write_state <attempt>` defined in both runners, writing `release=`, `started_at=`, `started_epoch=`, `trigger=`, `operation=`, `attempt=`, `inputs=` lines atomically to `names.ScheduledJobRunState(job)`. Runner no longer deletes the state file. Go helper `scheduleStateFunction(state string) []string`.

- [ ] **Step 1: Write the failing test**

Append to `internal/engine/schedule_test.go`:

```go
func TestScheduledJobRunnersRecordRunStateForTheNotifier(t *testing.T) {
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	for _, tc := range []struct {
		name string
		job  app.ScheduledJob
	}{
		{"exclusive", app.ScheduledJob{Name: "nightly", Cron: "0 2 * * *", Timezone: "UTC", Calendar: "*-*-* 02:00:00", Timeout: "45m", DeployLock: "exclusive"}},
		{"pinned", app.ScheduledJob{Name: "nightly", Cron: "0 2 * * *", Timezone: "UTC", Calendar: "*-*-* 02:00:00", Timeout: "45m", DeployLock: "pinned"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := scheduleRunnerScript("sample", tc.job, names, "/var/lib/ob/sample/lock", nil)
			for _, want := range []string{
				"state='/var/lib/ob/sample/schedule/nightly.state'",
				"write_state() {",
				"started_epoch=%s",
				`trigger=%s`,
				"write_state 1",
				"mv -f \"$tmp\" \"$state\"",
			} {
				if !strings.Contains(runner, want) {
					t.Errorf("%s runner is missing %q:\n%s", tc.name, want, runner)
				}
			}
			if strings.Contains(runner, `rm -f "$state"`) {
				t.Errorf("%s runner removes the state the notifier finalises:\n%s", tc.name, runner)
			}
			command := exec.CommandContext(context.Background(), "sh", "-n")
			command.Stdin = strings.NewReader(runner)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("%s runner is not valid POSIX shell: %v: %s\n%s", tc.name, err, output, runner)
			}
		})
	}
	service := scheduleServiceUnit("sample", app.ScheduledJob{Name: "nightly", Timeout: "45m"},
		"/etc/systemd/system/ob-sample-nightly.run", "/etc/systemd/system/ob-sample-nightly.notify")
	if !strings.Contains(service, "SuccessExitStatus=75") {
		t.Errorf("a lock-conflict skip must not be a failed unit:\n%s", service)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine -run TestScheduledJobRunnersRecordRunStateForTheNotifier`
Expected: FAIL, runner is missing `write_state() {`.

- [ ] **Step 3: Implement**

In `internal/engine/schedule.go` add after `scheduleContainerCleanup`:

```go
// scheduleStateFunction renders the shell function both runners use to record
// the run in progress. The notifier reads it after the run ends, so the runner
// never removes it: a runner that cleaned up its own state would erase the
// only evidence a timed-out run leaves behind.
func scheduleStateFunction() []string {
	return []string{
		"write_state() {",
		"  umask 077",
		"  printf 'release=%s\\nstarted_at=%s\\nstarted_epoch=%s\\ntrigger=%s\\noperation=%s\\nattempt=%s\\ninputs=%s\\n' " +
			"\"$release\" \"$started_at\" \"$started_epoch\" \"$trigger\" \"$operation\" \"$1\" \"$inputs_json\" >\"$tmp\"",
		"  mv -f \"$tmp\" \"$state\"",
		"}",
	}
}

// scheduleRunPreamble sets the variables write_state records. The trigger is
// systemd's own word for it: a timer activation carries TRIGGER_UNIT (systemd
// 252+), anything else is an operator.
func scheduleRunPreamble(state string) []string {
	return append([]string{
		"state=" + q(state),
		"tmp=\"$state.$$\"",
		"started_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')",
		"started_epoch=$(date -u '+%s')",
		"if [ -n \"${TRIGGER_UNIT:-}\" ]; then trigger=timer; else trigger=manual; fi",
		"operation=''",
		"inputs_json=''",
	}, scheduleStateFunction()...)
}
```

Rewrite the exclusive runner body (replace lines from `scheduleContainerCleanup(container),` through `compose,` in `scheduleRunnerScript`):

```go
	lines := []string{
		"#!/bin/sh",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"set -eu",
		"install -d -m 700 " + q(names.AppDir()+"/schedule"),
		"exec 9>" + q(names.ScheduledJobRunLock(job.Name)),
		"/usr/bin/flock --exclusive --nonblock --conflict-exit-code 75 9",
		"exec 8>" + q(names.ScheduleRunLock()),
		"/usr/bin/flock --exclusive --nonblock --conflict-exit-code 75 8",
		"if [ -e " + q(applicationLock) + " ]; then echo 'onebox: an application operation holds the deploy lock' >&2; exit 75; fi",
		"release_dir=$(readlink -f " + q(names.CurrentLink()) + " 2>/dev/null || true)",
		"release=${release_dir##*/}",
		scheduleContainerCleanup(container),
		"cleanup() { " + scheduleContainerCleanup(container) + "; rm -f \"$tmp\"; }",
		"trap cleanup 0",
		"trap 'exit 129' 1",
		"trap 'exit 130' 2",
		"trap 'exit 143' 15",
	}
	lines = append(lines, scheduleRunPreamble(names.ScheduledJobRunState(job.Name))...)
	lines = append(lines, "write_state 1", compose, "")
	return strings.Join(lines, "\n")
```

In `pinnedScheduleRunnerScript` replace the block from `"state=" + q(state),` through `"mv -f \"$tmp\" \"$state\"",` with:

```go
		"cleanup() { " + scheduleContainerCleanup(container) + "; rm -f \"$tmp\"; }",
		"trap cleanup 0",
		"trap 'exit 129' 1",
		"trap 'exit 130' 2",
		"trap 'exit 143' 15",
	}
	lines = append(lines, scheduleRunPreamble(state)...)
	lines = append(lines, "write_state 1", "/usr/bin/flock --unlock 8", compose, "")
	return strings.Join(lines, "\n")
```

Remove the now-unused `"started_at=$(date ...)"`, `"umask 077"`, `"printf 'release=...'"` lines and the old `"state="`/`"tmp="` lines from the pinned runner. Keep `"release=${release_dir##*/}"` in the pinned runner (it already exists).

In `scheduleServiceUnit` add after `"TimeoutStartSec=" + job.Timeout,`:

```go
		// Exit 75 is the runner's "skipped for a lock conflict". It is a fact
		// about timing, not a failure of the job, and it must not leave the
		// unit failed or trip failure notifications.
		"SuccessExitStatus=75",
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine`
Expected: PASS. `TestScheduleStatusReportsRunningPinnedRelease` still passes because the status reader only needs `release=` and `started_at=` lines.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/schedule.go internal/engine/schedule_test.go
git commit -m "feat(schedule): record run state in both runner modes and stop treating a skip as failure

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

### Task 2: Notifier finalises one journal record per run

**Files:**
- Modify: `internal/engine/schedule.go:287-335` (`scheduleFailureNotifier`)
- Test: `internal/engine/schedule_test.go`

**Interfaces:**
- Produces: the notifier script reads the state file, derives `outcome`, writes one JSON line via `systemd-cat -t ob-run`, deletes the state file, then keeps today's failure/timeout notification behaviour. Constant `scheduleRunIdentifier = "ob-run"`.

- [ ] **Step 1: Write the failing test**

```go
func TestScheduledJobNotifierWritesOneRunRecordToTheJournal(t *testing.T) {
	cfg := testConfig()
	f := &transport.Fake{TargetName: "root@example.internal"}
	e := New(cfg, testProject(t), f, Options{Environment: "production", Out: &bytes.Buffer{}, Sleep: noSleep})
	script, err := e.scheduleFailureNotifier("nightly")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"state='/var/lib/ob/sample/schedule/nightly.state'",
		`started_epoch=*) started_epoch=${line#started_epoch=}`,
		`rm -f "$state"`,
		`result=${SERVICE_RESULT:-success}`,
		`status=${EXIT_STATUS:-0}`,
		`outcome=timeout`,
		`outcome=skipped`,
		`outcome=success`,
		`outcome=failure`,
		`"run":"%s","job":"%s","trigger":"%s","operation":"%s","release":"%s"`,
		`"duration_s":%s,"attempts":%s,"exit_status":%s,"outcome":"%s","inputs":{%s}`,
		`"${INVOCATION_ID:-}" 'nightly'`,
		"systemd-cat -t ob-run",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("notifier is missing %q:\n%s", want, script)
		}
	}
	command := exec.CommandContext(context.Background(), "sh", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("notifier is not valid POSIX shell: %v: %s\n%s", err, output, script)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine -run TestScheduledJobNotifierWritesOneRunRecordToTheJournal`
Expected: FAIL, missing `state='...'`.

- [ ] **Step 3: Implement**

Add the constant and a record-rendering helper near `scheduleNotificationTimestamp`:

```go
// scheduleRunIdentifier is the syslog identifier of the one line the notifier
// writes per run. `journalctl -u <unit> -t ob-run` is the run history.
const scheduleRunIdentifier = "ob-run"

// scheduleRunRecordLines finalises the run the runner started. It runs in
// ExecStopPost because only systemd knows how the run ended: a timed-out
// runner is killed mid-sleep and cannot write its own outcome.
func scheduleRunRecordLines(job, state string) []string {
	return []string{
		"state=" + q(state),
		"release=''; started_at=''; started_epoch=''; trigger=''; operation=''; attempt=0; inputs=''",
		"if [ -f \"$state\" ]; then",
		"  while IFS= read -r line || [ -n \"$line\" ]; do",
		"    case \"$line\" in",
		"      release=*) release=${line#release=} ;;",
		"      started_at=*) started_at=${line#started_at=} ;;",
		"      started_epoch=*) started_epoch=${line#started_epoch=} ;;",
		"      trigger=*) trigger=${line#trigger=} ;;",
		"      operation=*) operation=${line#operation=} ;;",
		"      attempt=*) attempt=${line#attempt=} ;;",
		"      inputs=*) inputs=${line#inputs=} ;;",
		"    esac",
		"  done <\"$state\"",
		"  rm -f \"$state\"",
		"fi",
		"if [ -z \"$trigger\" ]; then if [ -n \"${TRIGGER_UNIT:-}\" ]; then trigger=timer; else trigger=manual; fi; fi",
		"result=${SERVICE_RESULT:-success}",
		"status=${EXIT_STATUS:-0}",
		"case \"$status\" in ''|*[!0-9]*) status=null ;; esac",
		"case \"$attempt\" in ''|*[!0-9]*) attempt=0 ;; esac",
		"if [ \"$result\" = timeout ]; then outcome=timeout",
		"elif [ \"$status\" = 75 ]; then outcome=skipped",
		"elif [ \"$result\" = success ] && [ \"$status\" = 0 ]; then outcome=success",
		"else outcome=failure; fi",
		"finished_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')",
		"now=$(date -u '+%s')",
		"duration=0",
		"case \"$started_epoch\" in ''|*[!0-9]*) ;; *) duration=$((now - started_epoch)) ;; esac",
		"[ -z \"$started_at\" ] && started_at=$finished_at",
		"printf '{\"run\":\"%s\",\"job\":\"%s\",\"trigger\":\"%s\",\"operation\":\"%s\",\"release\":\"%s\",\"started_at\":\"%s\",\"finished_at\":\"%s\",\"duration_s\":%s,\"attempts\":%s,\"exit_status\":%s,\"outcome\":\"%s\",\"inputs\":{%s}}\\n' " +
			"\"${INVOCATION_ID:-}\" " + q(job) + " \"$trigger\" \"$operation\" \"$release\" \"$started_at\" \"$finished_at\" \"$duration\" \"$attempt\" \"$status\" \"$outcome\" \"$inputs\" " +
			"| systemd-cat -t " + scheduleRunIdentifier + " || true",
	}
}
```

In `scheduleFailureNotifier`, replace the line `` `[ "${SERVICE_RESULT:-success}" = success ] && exit 0`, `` with:

```go
	}
	lines = append(lines, scheduleRunRecordLines(job, e.names().ScheduledJobRunState(job))...)
	lines = append(lines, `case "$outcome" in failure|timeout) ;; *) exit 0 ;; esac`)
	lines = append(lines, "")
	if false {
```

Concretely the function becomes:

```go
	lines := []string{
		"#!/bin/sh",
		"# Written by Onebox. Edits are overwritten on the next deploy.",
		"set -u",
		"exec 9>" + q(e.names().ScheduledJobRunLock(job)),
		"if /usr/bin/flock --exclusive --nonblock 9; then",
		"  " + scheduleContainerCleanup(e.names().Container(job, 1)),
		"fi",
	}
	lines = append(lines, scheduleRunRecordLines(job, e.names().ScheduledJobRunState(job))...)
	lines = append(lines, `case "$outcome" in failure|timeout) ;; *) exit 0 ;; esac`)
	var sends []string
	// ... unchanged send rendering ...
```

Update `TestScheduledJobFailureNotifierUsesConfiguredWebhooks`: replace the expectation `` `${SERVICE_RESULT:-success}` `` with `` `result=${SERVICE_RESULT:-success}` ``.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/schedule.go internal/engine/schedule_test.go
git commit -m "feat(schedule): finalise one journal record per scheduled run from ExecStopPost

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

### Task 3: Read run records back: `ScheduleHistory`, `ScheduleList`, run logs

**Files:**
- Create: `internal/engine/schedule_history.go`
- Test: `internal/engine/schedule_history_test.go`

**Interfaces:**
- Produces:
  - `type ScheduleRunRecord struct { Run, Job, Trigger, Operation, Release, StartedAt, FinishedAt string; DurationSeconds int; Attempts int; ExitStatus *int; Outcome string; Inputs map[string]string }` with JSON tags `run, job, trigger, operation, release, started_at, finished_at, duration_s, attempts, exit_status, outcome, inputs`.
  - `func parseScheduleRunRecords(stdout string) []ScheduleRunRecord` (skips unparseable lines).
  - `func scheduleHistoryCommand(unit string, n int) string` = `journalctl -u <unit>.service -t ob-run -o cat -r -n <n> --no-pager 2>/dev/null || true`.
  - `func (e *Engine) ScheduleHistory(ctx, job string, n int) ([]ScheduleRunRecord, error)`; unknown or unscheduled job returns an error `job %q is not a scheduled job`.
  - `type ScheduleListing struct { Name, Unit, Cron, Timezone, DeployLock, Timeout, TimerState, NextRun, LastTrigger string }` JSON tags `name, unit, cron, timezone, deploy_lock, timeout, timer_state, next_run, last_trigger`.
  - `func (e *Engine) ScheduleList(ctx) ([]ScheduleListing, error)` using `systemctl show <unit>.timer --no-pager --property=ActiveState --property=NextElapseUSecRealtime --property=LastTriggerUSec` batched with `@@<job>` markers.
  - `func (e *Engine) ScheduleLogs(ctx, job, run string, tail int, stdout, stderr io.Writer) error`: with `run` empty, resolve the newest record's run id via `ScheduleHistory(ctx, job, 1)`; command `journalctl _SYSTEMD_INVOCATION_ID=<run> --no-pager -o short-iso`; with no record fall back to `journalctl -u <unit>.service -n <tail> --no-pager -o short-iso`. Run ids are validated against `^[0-9a-f]{32}$` before reaching a shell.

- [ ] **Step 1: Write the failing tests**

`internal/engine/schedule_history_test.go`:

```go
package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

const sampleRunRecords = `{"run":"b2c3d4e5f60718293a4b5c6d7e8f9012","job":"nightly","trigger":"manual","operation":"20260905-151200-schedule_run-7c1e","release":"20260905-140000-ab12cd","started_at":"2026-09-05T15:12:01Z","finished_at":"2026-09-05T15:12:31Z","duration_s":30,"attempts":2,"exit_status":0,"outcome":"success","inputs":{"SOURCE":"prices"}}
not json
{"run":"a1b2c3d4e5f60718293a4b5c6d7e8f90","job":"nightly","trigger":"timer","operation":"","release":"20260905-140000-ab12cd","started_at":"2026-09-05T15:00:01Z","finished_at":"2026-09-05T15:00:02Z","duration_s":1,"attempts":0,"exit_status":75,"outcome":"skipped","inputs":{}}
`

func TestParseScheduleRunRecordsSkipsNoiseAndKeepsOrder(t *testing.T) {
	records := parseScheduleRunRecords(sampleRunRecords)
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Run != "b2c3d4e5f60718293a4b5c6d7e8f9012" || records[0].Trigger != "manual" || records[0].Attempts != 2 ||
		records[0].DurationSeconds != 30 || records[0].Inputs["SOURCE"] != "prices" || records[0].ExitStatus == nil || *records[0].ExitStatus != 0 {
		t.Fatalf("first record was not decoded: %#v", records[0])
	}
	if records[1].Outcome != "skipped" || *records[1].ExitStatus != 75 {
		t.Fatalf("second record was not decoded: %#v", records[1])
	}
}

func scheduledFixture(t *testing.T) (*Engine, *transport.Fake) {
	t.Helper()
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := &transport.Fake{}
	return New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep}), f
}

func TestScheduleHistoryReadsTheUnitJournalNewestFirst(t *testing.T) {
	e, f := scheduledFixture(t)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "journalctl") {
			return transport.Result{Stdout: sampleRunRecords}, true
		}
		return transport.Result{}, false
	}
	records, err := e.ScheduleHistory(context.Background(), "nightly", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Outcome != "success" {
		t.Fatalf("records = %#v", records)
	}
	seq := strings.Join(f.Commands, "\n")
	for _, want := range []string{"journalctl -u ob-sample-nightly.service", "-t ob-run", "-o cat", "-r", "-n 20", "--no-pager"} {
		if !strings.Contains(seq, want) {
			t.Fatalf("history read is missing %q:\n%s", want, seq)
		}
	}
	if _, err := e.ScheduleHistory(context.Background(), "web", 20); err == nil {
		t.Fatal("history of a non-scheduled workload was not refused")
	}
}

func TestScheduleListReadsTimerState(t *testing.T) {
	e, f := scheduledFixture(t)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: "@@nightly\nActiveState=active\nNextElapseUSecRealtime=Sat 2026-09-06 02:00:00 UTC\nLastTriggerUSec=Fri 2026-09-05 02:00:00 UTC\n"}, true
		}
		return transport.Result{}, false
	}
	listing, err := e.ScheduleList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing) != 1 || listing[0].Unit != "ob-sample-nightly" || listing[0].TimerState != "active" ||
		listing[0].NextRun != "Sat 2026-09-06 02:00:00 UTC" || listing[0].LastTrigger != "Fri 2026-09-05 02:00:00 UTC" || listing[0].Cron != "0 2 * * *" {
		t.Fatalf("listing = %#v", listing)
	}
}

func TestScheduleLogsTargetsOneInvocation(t *testing.T) {
	e, f := scheduledFixture(t)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "-t ob-run") {
			return transport.Result{Stdout: sampleRunRecords}, true
		}
		return transport.Result{}, false
	}
	var out bytes.Buffer
	if err := e.ScheduleLogs(context.Background(), "nightly", "", 200, &out, &out); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "journalctl _SYSTEMD_INVOCATION_ID=b2c3d4e5f60718293a4b5c6d7e8f9012") {
		t.Fatalf("logs did not target the newest run:\n%s", seq)
	}
	if err := e.ScheduleLogs(context.Background(), "nightly", "../etc", 200, &out, &out); err == nil {
		t.Fatal("an invalid run id reached the shell")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine -run 'TestParseScheduleRunRecords|TestScheduleHistory|TestScheduleList|TestScheduleLogs'`
Expected: FAIL to compile (undefined symbols).

- [ ] **Step 3: Implement `internal/engine/schedule_history.go`**

```go
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

// ScheduleRunRecord is the line the notifier writes to the journal when a
// scheduled run ends. The journal is the store: there is no file to trim and
// nothing that can disagree with the unit's own log.
type ScheduleRunRecord struct {
	Run             string            `json:"run"`
	Job             string            `json:"job"`
	Trigger         string            `json:"trigger"`
	Operation       string            `json:"operation,omitempty"`
	Release         string            `json:"release,omitempty"`
	StartedAt       string            `json:"started_at"`
	FinishedAt      string            `json:"finished_at"`
	DurationSeconds int               `json:"duration_s"`
	Attempts        int               `json:"attempts"`
	ExitStatus      *int              `json:"exit_status"`
	Outcome         string            `json:"outcome"`
	Inputs          map[string]string `json:"inputs,omitempty"`
}

// ScheduleListing is one declared job beside its timer as the host reports it.
type ScheduleListing struct {
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Cron        string `json:"cron"`
	Timezone    string `json:"timezone"`
	DeployLock  string `json:"deploy_lock"`
	Timeout     string `json:"timeout"`
	TimerState  string `json:"timer_state"`
	NextRun     string `json:"next_run,omitempty"`
	LastTrigger string `json:"last_trigger,omitempty"`
}

var scheduleRunID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func scheduleHistoryCommand(unit string, n int) string {
	if n <= 0 {
		n = 20
	}
	return "journalctl -u " + q(unit+".service") + " -t " + scheduleRunIdentifier +
		" -o cat -r -n " + strconv.Itoa(n) + " --no-pager 2>/dev/null || true"
}

func parseScheduleRunRecords(stdout string) []ScheduleRunRecord {
	var out []ScheduleRunRecord
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var record ScheduleRunRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		out = append(out, record)
	}
	return out
}

func (e *Engine) scheduledJob(name string) (app.ScheduledJob, error) {
	jobs, err := e.Spec.ScheduledJobs()
	if err != nil {
		return app.ScheduledJob{}, err
	}
	for _, job := range jobs {
		if job.Name == name {
			return job, nil
		}
	}
	return app.ScheduledJob{}, fmt.Errorf("job %q is not a scheduled job", name)
}

// ScheduleHistory returns the newest n run records of one job, newest first.
func (e *Engine) ScheduleHistory(ctx context.Context, name string, n int) ([]ScheduleRunRecord, error) {
	job, err := e.scheduledJob(name)
	if err != nil {
		return nil, err
	}
	res, err := e.T.Run(ctx, scheduleHistoryCommand(e.names().ScheduledJobUnit(job.Name), n))
	if err != nil {
		return nil, err
	}
	records := parseScheduleRunRecords(res.Stdout)
	if records == nil {
		records = []ScheduleRunRecord{}
	}
	return records, nil
}

// ScheduleList reads every declared job's timer in one round trip.
func (e *Engine) ScheduleList(ctx context.Context) ([]ScheduleListing, error) {
	jobs, err := e.Spec.ScheduledJobs()
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return []ScheduleListing{}, nil
	}
	var commands []string
	for _, job := range jobs {
		unit := e.names().ScheduledJobUnit(job.Name)
		commands = append(commands,
			"printf '%s\\n' "+q("@@"+job.Name),
			"systemctl show "+q(unit+".timer")+" --no-pager --property=ActiveState --property=NextElapseUSecRealtime --property=LastTriggerUSec 2>/dev/null || true")
	}
	res, err := e.T.Run(ctx, strings.Join(commands, "\n"))
	if err != nil {
		return nil, err
	}
	observed := map[string]map[string]string{}
	current := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "@@") {
			current = strings.TrimPrefix(line, "@@")
			observed[current] = map[string]string{}
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok && current != "" {
			observed[current][key] = value
		}
	}
	out := make([]ScheduleListing, 0, len(jobs))
	for _, job := range jobs {
		values := observed[job.Name]
		out = append(out, ScheduleListing{
			Name: job.Name, Unit: e.names().ScheduledJobUnit(job.Name), Cron: job.Cron, Timezone: job.Timezone,
			DeployLock: job.DeployLock, Timeout: job.Timeout, TimerState: values["ActiveState"],
			NextRun: values["NextElapseUSecRealtime"], LastTrigger: values["LastTriggerUSec"],
		})
	}
	return out, nil
}

// ScheduleLogs streams the journal of one run. The run id is systemd's
// invocation id, so the output is exactly that activation and nothing else.
func (e *Engine) ScheduleLogs(ctx context.Context, name, run string, tail int, stdout, stderr io.Writer) error {
	job, err := e.scheduledJob(name)
	if err != nil {
		return err
	}
	unit := e.names().ScheduledJobUnit(job.Name)
	if run == "" {
		records, err := e.ScheduleHistory(ctx, name, 1)
		if err != nil {
			return err
		}
		if len(records) > 0 {
			run = records[0].Run
		}
	}
	if tail <= 0 {
		tail = 200
	}
	var cmd string
	switch {
	case run == "":
		cmd = "journalctl -u " + q(unit+".service") + " -n " + strconv.Itoa(tail) + " --no-pager -o short-iso"
	case scheduleRunID.MatchString(run):
		cmd = "journalctl _SYSTEMD_INVOCATION_ID=" + run + " --no-pager -o short-iso"
	default:
		return fmt.Errorf("run id %q is not a systemd invocation id", run)
	}
	return e.T.RunStream(ctx, cmd, stdout, stderr)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine`
Expected: PASS. If `transport.Fake` lacks `RunStream`, check `internal/transport/fake.go`; it implements the interface, so this compiles.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/schedule_history.go internal/engine/schedule_history_test.go
git commit -m "feat(schedule): read run records, timer state and per-run logs from the host journal

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

### Task 4: `ob status` reads the record

**Files:**
- Modify: `internal/engine/schedule_status.go` (struct, batched command, parser), `internal/engine/status.go:168-187` (human lines)
- Test: `internal/engine/schedule_test.go`

**Interfaces:**
- Produces new `StatusSchedule` fields: `NextRun string json:"next_run,omitempty"`, `LastOutcome string json:"last_outcome,omitempty"`, `LastDurationSeconds int json:"last_duration_s,omitempty"`, `LastAttempts int json:"last_attempts,omitempty"`, `ConsecutiveFailures int json:"consecutive_failures,omitempty"`, `Attempt int json:"attempt,omitempty"`, `JournalPersistent bool json:"journal_persistent"`.
- A record with outcome `failure` or `timeout` produces the issue `last run failed: <outcome> (exit N)`; `skipped` and `success` produce none. With no record, today's systemd-based issue stays.

- [ ] **Step 1: Write the failing test**

```go
func TestScheduleStatusPrefersTheRunRecordOverSystemdResult(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@journal
persistent
@@nightly:service
LoadState=loaded
ActiveState=inactive
Result=success
ExecMainStatus=75
@@nightly:timer
LoadState=loaded
ActiveState=active
NextElapseUSecRealtime=Sat 2026-09-06 02:00:00 UTC
@@nightly:run
@@nightly:history
{"run":"a1b2c3d4e5f60718293a4b5c6d7e8f90","job":"nightly","trigger":"timer","release":"r1","started_at":"2026-09-05T02:00:01Z","finished_at":"2026-09-05T02:00:02Z","duration_s":1,"attempts":0,"exit_status":75,"outcome":"skipped","inputs":{}}
{"run":"b2c3d4e5f60718293a4b5c6d7e8f9012","job":"nightly","trigger":"timer","release":"r1","started_at":"2026-09-04T02:00:01Z","finished_at":"2026-09-04T02:05:02Z","duration_s":301,"attempts":3,"exit_status":1,"outcome":"failure","inputs":{}}
{"run":"c3d4e5f60718293a4b5c6d7e8f901234","job":"nightly","trigger":"timer","release":"r1","started_at":"2026-09-03T02:00:01Z","finished_at":"2026-09-03T02:01:02Z","duration_s":61,"attempts":1,"exit_status":0,"outcome":"success","inputs":{}}
`}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := statuses[0]
	if got.LastOutcome != "skipped" || got.NextRun != "Sat 2026-09-06 02:00:00 UTC" || !got.JournalPersistent {
		t.Fatalf("record fields not surfaced: %#v", got)
	}
	if got.ConsecutiveFailures != 0 || got.Diverged {
		t.Fatalf("a skip is not a failure: %#v", got)
	}
	if got.LastAttempts != 0 || got.LastDurationSeconds != 1 {
		t.Fatalf("last run detail not surfaced: %#v", got)
	}
}

func TestScheduleStatusCountsConsecutiveFailuresFromRecords(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@journal
volatile
@@nightly:service
LoadState=loaded
ActiveState=failed
Result=exit-code
ExecMainStatus=1
@@nightly:timer
LoadState=loaded
ActiveState=active
@@nightly:run
@@nightly:history
{"run":"a1b2c3d4e5f60718293a4b5c6d7e8f90","job":"nightly","trigger":"timer","started_at":"2026-09-05T02:00:01Z","finished_at":"2026-09-05T02:00:02Z","duration_s":1,"attempts":2,"exit_status":1,"outcome":"failure","inputs":{}}
{"run":"b2c3d4e5f60718293a4b5c6d7e8f9012","job":"nightly","trigger":"timer","started_at":"2026-09-04T02:00:01Z","finished_at":"2026-09-04T02:05:02Z","duration_s":301,"attempts":2,"exit_status":null,"outcome":"timeout","inputs":{}}
{"run":"c3d4e5f60718293a4b5c6d7e8f901234","job":"nightly","trigger":"timer","started_at":"2026-09-03T02:00:01Z","finished_at":"2026-09-03T02:01:02Z","duration_s":61,"attempts":1,"exit_status":0,"outcome":"success","inputs":{}}
`}, true
		}
		return transport.Result{}, false
	}}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	statuses, err := e.scheduleStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := statuses[0]
	if got.ConsecutiveFailures != 2 || !got.Diverged || got.JournalPersistent {
		t.Fatalf("failures not counted: %#v", got)
	}
	if !strings.Contains(strings.Join(got.Issues, "; "), "last run failed: failure (exit 1)") {
		t.Fatalf("issue does not name the outcome: %#v", got.Issues)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine -run 'TestScheduleStatusPrefersTheRunRecord|TestScheduleStatusCountsConsecutive'`
Expected: FAIL (unknown fields).

- [ ] **Step 3: Implement**

`internal/engine/schedule_status.go`: add the fields to `StatusSchedule`; add `history []ScheduleRunRecord` and `next string` to `scheduleUnitObservation`; extend the batched command. Replace the loop body building `commands` with:

```go
	commands := []string{
		"printf '%s\\n' '@@journal'",
		"if [ -d /var/log/journal ]; then echo persistent; else echo volatile; fi",
	}
	for _, job := range jobs {
		unit := e.names().ScheduledJobUnit(job.Name)
		commands = append(commands,
			"printf '%s\\n' "+q("@@"+job.Name+":service"),
			"systemctl show "+q(unit+".service")+" --no-pager --property=LoadState --property=ActiveState --property=Result --property=ExecMainStatus",
			"printf '%s\\n' "+q("@@"+job.Name+":timer"),
			"systemctl show "+q(unit+".timer")+" --no-pager --property=LoadState --property=ActiveState --property=NextElapseUSecRealtime",
			"printf '%s\\n' "+q("@@"+job.Name+":run"),
			"cat "+q(e.names().ScheduledJobRunState(job.Name))+" 2>/dev/null || true",
			"printf '%s\\n' "+q("@@"+job.Name+":history"),
			scheduleHistoryCommand(unit, 20),
		)
	}
```

Parser: keep `values` for `key=value` kinds; for kind `history` collect raw lines; for marker `@@journal` (no colon) read the next non-empty line into `journalPersistent`. Replace the parse loop and `flush` with:

```go
	observed := map[string]map[string]scheduleUnitObservation{}
	journalPersistent := true
	name, kind := "", ""
	values := map[string]string{}
	var raw []string
	flush := func() {
		if name == "" || kind == "" {
			return
		}
		if observed[name] == nil {
			observed[name] = map[string]scheduleUnitObservation{}
		}
		exit, _ := strconv.Atoi(values["ExecMainStatus"])
		observed[name][kind] = scheduleUnitObservation{
			loadState: values["LoadState"], activeState: values["ActiveState"],
			result: values["Result"], exitStatus: exit,
			release: values["release"], startedAt: values["started_at"], attempt: values["attempt"],
			next:    values["NextElapseUSecRealtime"],
			history: parseScheduleRunRecords(strings.Join(raw, "\n")),
		}
		values = map[string]string{}
		raw = nil
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "@@") {
			flush()
			marker := strings.TrimPrefix(line, "@@")
			if marker == "journal" {
				name, kind = "", ""
				continue
			}
			name, kind, _ = strings.Cut(marker, ":")
			continue
		}
		if name == "" {
			if line == "volatile" {
				journalPersistent = false
			}
			continue
		}
		if kind == "history" {
			raw = append(raw, line)
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	flush()
```

Add `attempt`, `next string` and `history []ScheduleRunRecord` to `scheduleUnitObservation`. In the status assembly loop, after building `status`:

```go
		status.JournalPersistent = journalPersistent
		status.NextRun = timer.next
		if status.Running {
			status.Attempt, _ = strconv.Atoi(run.attempt)
		}
		records := observed[job.Name]["history"].history
		if len(records) > 0 {
			last := records[0]
			status.LastOutcome = last.Outcome
			status.LastDurationSeconds = last.DurationSeconds
			status.LastAttempts = last.Attempts
			for _, record := range records {
				if record.Outcome == "skipped" {
					continue
				}
				if record.Outcome != "failure" && record.Outcome != "timeout" {
					break
				}
				status.ConsecutiveFailures++
			}
		}
```

Replace the systemd-result issue block with:

```go
		switch {
		case status.LastOutcome == "failure" || status.LastOutcome == "timeout":
			exit := "?"
			if records[0].ExitStatus != nil {
				exit = strconv.Itoa(*records[0].ExitStatus)
			}
			status.Issues = append(status.Issues, fmt.Sprintf("last run failed: %s (exit %s)", status.LastOutcome, exit))
		case status.LastOutcome == "":
			if service.result != "" && service.result != "success" {
				status.Issues = append(status.Issues, fmt.Sprintf("last run failed: %s (exit %d)", service.result, service.exitStatus))
			}
		}
```

`internal/engine/status.go` human lines, replace the final `fmt.Fprintf(... "active; policy: ...")` with:

```go
		detail := fmt.Sprintf("active; policy: %s; timeout: %s", schedule.DeployLock, schedule.Timeout)
		if schedule.NextRun != "" {
			detail += "; next: " + schedule.NextRun
		}
		last := result
		if schedule.LastOutcome != "" {
			last = fmt.Sprintf("%s (%ds, %d attempt(s))", schedule.LastOutcome, schedule.LastDurationSeconds, schedule.LastAttempts)
		}
		detail += "; last: " + last
		if !schedule.JournalPersistent {
			detail += "; journal: volatile, history since boot only"
		}
		fmt.Fprintf(e.Opts.Out, "schedule %-11s %s\n", schedule.Name, detail)
```

and in the running branch append `fmt.Sprintf("; attempt: %d", schedule.Attempt)` when `schedule.Attempt > 0`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine ./cmd/ob`
Expected: PASS. The existing `TestScheduleStatusSurfacesTheLastSystemdFailure` still passes because it has no history block.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/schedule_status.go internal/engine/status.go internal/engine/schedule_test.go
git commit -m "feat(status): read scheduled-run outcomes, attempts and next elapse from the journal

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

### Task 5: CLI: `ob schedule list`, `ob schedule history`, `ob schedule logs`

**Files:**
- Create: `cmd/ob/schedule.go`
- Modify: `cmd/ob/ops.go:69-88` (move `schedule` group construction into `addScheduleCommands(root, g)` in the new file), `cmd/ob/output.go:76-118` (matrix), `cmd/ob/output_test.go:492-530` (wantMatrix), `site/src/content/docs/reference/policies.mdx:98-102`
- Test: `cmd/ob/output_test.go`, `cmd/ob/docs_test.go` (regenerated `cli.mdx`)

**Interfaces:**
- Consumes: `engine.ScheduleList`, `engine.ScheduleHistory`, `engine.ScheduleLogs`, `loadAllLenient`, `connect`, `newUI`, `writeFiniteSuccess`, `writeStructuredReadFailure`, `writeStructuredCommandFailure`, `isStructuredOutput`.
- Produces matrix rows: `"ob schedule list": {finite_envelope, JSON}`, `"ob schedule history": {finite_envelope, JSON}`, `"ob schedule logs": {operator_passthrough, JSON, NDJSON}`.

- [ ] **Step 1: Update the closed-matrix test first**

In `cmd/ob/output_test.go` `wantMatrix` add:

```go
		"ob schedule list":    {Class: "finite_envelope", JSON: true},
		"ob schedule history": {Class: "finite_envelope", JSON: true},
		"ob schedule logs":    {Class: "operator_passthrough", JSON: true, NDJSON: true},
```

Run: `go test ./cmd/ob -run TestLeafOutputMatrixIsClosedAndHasNoAliases`
Expected: FAIL (matrix changed).

- [ ] **Step 2: Implement `cmd/ob/schedule.go`**

Move the existing `scheduleCmd`/`scheduleApplyCmd` construction from `ops.go` into `addScheduleCommands(root *cobra.Command, g *globalFlags)` and call it from where `root.AddCommand(scheduleCmd)` was. Then add:

```go
	var listCmd = &cobra.Command{
		Use:   "list",
		Short: "declared scheduled jobs with timer state and next elapse",
		Long:  "List every job that declares a schedule beside what the host's timer says: whether it is active, when it fires next, and when it last fired. Reads only.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			defer cleanup()
			jobs, err := e.ScheduleList(cmd.Context())
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "schedule_list_failed", "scheduled jobs could not be listed", err)
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"jobs": jobs})
			}
			if len(jobs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no scheduled jobs declared")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "JOB\tCRON\tTZ\tTIMER\tNEXT\tLAST TRIGGER\tPOLICY\tTIMEOUT")
			for _, j := range jobs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.Name, j.Cron, j.Timezone, orDash(j.TimerState), orDash(j.NextRun), orDash(j.LastTrigger), j.DeployLock, j.Timeout)
			}
			return w.Flush()
		},
	}
```

History:

```go
	var historyCount int
	historyCmd := &cobra.Command{
		Use:   "history <job>",
		Short: "run records of one scheduled job, newest first",
		Long:  "Read the run records the host wrote for one scheduled job. Each record is one activation: run id, trigger, release, start and end, attempts, exit status, outcome and, for a manual run, its inputs.\n\nRecords live in the host journal under the job's unit with syslog identifier ob-run; retention is the journal's. Reads only.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			defer cleanup()
			records, err := e.ScheduleHistory(cmd.Context(), args[0], historyCount)
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "schedule_history_failed", "run history could not be read", err)
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"job": args[0], "runs": records})
			}
			if len(records) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no recorded runs for %s\n", args[0])
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "STARTED\tOUTCOME\tDURATION\tATTEMPTS\tEXIT\tTRIGGER\tRELEASE\tRUN")
			for _, r := range records {
				exit := "-"
				if r.ExitStatus != nil {
					exit = strconv.Itoa(*r.ExitStatus)
				}
				fmt.Fprintf(w, "%s\t%s\t%ds\t%d\t%s\t%s\t%s\t%s\n", r.StartedAt, r.Outcome, r.DurationSeconds, r.Attempts, exit, r.Trigger, orDash(r.Release), r.Run)
			}
			return w.Flush()
		},
	}
	historyCmd.Flags().IntVarP(&historyCount, "count", "n", 20, "number of newest runs to show")
```

Logs: mirror `ob logs` in `cmd/ob/ops.go:229-300` exactly (JSON buffers `stdout`/`stderr` into `{"job","run","stdout","stderr","passthrough_unredacted":true}`; NDJSON uses the same streaming writer `ob logs` uses; human streams to the terminal). Flags: `--run <id>` and `-n/--tail` (default 200). Failure code `schedule_logs_failed`, message `run logs could not be read`.

`orDash`:

```go
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
```

Add the three matrix rows to `cliOutputMatrix` in `output.go`. In `policies.mdx` add `` `ob schedule history` `` and `` `ob schedule list` `` to the Finite envelope row (alphabetical position after `ob preview`) and `` `ob schedule logs` `` to the `Operator passthrough | finite only | yes` row beside `ob logs`.

- [ ] **Step 3: Regenerate docs and run tests**

Run: `just build && just docs-generate && go test ./cmd/ob ./internal/... && just docs-generate-check`
Expected: PASS; `cli.mdx` gains the three commands.

- [ ] **Step 4: Commit**

```bash
git add cmd/ob/schedule.go cmd/ob/ops.go cmd/ob/output.go cmd/ob/output_test.go site/src/content/docs/reference/policies.mdx site/src/content/docs/reference/cli.mdx
git commit -m "feat(cli): ob schedule list, history and logs read the host's run records

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

## Slice 2: Retry and notify

### Task 6: Schema, validation and defaults for `retry` and `notify`

**Files:**
- Modify: `internal/app/types.go:268-274` (`JobSchedule`), `internal/app/constraints.go:173` (add `eScheduleNotify`), `internal/app/validate.go:573-584` (`validateJobSchedule`), `internal/app/schedule.go:26-64` (`ScheduledJob`, `ScheduledJobs`)
- Test: `internal/app/schedule_test.go`

**Interfaces:**
- Produces:
  - `type JobRetry struct { Attempts int; Backoff string; MaxBackoff string }` JSON `attempts`, `backoff`, `max_backoff`, with `description` and `default` tags.
  - `JobSchedule.Retry *JobRetry json:"retry,omitempty"`, `JobSchedule.Notify []string json:"notify,omitempty"`.
  - `ScheduledJob` gains `RetryAttempts int`, `RetryBackoff time.Duration`, `RetryMaxBackoff time.Duration`, `Notify []string`, resolved with defaults in `ScheduledJobs()`.
  - `func scheduleRetryWorstCase(attempts int, backoff, max time.Duration) time.Duration` = sum over the attempts-1 sleeps of `min(backoff·2^i, max)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/app/schedule_test.go`:

```go
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
		"too many attempts": {`{cron: "0 * * * *", retry: {attempts: 11}}`, "project_invalid"},
		"zero attempts":     {`{cron: "0 * * * *", retry: {attempts: 0}}`, "project_invalid"},
		"backoff over max":  {`{cron: "0 * * * *", retry: {attempts: 2, backoff: 20m, max_backoff: 10m}}`, "project_invalid"},
		"backoff exceeds timeout": {`{cron: "0 * * * *", timeout: 5m, retry: {attempts: 3, backoff: 2m, max_backoff: 30m}}`, "project_invalid"},
		"unknown notify":    {`{cron: "0 * * * *", notify: [warning]}`, "project_invalid"},
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
```

Add `"errors"`, `"strings"`, `"time"` to that test file's imports if absent.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/app -run 'TestScheduledJobRetry'`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

`types.go`:

```go
type JobSchedule struct {
	Cron       string    `json:"cron" ...unchanged...`
	Timezone   string    ...
	Timeout    string    ...
	CatchUp    bool      ...
	DeployLock string    ...
	Retry      *JobRetry `json:"retry,omitempty" description:"Bounded retry inside one timer firing. Attempts run under the same locks and the same timeout; a timeout ends the run."`
	Notify     []string  `json:"notify,omitempty" description:"Run outcomes that send the configured notifications: success, failure, timeout, skipped." default:"failure, timeout"`
}

type JobRetry struct {
	Attempts   int    `json:"attempts,omitempty" description:"Total attempts including the first, 1 to 10." default:"1" example:"3"`
	Backoff    string `json:"backoff,omitempty" description:"Sleep before the second attempt; it doubles after each failure." default:"30s" example:"1m"`
	MaxBackoff string `json:"max_backoff,omitempty" description:"Upper bound for the doubling sleep." default:"10m" example:"30m"`
}
```

`constraints.go` beside `eNotifyEvent`: `eScheduleNotify = []string{"success", "failure", "timeout", "skipped"}`.

`schedule.go`: add constants and resolution:

```go
const (
	defaultRetryAttempts   = 1
	defaultRetryBackoff    = 30 * time.Second
	defaultRetryMaxBackoff = 10 * time.Minute
)

var defaultScheduleNotify = []string{"failure", "timeout"}

// scheduleRetryWorstCase is the longest a run can spend asleep between
// attempts. Validation keeps it under the timeout, so the last attempt can
// always start.
func scheduleRetryWorstCase(attempts int, backoff, max time.Duration) time.Duration {
	var total time.Duration
	sleep := backoff
	for i := 1; i < attempts; i++ {
		if sleep > max {
			sleep = max
		}
		total += sleep
		sleep *= 2
	}
	return total
}

func (s *JobSchedule) retryPolicy() (int, time.Duration, time.Duration) {
	attempts, backoff, max := defaultRetryAttempts, defaultRetryBackoff, defaultRetryMaxBackoff
	if s.Retry != nil {
		if s.Retry.Attempts > 0 {
			attempts = s.Retry.Attempts
		}
		if d, ok := ParseDuration(s.Retry.Backoff); ok && s.Retry.Backoff != "" {
			backoff = d
		}
		if d, ok := ParseDuration(s.Retry.MaxBackoff); ok && s.Retry.MaxBackoff != "" {
			max = d
		}
	}
	return attempts, backoff, max
}
```

Extend `ScheduledJob` with `RetryAttempts int`, `RetryBackoff, RetryMaxBackoff time.Duration`, `Notify []string`; in `ScheduledJobs()` fill them:

```go
		attempts, backoff, max := w.Schedule.retryPolicy()
		notify := w.Schedule.Notify
		if len(notify) == 0 {
			notify = append([]string(nil), defaultScheduleNotify...)
		}
		out = append(out, ScheduledJob{
			Name: name, Cron: w.Schedule.Cron, Timezone: tz, Calendar: cal,
			Timeout: w.Schedule.Timeout, CatchUp: w.Schedule.CatchUp, DeployLock: deployLock,
			RetryAttempts: attempts, RetryBackoff: backoff, RetryMaxBackoff: max, Notify: notify,
		})
```

`validate.go` `validateJobSchedule`, after the timeout check:

```go
	if s.Retry != nil {
		if s.Retry.Attempts < 0 || s.Retry.Attempts > 10 {
			return errf("project_invalid", path+".retry.attempts", "", "attempts must be between 1 and 10, got %d", s.Retry.Attempts)
		}
		if raw, err := yamlZeroAttempts(s); err == nil && raw {
			return errf("project_invalid", path+".retry.attempts", "", "attempts must be between 1 and 10, got 0")
		}
		if err := gDur.checkOptional(path+".retry.backoff", s.Retry.Backoff); err != nil {
			return err
		}
		if err := gDur.checkOptional(path+".retry.max_backoff", s.Retry.MaxBackoff); err != nil {
			return err
		}
		attempts, backoff, max := s.retryPolicy()
		if backoff > max {
			return errf("project_invalid", path+".retry.backoff", "", "backoff %s exceeds max_backoff %s", s.Retry.Backoff, s.Retry.MaxBackoff)
		}
		timeout := time.Hour
		if s.Timeout != "" {
			if d, ok := ParseDuration(s.Timeout); ok {
				timeout = d
			}
		}
		if worst := scheduleRetryWorstCase(attempts, backoff, max); worst >= timeout {
			return errf("project_invalid", path+".retry", "",
				"worst-case backoff %s is not smaller than timeout %s; later attempts could never start", worst, timeout)
		}
	}
	for i, outcome := range s.Notify {
		if err := checkEnum(indexed(path+".notify", i), outcome, eScheduleNotify); err != nil {
			return err
		}
	}
```

`attempts: 0` cannot be told from "absent" after decoding into `int`. Make `Attempts` a `*int`? Simpler and honest: change `JobRetry.Attempts` to `int` and treat `0` as "not set" only when the whole `retry` block is absent; when `retry:` is present with `attempts: 0`, the raw YAML check `applyDefaults` already receives `raw map[string]any`. Rather than a raw lookup, declare `Attempts *int` with `default:"1"` and validate `*Attempts` in 1..10 when non-nil; `retryPolicy` uses `*Attempts` when non-nil. Delete the `yamlZeroAttempts` line above and implement with the pointer. `schemaTagValue` handles `*int` via `deref` for the default tag; verify with `TestPublishedSchemaDocumentsEveryPublicField`.

- [ ] **Step 4: Run tests and regenerate schema**

Run: `go test ./internal/app && go run ./cmd/ob schema --out docs/onebox.run-v1.schema.json && go test ./internal/app ./cmd/ob-docgen`
Expected: PASS. `TestContractDidNotMove` still passes: no existing case declares `retry` or `notify`, and neither field renders into Compose.

- [ ] **Step 5: Commit**

```bash
git add internal/app docs/onebox.run-v1.schema.json
git commit -m "feat(schema): schedule.retry and schedule.notify with validation against the timeout

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

### Task 7: Runner retry loop and per-outcome notifications

**Files:**
- Modify: `internal/engine/schedule.go` (`scheduleRunnerScript`, `pinnedScheduleRunnerScript`, `scheduleFailureNotifier`)
- Test: `internal/engine/schedule_test.go`

**Interfaces:**
- Produces: `func scheduleAttemptLoop(job app.ScheduledJob, compose string) []string`; notifier renders a success body and a failure body per notification and selects by outcome; `scheduleFailureNotifier(job string)` becomes `scheduleNotifier(job app.ScheduledJob)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestScheduledJobRunnerRetriesWithCappedDoublingBackoff(t *testing.T) {
	job := app.ScheduledJob{Name: "nightly", Timeout: "45m", DeployLock: "exclusive",
		RetryAttempts: 3, RetryBackoff: 30 * time.Second, RetryMaxBackoff: 10 * time.Minute}
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil)
	for _, want := range []string{
		"max_attempts=3", "backoff=30", "max_backoff=600",
		"attempt=1", "while :; do", "write_state \"$attempt\"",
		"status=0", "|| status=$?", "[ \"$status\" -eq 0 ] && exit 0",
		"if [ \"$attempt\" -ge \"$max_attempts\" ]; then exit \"$status\"; fi",
		"sleep \"$backoff\"", "backoff=$((backoff * 2))", "attempt=$((attempt + 1))",
	} {
		if !strings.Contains(runner, want) {
			t.Errorf("runner is missing %q:\n%s", want, runner)
		}
	}
	single := scheduleRunnerScript("sample", app.ScheduledJob{Name: "nightly", Timeout: "1h", DeployLock: "exclusive", RetryAttempts: 1}, names, "/var/lib/ob/sample/lock", nil)
	if strings.Contains(single, "while :; do") {
		t.Errorf("a single-attempt job must not carry a retry loop:\n%s", single)
	}
	for _, script := range []string{runner, single} {
		command := exec.CommandContext(context.Background(), "sh", "-n")
		command.Stdin = strings.NewReader(script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("runner is not valid POSIX shell: %v: %s\n%s", err, output, script)
		}
	}
}

func TestScheduledJobNotifierSelectsBodiesByOutcome(t *testing.T) {
	cfg := testConfig()
	cfg.Notifications = map[string]app.Notification{
		"ops": {Webhook: "https://hooks.example.com/ops", On: []string{"success", "failure"}, Format: "json"},
	}
	f := &transport.Fake{TargetName: "root@example.internal"}
	e := New(cfg, testProject(t), f, Options{Environment: "production", Out: &bytes.Buffer{}, Sleep: noSleep})
	script, err := e.scheduleNotifier(app.ScheduledJob{Name: "nightly", Notify: []string{"success", "failure", "skipped"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`case " success failure skipped " in *" $outcome "*) ;; *) exit 0 ;; esac`,
		`"status":"ok"`,
		`"status":"fail"`,
		`if [ "$outcome" = success ]; then`,
		`"$detail"`,
		`detail="run ${INVOCATION_ID:-?}: $outcome after $attempt attempt(s) in ${duration}s (exit $status)"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("notifier is missing %q:\n%s", want, script)
		}
	}
}
```

Update `TestScheduledJobFailureNotifierUsesConfiguredWebhooks` to call `e.scheduleNotifier(app.ScheduledJob{Name: "nightly", Notify: []string{"failure", "timeout"}})` and keep its expectations; the `success-only` webhook is still absent because its `On` excludes failure and the job's `Notify` excludes success.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine -run 'TestScheduledJobRunnerRetries|TestScheduledJobNotifierSelects'`
Expected: FAIL.

- [ ] **Step 3: Implement**

`scheduleAttemptLoop`:

```go
// scheduleAttemptLoop runs the container until it exits 0 or the attempts are
// spent. Backoff doubles and is capped; every sleep happens under the locks the
// run already holds, which is why validation keeps the sum under the timeout.
func scheduleAttemptLoop(job app.ScheduledJob, compose string) []string {
	if job.RetryAttempts <= 1 {
		return []string{"write_state 1", compose}
	}
	return []string{
		fmt.Sprintf("max_attempts=%d", job.RetryAttempts),
		fmt.Sprintf("backoff=%d", int(math.Ceil(job.RetryBackoff.Seconds()))),
		fmt.Sprintf("max_backoff=%d", int(math.Ceil(job.RetryMaxBackoff.Seconds()))),
		"attempt=1",
		"while :; do",
		"  write_state \"$attempt\"",
		"  status=0",
		"  " + compose + " || status=$?",
		"  [ \"$status\" -eq 0 ] && exit 0",
		"  if [ \"$attempt\" -ge \"$max_attempts\" ]; then exit \"$status\"; fi",
		"  echo \"onebox: attempt $attempt of $max_attempts exited $status; retrying in ${backoff}s\" >&2",
		"  sleep \"$backoff\"",
		"  backoff=$((backoff * 2))",
		"  [ \"$backoff\" -gt \"$max_backoff\" ] && backoff=$max_backoff",
		"  attempt=$((attempt + 1))",
		"done",
	}
}
```

Both runners: replace `lines = append(lines, "write_state 1", compose, "")` with `lines = append(lines, scheduleAttemptLoop(job, compose)...); lines = append(lines, "")` (pinned keeps `"/usr/bin/flock --unlock 8"` before the loop). `pinnedScheduleRunnerScript` takes `job app.ScheduledJob` instead of `job string`; update its two callers.

Notifier: rename to `scheduleNotifier(job app.ScheduledJob)`; after the record lines:

```go
	lines = append(lines,
		`detail="run ${INVOCATION_ID:-?}: $outcome after $attempt attempt(s) in ${duration}s (exit $status)"`,
		`case " `+strings.Join(job.Notify, " ")+` " in *" $outcome "*) ;; *) exit 0 ;; esac`,
	)
```

For each notification prepare two payloads: `Status: "ok", Error: ""` and `Status: "fail", Error: scheduleNotificationDetail` where `const scheduleNotificationDetail = "__ONEBOX_SCHEDULE_DETAIL__"`; substitute both `scheduleNotificationTimestamp` with `"$ts"` and `scheduleNotificationDetail` with `"$detail"` (same `strings.Cut` technique, applied twice through a small helper `shellBody(body string) string`). Emit:

```
ts=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
if [ "$outcome" = success ]; then
  <success sends, one per notification whose Prepare returned non-nil for "ok">
else
  <failure sends, one per notification whose Prepare returned non-nil for "fail">
fi
wait || true
exit 0
```

Both branches may be empty; render `:` in an empty branch so the shell stays valid. The verb stays `"scheduled job " + job.Name`; the `Text` line for text-format webhooks carries the detail through the same placeholder.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine
git commit -m "feat(schedule): bounded retry in the runner and per-outcome notifications

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

## Slice 3: Inputs and manual runs

### Task 8: Schema and validation for `inputs`; defaults rendered into Compose

**Files:**
- Modify: `internal/app/types.go:136-174` (`Workload.Inputs`), `internal/app/validate.go:436-458`, `internal/app/generate.go:347` (environment), `internal/app/schedule.go` (`ScheduledJob.Inputs`), `internal/app/constraints.go`, `internal/app/names.go:242`
- Test: `internal/app/schedule_test.go`, `internal/app/generate_test.go` (or the nearest existing render test file)

**Interfaces:**
- Produces:
  - `type JobInput struct { Enum []string; Pattern string; Default string; Description string }` JSON `enum, pattern, default, description`.
  - `Workload.Inputs map[string]JobInput json:"inputs,omitempty"`.
  - `ScheduledJob.Inputs map[string]JobInput`.
  - `func ValidateJobInputValues(w Workload, values map[string]string) error` (used by the engine for overrides): unknown name, pattern/enum mismatch, charset, length.
  - `var gInputName = grammar{"input name", ^[A-Z][A-Z0-9_]*$}`, `const ReservedInputPrefix = "ONEBOX_"`, `func InputValueAllowed(v string) bool` (≤256 bytes, no `"`, `\`, control chars).
  - `func (n Names) ScheduledJobRunInputs(job string) string` = `<AppDir>/schedule/<job>.inputs`.

- [ ] **Step 1: Write the failing tests**

```go
func TestJobInputsValidateNamesConstraintsAndDefaults(t *testing.T) {
	base := `api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  sync:
    role: job
    image: x:1
    data_effect: %s
    env: {MODE: fast}
    %s
    inputs:
      %s
`
	load := func(effect, schedule, inputs string) error {
		_, err := LoadBytes([]byte(fmt.Sprintf(base, effect, schedule, inputs)), "ob.yml")
		return err
	}
	good := "SOURCE: {enum: [catalog, prices], default: catalog, description: Which upstream.}"
	if err := load("none", `schedule: {cron: "0 * * * *"}`, good); err != nil {
		t.Fatalf("valid inputs refused: %v", err)
	}
	for name, tc := range map[string]struct{ effect, schedule, inputs string }{
		"no schedule":        {"none", "", good},
		"destructive job":    {"destructive", `schedule: {cron: "0 * * * *"}`, good},
		"lowercase name":     {"none", `schedule: {cron: "0 * * * *"}`, "source: {enum: [a], default: a}"},
		"reserved prefix":    {"none", `schedule: {cron: "0 * * * *"}`, "ONEBOX_X: {enum: [a], default: a}"},
		"collides with env":  {"none", `schedule: {cron: "0 * * * *"}`, "MODE: {enum: [a], default: a}"},
		"enum and pattern":   {"none", `schedule: {cron: "0 * * * *"}`, "S: {enum: [a], pattern: '^a$', default: a}"},
		"neither":            {"none", `schedule: {cron: "0 * * * *"}`, "S: {default: a}"},
		"default off enum":   {"none", `schedule: {cron: "0 * * * *"}`, "S: {enum: [a], default: b}"},
		"default off pattern": {"none", `schedule: {cron: "0 * * * *"}`, "S: {pattern: '^[0-9]+$', default: x}"},
		"quote in default":   {"none", `schedule: {cron: "0 * * * *"}`, `S: {pattern: '.*', default: 'a"b'}`},
		"bad regex":          {"none", `schedule: {cron: "0 * * * *"}`, "S: {pattern: '(', default: a}"},
	} {
		t.Run(name, func(t *testing.T) {
			var e *Error
			if err := load(tc.effect, tc.schedule, tc.inputs); !errors.As(err, &e) || e.Code != "project_invalid" {
				t.Fatalf("err = %v, want project_invalid", err)
			}
		})
	}
}

func TestValidateJobInputValuesChecksOverrides(t *testing.T) {
	w := Workload{Role: RoleJob, Inputs: map[string]JobInput{
		"SOURCE": {Enum: []string{"catalog", "prices"}, Default: "catalog"},
		"SINCE":  {Pattern: `^([0-9]{4}-[0-9]{2}-[0-9]{2})?$`, Default: ""},
	}}
	if err := ValidateJobInputValues(w, map[string]string{"SOURCE": "prices", "SINCE": "2026-09-01"}); err != nil {
		t.Fatal(err)
	}
	for name, values := range map[string]map[string]string{
		"unknown":       {"OTHER": "x"},
		"off enum":      {"SOURCE": "reviews"},
		"off pattern":   {"SINCE": "yesterday"},
		"backslash":     {"SINCE": `2026\-09-01`},
		"newline":       {"SOURCE": "prices\n"},
	} {
		if err := ValidateJobInputValues(w, values); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if InputValueAllowed(strings.Repeat("a", 257)) {
		t.Error("an oversized value was allowed")
	}
}

func TestScheduledJobInputDefaultsRenderIntoTheComposeEnvironment(t *testing.T) {
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  sync:
    role: job
    image: x:1
    data_effect: none
    env: {MODE: fast}
    schedule: {cron: "0 * * * *"}
    inputs:
      SOURCE: {enum: [catalog, prices], default: catalog}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderForTest(t, spec) // use the same helper the existing generate tests use to render Compose YAML
	for _, want := range []string{"SOURCE: catalog", "MODE: fast"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered runtime is missing %q:\n%s", want, rendered)
		}
	}
	jobs, _ := spec.ScheduledJobs()
	if jobs[0].Inputs["SOURCE"].Default != "catalog" {
		t.Fatalf("scheduled job did not carry its inputs: %#v", jobs[0])
	}
}
```

Before writing the render test, locate the render helper with `rtk rg -n 'func render.*\(t \*testing.T' internal/app/*_test.go` and use its real name in place of `renderForTest`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/app -run 'TestJobInputs|TestValidateJobInputValues|TestScheduledJobInputDefaults'`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

`types.go`, in `Workload` under `// Job only.`:

```go
	Inputs map[string]JobInput `json:"inputs,omitempty" description:"Declared parameters of a scheduled job, exposed as environment variables. Names are upper-case identifiers; each declares exactly one of enum or pattern and a default. A timer firing uses the defaults; ob schedule run may override them."`
```

and

```go
type JobInput struct {
	Enum        []string `json:"enum,omitempty" description:"Accepted values." example:"catalog"`
	Pattern     string   `json:"pattern,omitempty" description:"Regular expression the whole value must match." example:"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"`
	Default     string   `json:"default" description:"Value used by a timer firing and by a manual run that does not override it. Must satisfy the input's own constraint."`
	Description string   `json:"description,omitempty" description:"What the input controls."`
}
```

`constraints.go`:

```go
	gInputName = grammar{"input name", regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`),
		"upper-case letters, digits and underscores, starting with a letter"}
```

(match the existing `grammar` literal shape; the third field is the hint string if the struct has one).

`schedule.go` (app):

```go
// ReservedInputPrefix is Onebox's environment namespace; declared inputs may
// not enter it, and the manual-run metadata line lives there.
const ReservedInputPrefix = "ONEBOX_"

const maxInputValueBytes = 256

// InputValueAllowed is the charset that lets the runner pass a value through
// to the container and into the run record without escaping anything.
func InputValueAllowed(v string) bool {
	if len(v) > maxInputValueBytes {
		return false
	}
	for _, r := range v {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (in JobInput) accepts(v string) bool {
	if !InputValueAllowed(v) {
		return false
	}
	if len(in.Enum) > 0 {
		for _, allowed := range in.Enum {
			if v == allowed {
				return true
			}
		}
		return false
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return false
	}
	return re.MatchString(v) && re.FindStringIndex(v) != nil && wholeMatch(re, v)
}

func wholeMatch(re *regexp.Regexp, v string) bool {
	loc := re.FindStringIndex(v)
	return loc != nil && loc[0] == 0 && loc[1] == len(v)
}

// ValidateJobInputValues checks operator-supplied overrides against the
// declaration. Defaults were checked at load; this is the other half.
func ValidateJobInputValues(w Workload, values map[string]string) error {
	for _, name := range sortedKeys(values) {
		in, ok := w.Inputs[name]
		if !ok {
			return fmt.Errorf("input %s is not declared", name)
		}
		if !in.accepts(values[name]) {
			return fmt.Errorf("input %s: %q is not an accepted value", name, values[name])
		}
	}
	return nil
}
```

(`sortedKeys` is generic over map value type in this package? Check its signature at `internal/app/schedule.go` usage; if it is `map[string]Workload`-specific, add a local `sortedStringKeys(map[string]string)`.) In `ScheduledJobs()` add `Inputs: w.Inputs` to the literal and `Inputs map[string]JobInput` to `ScheduledJob`.

`validate.go` inside `if w.IsJob() {` after the pinned block:

```go
		if len(w.Inputs) > 0 {
			if w.Schedule == nil {
				return errf("project_invalid", path+".inputs", "", "inputs belong to a scheduled job; declare schedule or remove inputs")
			}
			if w.DataEffect != DataEffectNone {
				return errf("project_invalid", path+".inputs", "",
					"inputs require data_effect %q; a %q job keeps the sealed plan of ob job run for operator-initiated runs", DataEffectNone, w.DataEffect)
			}
			for _, name := range sortedKeys(w.Inputs) {
				ip := path + ".inputs." + name
				if err := gInputName.check(ip, name); err != nil {
					return err
				}
				if strings.HasPrefix(name, ReservedInputPrefix) {
					return errf("project_invalid", ip, "", "%s is in Onebox's environment namespace", ReservedInputPrefix)
				}
				if _, clash := w.Env[name]; clash {
					return errf("project_invalid", ip, "", "input %s collides with an env key of the same name", name)
				}
				in := w.Inputs[name]
				switch {
				case len(in.Enum) > 0 && in.Pattern != "":
					return errf("project_invalid", ip, "", "declare exactly one of enum or pattern")
				case len(in.Enum) == 0 && in.Pattern == "":
					return errf("project_invalid", ip, "", "declare exactly one of enum or pattern")
				}
				if in.Pattern != "" {
					if _, err := regexp.Compile(in.Pattern); err != nil {
						return errf("project_invalid", ip+".pattern", "", "%v", err)
					}
				}
				for i, v := range in.Enum {
					if !InputValueAllowed(v) {
						return errf("project_invalid", indexed(ip+".enum", i), "", "enum values may not contain quotes, backslashes or control characters and are at most %d bytes", maxInputValueBytes)
					}
				}
				if !in.accepts(in.Default) {
					return errf("project_invalid", ip+".default", "", "default %q does not satisfy the input's own constraint", in.Default)
				}
			}
		}
```

Also in the `else if` branch for non-jobs add `|| len(w.Inputs) > 0` to the condition and mention `inputs` in the message.

`generate.go:347`:

```go
	env := stringMap(w.Env)
	if len(w.Inputs) > 0 {
		if env == nil {
			env = map[string]any{}
		}
		for name, in := range w.Inputs {
			env[name] = in.Default
		}
	}
	if len(env) > 0 {
		svc["environment"] = env
	}
```

`names.go`:

```go
// ScheduledJobRunInputs is the one-shot file ob schedule run leaves for the
// next manual activation. The runner consumes and deletes it.
func (n Names) ScheduledJobRunInputs(job string) string {
	return path.Join(n.AppDir(), "schedule", job+".inputs")
}
```

- [ ] **Step 4: Run tests, regenerate schema**

Run: `go test ./internal/app && go run ./cmd/ob schema --out docs/onebox.run-v1.schema.json && go test ./internal/app ./cmd/ob-docgen`
Expected: PASS. If `TestContractDidNotMove` fails, no existing case uses `inputs`, so investigate before ever running `-update`.

- [ ] **Step 5: Commit**

```bash
git add internal/app docs/onebox.run-v1.schema.json
git commit -m "feat(schema): declared inputs for scheduled jobs, rendered as environment defaults

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

### Task 9: Runner consumes manual inputs; host systemd floor

**Files:**
- Modify: `internal/engine/schedule.go` (both runners, `SyncSchedules`)
- Test: `internal/engine/schedule_test.go`

**Interfaces:**
- Produces: `func scheduleInputsLines(inputsPath string) []string` inserted after the preamble in both runners; the compose command gains `"$@"` after `run --rm --no-deps`; `SyncSchedules` checks `systemctl --version` ≥ 252 when any job has inputs; error text: `job %s declares inputs, which need systemd 252 or newer on the host for $TRIGGER_UNIT; the host runs %s`.

- [ ] **Step 1: Write the failing tests**

```go
func TestScheduledJobRunnerConsumesManualInputsWithoutShellInterpolation(t *testing.T) {
	job := app.ScheduledJob{Name: "sync", Timeout: "45m", DeployLock: "pinned", RetryAttempts: 1,
		Inputs: map[string]app.JobInput{"SOURCE": {Enum: []string{"catalog"}, Default: "catalog"}}}
	names := app.Names{App: "sample", BasePath: "/var/lib/ob"}
	runner := scheduleRunnerScript("sample", job, names, "/var/lib/ob/sample/lock", nil)
	for _, want := range []string{
		"inputs_file='/var/lib/ob/sample/schedule/sync.inputs'",
		`if [ -z "${TRIGGER_UNIT:-}" ] && [ -f "$inputs_file" ]; then`,
		`while IFS= read -r line || [ -n "$line" ]; do`,
		`ONEBOX_OPERATION=*) operation=${line#ONEBOX_OPERATION=} ;;`,
		`[A-Z]*=*) set -- "$@" -e "$line"`,
		`rm -f "$inputs_file"`,
		`run --rm --no-deps "$@" --name 'sample-sync-1' 'sync'`,
	} {
		if !strings.Contains(runner, want) {
			t.Errorf("runner is missing %q:\n%s", want, runner)
		}
	}
	if strings.Contains(runner, ". \"$inputs_file\"") || strings.Contains(runner, "eval") {
		t.Fatalf("runner evaluates the inputs file as shell:\n%s", runner)
	}
	command := exec.CommandContext(context.Background(), "sh", "-n")
	command.Stdin = strings.NewReader(runner)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("runner is not valid POSIX shell: %v: %s\n%s", err, output, runner)
	}
	// The consume block precedes the locks so a skipped manual run cannot
	// leave its inputs for the next timer firing.
	if strings.Index(runner, "inputs_file=") > strings.Index(runner, "flock --exclusive --nonblock --conflict-exit-code 75 9") {
		t.Fatalf("inputs are consumed after the lock:\n%s", runner)
	}
}

func TestSyncSchedulesRequiresSystemd252ForInputs(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Inputs:   map[string]app.JobInput{"SOURCE": {Enum: []string{"a"}, Default: "a"}},
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "list-unit-files"):
			return transport.Result{}, true
		case strings.Contains(cmd, "systemd-analyze calendar"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "command -v flock"):
			return transport.Result{Stdout: "ok\n"}, true
		case strings.Contains(cmd, "systemctl --version"):
			return transport.Result{Stdout: "systemd 249 (249.11-0ubuntu3)\n"}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.SyncSchedules(context.Background())
	if err == nil || !strings.Contains(err.Error(), "systemd 252") {
		t.Fatalf("old systemd was accepted for a job with inputs: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine -run 'TestScheduledJobRunnerConsumesManualInputs|TestSyncSchedulesRequiresSystemd252'`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// scheduleInputsLines consumes the one-shot inputs file on a manual
// activation. Values reach the container as -e arguments, never as shell
// text, and the file is gone before any lock is taken so a skipped manual run
// cannot hand its inputs to the next timer firing.
func scheduleInputsLines(inputsPath string) []string {
	return []string{
		"inputs_file=" + q(inputsPath),
		"if [ -z \"${TRIGGER_UNIT:-}\" ] && [ -f \"$inputs_file\" ]; then",
		"  while IFS= read -r line || [ -n \"$line\" ]; do",
		"    case \"$line\" in",
		"      ONEBOX_OPERATION=*) operation=${line#ONEBOX_OPERATION=} ;;",
		"      [A-Z]*=*) set -- \"$@\" -e \"$line\"; key=${line%%=*}; value=${line#*=}; inputs_json=\"${inputs_json:+$inputs_json,}\\\"$key\\\":\\\"$value\\\"\" ;;",
		"    esac",
		"  done <\"$inputs_file\"",
		"  rm -f \"$inputs_file\"",
		"fi",
	}
}
```

Order inside both runners: shebang, `set -eu`, `install -d`, then `operation=''`, `inputs_json=''`, then `scheduleInputsLines(...)`, then the flocks and the rest; move the two variable initialisations out of `scheduleRunPreamble` (keep the preamble's `state`, `tmp`, `started_*`, `trigger`, `write_state`). The compose command string in both runners becomes:

```go
	compose := "/usr/bin/docker compose -p " + q(application) + " --project-directory " + projectDir +
		" -f " + projectDir + "/" + q("compose.yaml") + scheduleRuntimeEnvArgs(projectDir, runtimeEnvFiles) +
		" run --rm --no-deps \"$@\" --name " + q(container) + " " + q(job.Name)
```

In `SyncSchedules`, before the per-job loop:

```go
	if needsTriggerUnit(jobs) {
		res, err := e.T.Run(ctx, "systemctl --version 2>/dev/null | head -1")
		if err != nil {
			return err
		}
		if version, ok := systemdVersion(res.Stdout); !ok || version < 252 {
			return fmt.Errorf("a job declares inputs, which need systemd 252 or newer on the host for $TRIGGER_UNIT; the host reports %q", strings.TrimSpace(res.Stdout))
		}
	}
```

with

```go
func needsTriggerUnit(jobs []app.ScheduledJob) bool {
	for _, job := range jobs {
		if len(job.Inputs) > 0 {
			return true
		}
	}
	return false
}

// systemdVersion reads the leading number from `systemd 255 (255.4-1ubuntu8)`.
func systemdVersion(firstLine string) (int, bool) {
	fields := strings.Fields(firstLine)
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0, false
	}
	n, err := strconv.Atoi(fields[1])
	return n, err == nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine`
Expected: PASS. Existing runner tests that assert `run --rm --no-deps --name 'sample-nightly-1'` must be updated to `run --rm --no-deps "$@" --name 'sample-nightly-1'` (TestScheduledJobUnitContract and TestPinnedScheduledJobLockProtocol).

- [ ] **Step 5: Commit**

```bash
git add internal/engine
git commit -m "feat(schedule): manual activations consume declared inputs; hosts need systemd 252 for them

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

### Task 10: `Engine.ScheduleRun` and the `schedule_run` operation

**Files:**
- Create: `internal/engine/schedule_run.go`
- Modify: `internal/onebox/operation_types.go:43,480` (`KindScheduleRun`), `internal/onebox/execution_types.go:288-310,357-373` (`ExecuteRequest.Job/Inputs/Wait`, `OperationResult.ScheduleRun`), `internal/onebox/execute.go:208` (dispatch), `internal/onebox/binding.go:34`, `internal/engine/audit.go:33-57` (`schedule-run` action and outcome)
- Test: `internal/engine/schedule_run_test.go`

**Interfaces:**
- Produces:
  - `type ScheduleRunResult struct { Job, Unit, Operation string; Inputs map[string]string; Started bool; Record *ScheduleRunRecord }` JSON `job, unit, operation, inputs, started, record`.
  - `func (e *Engine) ScheduleRun(ctx, operationID, job string, inputs map[string]string, wait bool) (ScheduleRunResult, error)`.
  - `KindScheduleRun OperationKind = "schedule_run"`; `ExecuteRequest.Job string`, `ExecuteRequest.Inputs map[string]string`, `ExecuteRequest.Wait bool`; `OperationResult.ScheduleRun *engine.ScheduleRunResult json:"schedule_run,omitempty"`.
  - Journal: phase `schedule-run`, `Target: job`, `Detail: "inputs: K=V,..."` (or `inputs: defaults`), events `start` then `finish` (`Status: ok`, `Detail: "unit started; outcome in ob schedule history"`), both before the lock is released; `auditAction("schedule-run") == "schedule run"`, `auditOutcome("schedule run") == "started"`.

- [ ] **Step 1: Write the failing test**

```go
package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestScheduleRunWritesInputsJournalsThenStartsAfterReleasingTheLock(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Inputs:   map[string]app.JobInput{"SOURCE": {Enum: []string{"catalog", "prices"}, Default: "catalog"}},
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "systemctl is-active"):
			return transport.Result{ExitCode: 3}, true
		case strings.Contains(cmd, "systemctl start"):
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	result, err := e.ScheduleRun(context.Background(), "20260905-151200-schedule_run-7c1e", "sync", map[string]string{"SOURCE": "prices"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Started || result.Unit != "ob-sample-sync" || result.Inputs["SOURCE"] != "prices" {
		t.Fatalf("result = %#v", result)
	}
	seq := strings.Join(f.Commands, "\n")
	inputs := strings.Index(seq, "sync.inputs")
	start := strings.Index(seq, "systemctl start --no-block 'ob-sample-sync.service'")
	release := strings.LastIndex(seq, "rm -f '/var/lib/ob/sample/lock'")
	if inputs < 0 || start < 0 || release < 0 || !(inputs < release && release < start) {
		t.Fatalf("expected inputs write, lock release, then start:\n%s", seq)
	}
	if !strings.Contains(seq, "set -C") {
		t.Fatalf("inputs file was not created with noclobber:\n%s", seq)
	}
	written := strings.Join(f.Inputs, "\n")
	for _, want := range []string{"ONEBOX_OPERATION=20260905-151200-schedule_run-7c1e", "SOURCE=prices"} {
		if !strings.Contains(written, want) {
			t.Fatalf("inputs file is missing %q:\n%s", want, written)
		}
	}
	if !strings.Contains(written, `"phase":"schedule-run"`) || !strings.Contains(written, `"target":"sync"`) {
		t.Fatalf("schedule run was not journaled:\n%s", written)
	}
}

func TestScheduleRunRefusals(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["sync"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Inputs:   map[string]app.JobInput{"SOURCE": {Enum: []string{"catalog"}, Default: "catalog"}},
		Schedule: &app.JobSchedule{Cron: "0 * * * *", Timezone: "UTC", Timeout: "1h"},
	}
	cfg.Workloads["prune"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "destructive",
		Schedule: &app.JobSchedule{Cron: "0 3 * * *", Timezone: "UTC", Timeout: "1h"},
	}
	active := false
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl is-active") {
			if active {
				return transport.Result{Stdout: "activating\n"}, true
			}
			return transport.Result{ExitCode: 3}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	ctx := context.Background()
	if _, err := e.ScheduleRun(ctx, "op", "prune", nil, false); err == nil || !strings.Contains(err.Error(), "ob job run") {
		t.Fatalf("destructive job accepted: %v", err)
	}
	if _, err := e.ScheduleRun(ctx, "op", "sync", map[string]string{"SOURCE": "reviews"}, false); err == nil {
		t.Fatal("undeclared value accepted")
	}
	if _, err := e.ScheduleRun(ctx, "op", "web", nil, false); err == nil {
		t.Fatal("non-scheduled workload accepted")
	}
	active = true
	if _, err := e.ScheduleRun(ctx, "op", "sync", nil, false); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("active unit not refused: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine -run 'TestScheduleRun'`
Expected: FAIL to compile.

- [ ] **Step 3: Implement `internal/engine/schedule_run.go`**

```go
package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
)

// ScheduleRunResult is what an operator-initiated run leaves on the
// workstation side. The outcome lives on the host: it is the run record.
type ScheduleRunResult struct {
	Job       string             `json:"job"`
	Unit      string             `json:"unit"`
	Operation string             `json:"operation"`
	Inputs    map[string]string  `json:"inputs,omitempty"`
	Started   bool               `json:"started"`
	Record    *ScheduleRunRecord `json:"record,omitempty"`
}

// ScheduleRun starts a scheduled job's unit now with declared, validated
// inputs. It is journaled like every operation, but the application lock is
// released before the unit starts: the runner exits 75 when it sees that lock.
func (e *Engine) ScheduleRun(ctx context.Context, operationID, name string, inputs map[string]string, wait bool) (ScheduleRunResult, error) {
	result := ScheduleRunResult{Job: name, Operation: operationID, Inputs: inputs}
	if strings.TrimSpace(operationID) == "" {
		return result, errors.New("schedule run requires an operation id")
	}
	if err := e.RequireHostOwner(ctx); err != nil {
		return result, err
	}
	job, err := e.scheduledJob(name)
	if err != nil {
		return result, err
	}
	workload := e.Spec.Workloads[name]
	if workload.DataEffect != app.DataEffectNone {
		return result, fmt.Errorf("job %s declares data_effect %q; operator-initiated runs of it go through the sealed plan: ob job plan %s, then ob job run", name, workload.DataEffect, name)
	}
	if err := app.ValidateJobInputValues(workload, inputs); err != nil {
		return result, err
	}
	unit := e.names().ScheduledJobUnit(name)
	result.Unit = unit

	active, err := e.T.Run(ctx, "systemctl is-active "+q(unit+".service")+" 2>/dev/null || true")
	if err != nil {
		return result, err
	}
	if state := strings.TrimSpace(active.Stdout); state == "active" || state == "activating" || state == "deactivating" {
		return result, fmt.Errorf("job %s is running (%s); wait for it or read ob schedule history %s", name, state, name)
	}

	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return result, err
	}
	locked := true
	defer func() {
		if locked {
			e.ReleaseLock(ctx)
		}
	}()
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return result, err
	}

	body := scheduleInputsFile(operationID, inputs)
	path := e.names().ScheduledJobRunInputs(name)
	create := "umask 077 && install -d -m 700 " + q(e.names().AppDir()+"/schedule") + " && set -C && cat > " + q(path)
	res, err := e.T.RunInput(ctx, create, body)
	if err != nil {
		return result, err
	}
	if res.ExitCode != 0 {
		return result, fmt.Errorf("a manual run of %s is already pending (%s exists); wait for it or remove the file on the host", name, path)
	}

	writer := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: operationID, Epoch: epoch, Operator: journal.DefaultOperator(),
		GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner,
	}
	detail := "inputs: defaults"
	if len(inputs) > 0 {
		detail = "inputs: " + scheduleInputsDetail(inputs)
	}
	record := journal.Record{Phase: "schedule-run", Event: "start", Status: "ok", Target: name, TargetKind: "job", Detail: detail}
	if err := writer.Append(ctx, record); err != nil {
		return result, fmt.Errorf("journal schedule run start: %w", err)
	}
	record.Event, record.Detail = "finish", "unit started; outcome in ob schedule history "+name
	if err := writer.Append(ctx, record); err != nil {
		return result, fmt.Errorf("journal schedule run finish: %w", err)
	}
	e.ReleaseLock(ctx)
	locked = false

	start := "systemctl start --no-block " + q(unit+".service")
	if wait {
		start = "systemctl start " + q(unit+".service")
	}
	res, err = e.mutate(ctx, start)
	if err != nil {
		return result, err
	}
	result.Started = true
	if res.ExitCode != 0 && !wait {
		return result, fmt.Errorf("systemctl start %s: %s", unit, strings.TrimSpace(res.Stderr))
	}
	if wait {
		records, err := e.ScheduleHistory(ctx, name, 1)
		if err != nil {
			return result, err
		}
		if len(records) > 0 {
			result.Record = &records[0]
		}
		if res.ExitCode != 0 && (result.Record == nil || result.Record.Outcome != "skipped") {
			return result, fmt.Errorf("job %s did not succeed; see ob schedule logs %s", name, name)
		}
	}
	_ = job
	return result, nil
}

func scheduleInputsFile(operationID string, inputs map[string]string) string {
	lines := []string{app.ReservedInputPrefix + "OPERATION=" + operationID}
	for _, name := range sortedInputNames(inputs) {
		lines = append(lines, name+"="+inputs[name])
	}
	return strings.Join(lines, "\n") + "\n"
}

func scheduleInputsDetail(inputs map[string]string) string {
	parts := make([]string, 0, len(inputs))
	for _, name := range sortedInputNames(inputs) {
		parts = append(parts, name+"="+inputs[name])
	}
	return strings.Join(parts, ",")
}

func sortedInputNames(inputs map[string]string) []string {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
```

`e.mutate` is fence-checked; if it refuses commands that are not file writes, use `e.T.Run` for the start. Check `internal/engine/lock.go:312` (`mutate`) first and use whichever the existing `systemctl enable --now` path uses (it uses `e.mutate`, so `mutate` is right; but after `ReleaseLock` the fence value is still ours, so it passes).

Wiring in `internal/onebox`:

```go
// operation_types.go
	KindScheduleRun   OperationKind = "schedule_run"
```

add `KindScheduleRun` to `validOperationKind` and `operationUsesInspectionRuntime`; add to `ExecuteRequest`:

```go
	// Job, Inputs and Wait are the schedule_run arguments: a declared
	// scheduled job, validated input overrides, and whether to block until
	// the unit exits.
	Job    string
	Inputs map[string]string
	Wait   bool
```

`OperationResult`: `ScheduleRun *engine.ScheduleRunResult json:"schedule_run,omitempty"`. `execute.go` dispatch:

```go
	case KindScheduleRun:
		result.EvidenceID = operationID
		var run engine.ScheduleRunResult
		run, err = e.ScheduleRun(ctx, operationID, request.Job, request.Inputs, request.Wait)
		result.ScheduleRun = &run
```

`audit.go`: `case "schedule-run": return "schedule run"` and `case "schedule run": return "started"`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine ./internal/onebox`
Expected: PASS. If an `internal/onebox` test enumerates every kind (search `validOperationKind` in tests), add `KindScheduleRun` there.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/schedule_run.go internal/engine/schedule_run_test.go internal/engine/audit.go internal/onebox
git commit -m "feat(schedule): operator-initiated runs with validated inputs, journaled as schedule_run

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

### Task 11: CLI `ob schedule run`

**Files:**
- Modify: `cmd/ob/schedule.go`, `cmd/ob/output.go` (matrix), `cmd/ob/output_test.go` (wantMatrix), `site/src/content/docs/reference/policies.mdx`
- Test: `cmd/ob/output_test.go`, `cmd/ob/schedule_test.go`

**Interfaces:**
- Consumes: `runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindScheduleRun, Job, Inputs, Wait, BreakLock}, "schedule run")`.
- Produces matrix row `"ob schedule run": {finite_stream, JSON, NDJSON}`; flag parsing `--input NAME=VALUE` (repeatable) into `map[string]string`, refusing a duplicate name or a missing `=` before any connection.

- [ ] **Step 1: Write the failing tests**

`cmd/ob/schedule_test.go`:

```go
package main

import "testing"

func TestParseScheduleInputsFlags(t *testing.T) {
	got, err := parseScheduleInputs([]string{"SOURCE=prices", "SINCE=2026-09-01"})
	if err != nil || got["SOURCE"] != "prices" || got["SINCE"] != "2026-09-01" {
		t.Fatalf("got %#v, %v", got, err)
	}
	for _, bad := range [][]string{{"SOURCE"}, {"=x"}, {"SOURCE=a", "SOURCE=b"}} {
		if _, err := parseScheduleInputs(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}
```

Add `"ob schedule run": {Class: "finite_stream", JSON: true, NDJSON: true}` to `wantMatrix`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/ob -run 'TestParseScheduleInputsFlags|TestLeafOutputMatrix'`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
func parseScheduleInputs(raw []string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range raw {
		name, value, ok := strings.Cut(item, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--input %q must be NAME=VALUE", item)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--input %s given twice", name)
		}
		out[name] = value
	}
	return out, nil
}
```

Command:

```go
	var runInputs []string
	var runWait, runBreakLock bool
	runCmd := &cobra.Command{
		Use:   "run <job>",
		Short: "start a scheduled job now with declared inputs",
		Long:  "Start one scheduled job's unit now, with values for its declared inputs. Values are validated on the workstation against the declaration; an undeclared name or a value outside its enum or pattern is refused before anything reaches the host.\n\nOnly a job with data_effect none may run this way; a migration or destructive job keeps the sealed plan of ob job run. The request is journaled as schedule_run with the operator and inputs. The outcome is the run record: ob schedule history <job>, or --wait to block until the unit exits and print it.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := parseScheduleInputs(runInputs)
			if err != nil {
				return writeEarlyOperationFailure(cmd, g, codedError("invalid_argument", "%v", err))
			}
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindScheduleRun, Job: args[0], Inputs: inputs, Wait: runWait, BreakLock: runBreakLock,
			}, "schedule run")
		},
	}
	runCmd.Flags().StringArrayVar(&runInputs, "input", nil, "input override as NAME=VALUE; repeatable")
	runCmd.Flags().BoolVar(&runWait, "wait", false, "block until the unit exits and print the run record")
	runCmd.Flags().BoolVar(&runBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	scheduleCmd.AddCommand(runCmd)
```

Check `codedError`'s signature at `cmd/ob/job.go:182` usage and match it. After a successful human-mode run, `runMutation` prints the operation outcome; the record is in `result.ScheduleRun` for structured output. For human output with `--wait`, print the record after `runMutation` returns nil: `result` is not returned by `runMutation`, so add an optional `onResult func(onebox.OperationResult)` hook only if `runMutation` already exposes one; otherwise leave the human hint `run: ob schedule history <job>` in the command's `Long` and rely on structured output for the record.

Add `` `ob schedule run` `` to the Finite operation stream row in `policies.mdx`.

- [ ] **Step 4: Regenerate docs and run tests**

Run: `just build && just docs-generate && go test ./... && just docs-generate-check && just env-namespace`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/ob site/src/content/docs/reference
git commit -m "feat(cli): ob schedule run starts a scheduled job now with validated inputs

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

## Docs and end-to-end

### Task 12: Guide, capabilities page, issue follow-up

**Files:**
- Modify: `site/src/content/docs/guides/schedule-a-job.mdx`, `site/src/content/docs/status/capabilities.mdx`
- Regenerate: `site/src/content/docs/reference/fields/workloads.mdx`, `site/src/content/docs/reference/cli.mdx`, `site/public/onebox.run-v1.schema.json`, `docs/onebox.run-v1.schema.json`

- [ ] **Step 1: Guide sections**

In `schedule-a-job.mdx`:
- Replace the sentence "A failed run is not retried implicitly; the next attempt is the next cron elapse." with a `## Retry inside one firing` section: the `retry` block, attempts counting, doubling capped by `max_backoff`, the timeout bound, what exclusive and pinned hold during backoff, and that the next cron elapse is still the retry for short cadences.
- Rewrite `## Failures remain visible` as `## Every run leaves a record`: the record fields, `ob schedule history <job>`, `ob schedule logs <job> [--run <id>]`, `ob schedule list`, the new `ob status` line, `skipped` runs, journal retention and the volatile-journal note, and `notify`.
- Add `## Run one now with inputs` before `## Running one by hand`: the `inputs` block, naming and value rules, `ob schedule run <job> --input NAME=VALUE --wait`, the `data_effect: none` rule with the pointer to `ob job run`, the `TRIGGER_UNIT` requirement (systemd 252, Ubuntu 24.04 and Debian 12), and that the request appears in `ob audit` joined to the record by operation id.
- Update the frontmatter `description`/`summary` to mention retry, run history and manual runs.

- [ ] **Step 2: Capabilities page**

Under `## Shipped`, add one bullet:

```markdown
- Scheduled jobs with bounded retry inside one firing, one run record per
  activation in the host journal (`ob schedule history`, `ob schedule logs`,
  `ob status`), per-outcome notifications, and declared inputs for
  operator-initiated runs (`ob schedule run`).
```

- [ ] **Step 3: Regenerate and verify**

Run: `just build && just docs-generate && go run ./cmd/ob schema --out docs/onebox.run-v1.schema.json && just check`
Expected: PASS (`just check` runs mod-tidy, fmt-check, vet, test, docs-generate-check, site-build; `site-build` needs `just site-install` once).

- [ ] **Step 4: Commit**

```bash
git add site docs
git commit -m "docs(site): retry, run history, notify and manual runs for scheduled jobs

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

- [ ] **Step 5: Issue follow-up**

Edit #155: `OB_` becomes `ONEBOX_` (reserved prefix and metadata line), the systemd floor check lives in `SyncSchedules`, the volatile journal is a status note rather than an issue, and `ob schedule run` journals start and finish before releasing the lock. Post one comment summarising the deviations.

### Task 13: End-to-end coverage on the Lima host

**Files:**
- Modify: `e2e/testdata/postgres/ob.yml.tmpl:37-51`, `e2e/server_test.go:188-292`

- [ ] **Step 1: Fixture jobs**

Add beside `timeout-chore`:

```yaml
  # Fails once, then succeeds: proves the in-firing retry and that the run
  # record counts attempts. The marker lives on the host so the second
  # attempt, a fresh container, can see the first one ran.
  retry-chore:
    role: job
    image: public.ecr.aws/docker/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
    command: ["sh", "-c", "if [ -f /marker/ran ]; then echo second; else touch /marker/ran; exit 1; fi"]
    data_effect: none
    volumes: [{source: /tmp/onebox-e2e-retry, path: /marker}]
    schedule: { cron: "0 0 1 1 *", timeout: 60s, catch_up: false, retry: {attempts: 2, backoff: 1s} }
  # A declared input reaches the container as an environment variable, both
  # as its default on a timer-shaped start and as an override on a manual run.
  input-chore:
    role: job
    image: public.ecr.aws/docker/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
    command: ["sh", "-c", "echo greeting=$GREETING"]
    data_effect: none
    inputs:
      GREETING: {enum: [hi, hello], default: hi}
    schedule: { cron: "0 0 1 1 *", timeout: 20s, catch_up: false }
```

- [ ] **Step 2: Assertions**

In the "scheduled jobs" subtest after the normal `chore` run:

```go
		history := s.mustOb(t, dir, "schedule", "history", "chore", "--output", "json")
		for _, want := range []string{`"outcome":"success"`, `"trigger":"manual"`, `"attempts":1`} {
			if !strings.Contains(history, want) {
				t.Fatalf("history is missing %q:\n%s", want, history)
			}
		}
		s.run(t, "rm -rf /tmp/onebox-e2e-retry && mkdir -p /tmp/onebox-e2e-retry")
		s.run(t, "systemctl start ob-observer-retry--chore.service")
		retry := s.mustOb(t, dir, "schedule", "history", "retry-chore", "--output", "json")
		for _, want := range []string{`"outcome":"success"`, `"attempts":2`} {
			if !strings.Contains(retry, want) {
				t.Fatalf("retry history is missing %q:\n%s", want, retry)
			}
		}
		manual := s.mustOb(t, dir, "schedule", "run", "input-chore", "--input", "GREETING=hello", "--wait", "--output", "json")
		if !strings.Contains(manual, `"GREETING":"hello"`) || !strings.Contains(manual, `"outcome":"success"`) {
			t.Fatalf("manual run record missing inputs or outcome:\n%s", manual)
		}
		logs := s.mustOb(t, dir, "schedule", "logs", "input-chore")
		if !strings.Contains(logs, "greeting=hello") {
			t.Fatalf("run logs do not show the override:\n%s", logs)
		}
		list := s.mustOb(t, dir, "schedule", "list")
		if !strings.Contains(list, "input-chore") || !strings.Contains(list, "active") {
			t.Fatalf("schedule list did not show the timer:\n%s", list)
		}
```

Add to the timeout section: `"last run failed: timeout"` stays; the record for `timeout-chore` shows `"outcome":"timeout"` via `ob schedule history timeout-chore --output json`.

Check the unit name escaping for `retry-chore` (`ob-observer-retry--chore`, the double hyphen is how `timeout-chore` is spelled at line 192).

- [ ] **Step 3: Run**

Run: `command -v limactl && just server-e2e` (boots Ubuntu 24.04, about a minute, then the suite). If Lima is not installed, record that the e2e suite was not run locally and rely on CI.

- [ ] **Step 4: Commit**

```bash
git add e2e
git commit -m "test(e2e): scheduled-job retry, run records, manual runs with inputs on a real host

Claude-Session: https://claude.ai/code/session_013MqwrF5NA179khDtEQdib5"
```

## Self-review notes

- Spec coverage: history (Tasks 1-5), retry and notify (6-7), inputs and manual runs (8-11), docs (12), host validation (9, 13). `ob doctor` is not touched: the journal note lives in `ob status`, and the systemd floor in `SyncSchedules`, both recorded as deviations in Task 12 Step 5.
- Type consistency: `ScheduleRunRecord`, `ScheduleListing`, `ScheduleRunResult`, `JobRetry`, `JobInput`, `KindScheduleRun`, `scheduleRunIdentifier`, `scheduleHistoryCommand`, `scheduleInputsLines`, `scheduleAttemptLoop`, `scheduleNotifier` are used with the same names throughout.
