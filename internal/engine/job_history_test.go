package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestJobHistoryMergesTimerAndOperatorStoresByOperation(t *testing.T) {
	e, f := scheduledFixture(t)
	hostRecords := `{"run":"11111111111111111111111111111111","job":"nightly","trigger":"operator","operation":"op-host","release":"release-a","started_at":"2026-09-16T03:00:00Z","finished_at":"2026-09-16T03:00:04Z","duration_s":4,"attempts":1,"exit_status":0,"outcome":"success","inputs":{"SOURCE":"prices"}}
{"run":"22222222222222222222222222222222","job":"nightly","trigger":"timer","release":"release-a","started_at":"2026-09-16T02:00:00Z","finished_at":"2026-09-16T02:00:03Z","duration_s":3,"attempts":1,"exit_status":1,"outcome":"failure","inputs":{}}
`
	journals := `@@onebox-journal@@op-direct.jsonl
{"deploy_id":"op-direct","phase":"job","event":"start","status":"ok","ts":"2026-09-16T04:00:00Z","operator":"bob@example","service":"nightly","release_id":"release-a"}
{"deploy_id":"op-direct","phase":"job","event":"finish","status":"ok","ts":"2026-09-16T04:00:05Z","service":"nightly"}
@@onebox-journal@@op-host.jsonl
{"deploy_id":"op-host","phase":"schedule-run","event":"start","status":"ok","ts":"2026-09-16T02:59:59Z","operator":"alice@example","target":"nightly"}
{"deploy_id":"op-host","phase":"schedule-run","event":"finish","status":"ok","ts":"2026-09-16T03:00:04Z","target":"nightly"}
`
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "journalctl"):
			return transport.Result{Stdout: hostRecords}, true
		case strings.Contains(cmd, "@@onebox-journal@@"):
			return transport.Result{Stdout: journals}, true
		default:
			return transport.Result{}, false
		}
	}

	records, err := e.JobHistory(context.Background(), "nightly", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].ID != "op-direct" || records[0].Trigger != "operator" || records[0].Outcome != "success" || records[0].DurationSeconds != 5 {
		t.Fatalf("direct operator record = %#v", records[0])
	}
	if records[1].ID != "11111111111111111111111111111111" || records[1].Operator != "alice@example" || records[1].Inputs["SOURCE"] != "prices" {
		t.Fatalf("host-supervised operator record was not enriched in place: %#v", records[1])
	}
	if records[2].ID != "22222222222222222222222222222222" || records[2].Trigger != "timer" || records[2].Outcome != "failure" {
		t.Fatalf("timer record = %#v", records[2])
	}
}
