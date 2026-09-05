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
	for _, want := range []string{"journalctl SYSLOG_IDENTIFIER=ob-run ONEBOX_UNIT='ob-sample-nightly'", "-o cat", "-r", "-n 20", "--no-pager"} {
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
		if strings.Contains(cmd, "SYSLOG_IDENTIFIER=ob-run") {
			return transport.Result{Stdout: sampleRunRecords}, true
		}
		return transport.Result{}, false
	}
	var out bytes.Buffer
	run, err := e.ScheduleLogs(context.Background(), "nightly", "", &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if run != "b2c3d4e5f60718293a4b5c6d7e8f9012" {
		t.Fatalf("resolved run id = %q; the caller must learn which run was streamed", run)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "journalctl _SYSTEMD_INVOCATION_ID=b2c3d4e5f60718293a4b5c6d7e8f9012") {
		t.Fatalf("logs did not target the newest run:\n%s", seq)
	}
	if _, err := e.ScheduleLogs(context.Background(), "nightly", "../etc", &out, &out); err == nil {
		t.Fatal("an invalid run id reached the shell")
	}
}

func TestScheduleLogsRefusesAJobWithoutRecords(t *testing.T) {
	e, f := scheduledFixture(t)
	var out bytes.Buffer
	_, err := e.ScheduleLogs(context.Background(), "nightly", "", &out, &out)
	if err == nil || !strings.Contains(err.Error(), "no recorded runs") {
		t.Fatalf("err = %v", err)
	}
	for _, command := range f.Commands {
		if strings.Contains(command, "_SYSTEMD_INVOCATION_ID") || strings.Contains(command, "journalctl -u") {
			t.Fatalf("logs guessed a source without a record: %s", command)
		}
	}
}
