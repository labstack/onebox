package engine

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

const walgListing = `[
 {"backup_name":"base_A","finish_time":"2026-08-20T10:00:00Z"},
 {"backup_name":"base_C","finish_time":"2026-08-20T14:00:00Z"},
 {"backup_name":"base_B","finish_time":"2026-08-20T12:00:00Z"}
]`

func baseSelectionEngine(listing string) (*Engine, *transport.Fake) {
	fake := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "backup-list") {
			return transport.Result{Stdout: listing}, true
		}
		return transport.Result{}, false
	}}
	spec := &app.Spec{
		Name:     "shop",
		BasePath: "/var/lib/ob",
		Services: map[string]app.Service{"database": {Driver: "postgres", Version: "18"}},
	}
	e := New(&app.Resolved{Spec: spec, Env: "production"}, nil, fake,
		Options{Out: io.Discard, Sleep: func(time.Duration) {}})
	return e, fake
}

// Replay only moves forward, so the base backup has to be one that finished at
// or before the requested point. Fetching wal-g's LATEST unconditionally made
// every point older than the newest base backup unrecoverable: PostgreSQL ran
// out of WAL before the target and died with "recovery ended before configured
// recovery target was reached". With a daily backup and a seven-day window that
// is six days of a window `ob backup status` reported as recoverable.
func TestTheBaseBackupIsChosenByTheRequestedPoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{"between two backups picks the earlier", "2026-08-20T13:00:00Z", "base_B"},
		{"after every backup picks the newest", "2026-08-20T20:00:00Z", "base_C"},
		{"exactly at a finish time picks it", "2026-08-20T12:00:00Z", "base_B"},
		{"just before a finish time picks the one before", "2026-08-20T11:59:59Z", "base_A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := baseSelectionEngine(walgListing)
			got, err := e.baseBackupFor(context.Background(), "c1", "database", tc.target)
			if err != nil {
				t.Fatalf("selecting a base backup: %v", err)
			}
			if got != tc.want {
				t.Fatalf("chose %q, want %q", got, tc.want)
			}
		})
	}
}

// A point older than everything in the repository is refused, and the refusal
// says what the repository can actually reach — the operator asking has a
// window in mind and needs to know where it really starts.
func TestAPointOlderThanTheRepositoryIsRefusedWithItsOldestBackup(t *testing.T) {
	e, _ := baseSelectionEngine(walgListing)
	_, err := e.baseBackupFor(context.Background(), "c1", "database", "2020-01-01T00:00:00Z")
	if err == nil {
		t.Fatal("a point older than every base backup was accepted")
	}
	if !strings.Contains(err.Error(), "2026-08-20T10:00:00Z") {
		t.Fatalf("refusal does not name the oldest recoverable base: %v", err)
	}
}

// An entry whose completion time cannot be read is refused, not defaulted: a
// zero time sorts before every real one, so defaulting would make the
// unreadable entry the answer to "what can reach this point".
func TestAnUnreadableCompletionTimeIsRefused(t *testing.T) {
	if _, err := parseWalgBackupList(`[{"backup_name":"base_A","finish_time":"whenever"}]`); err == nil {
		t.Fatal("an unreadable completion time was accepted")
	}
	entries, err := parseWalgBackupList("null")
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty listing = %v, %v", entries, err)
	}
}

// Without a requested point the recovery takes the newest backup, which is what
// "the newest recoverable point" means and needs no listing at all.
func TestNoRequestedPointStillUsesLatest(t *testing.T) {
	e, fake := baseSelectionEngine(walgListing)
	if _, err := e.fetchRecoveryBase(context.Background(), "c1", "database", ""); err != nil {
		t.Fatalf("fetching the newest base: %v", err)
	}
	for _, cmd := range fake.Commands {
		if strings.Contains(cmd, "backup-list") {
			t.Fatalf("listed the repository for a recovery that asked for no point: %s", cmd)
		}
	}
	if len(fake.Commands) == 0 || !strings.Contains(fake.Commands[len(fake.Commands)-1], "backup-fetch") {
		t.Fatalf("no backup-fetch was issued: %v", fake.Commands)
	}
	if !strings.Contains(fake.Commands[len(fake.Commands)-1], "LATEST") {
		t.Fatalf("did not fetch LATEST: %v", fake.Commands)
	}
}

// wal-g reports a broken WAL chain and exits 0. It prints the integrity table,
// says "integrity check status: WARNING", lists the ranges it could not find —
// and returns success, so a check that reads only the exit code reports green
// over a repository with holes in it. Two segments deleted from the middle of a
// live repository produced exactly that, and the scheduled unit had the same
// blind spot.
func TestABrokenWALChainIsNotAPassingVerification(t *testing.T) {
	report := `[wal-verify] integrity check status: FAILURE
[wal-verify] integrity check details:
+-----+--------------------------+--------------------------+----------------+-------------------+
| TLI | START                    | END                      | SEGMENTS COUNT |            STATUS |
+-----+--------------------------+--------------------------+----------------+-------------------+
|   3 | 000000030000000000000027 | 00000003000000000000002A |              4 |             FOUND |
|   3 | 00000003000000000000002B | 00000003000000000000002B |              1 |      MISSING_LOST |
|   3 | 00000003000000000000002C | 00000003000000000000002E |              3 |             FOUND |
+-----+--------------------------+--------------------------+----------------+-------------------+
[wal-verify] timeline check status: OK`
	err := walVerifyResult(report)
	if err == nil {
		t.Fatal("a gap in the middle of the chain passed verification")
	}
	if !strings.Contains(err.Error(), "00000003000000000000002B") {
		t.Fatalf("the failure does not name the missing range: %v", err)
	}
}

// A segment still uploading is not yet a hole, wherever it sits in the table.
// On a database taking backups in a loop, three segments were in flight while
// later ones had already arrived — so failing on in-flight ranges would fail
// every check that lands mid-upload.
func TestASegmentStillUploadingIsNotAGap(t *testing.T) {
	report := `[wal-verify] integrity check status: WARNING
| TLI | START                    | END                      | SEGMENTS COUNT |            STATUS |
|   3 | 000000030000000000000027 | 00000003000000000000002D |              7 |             FOUND |
|   3 | 00000003000000000000002E | 000000030000000000000030 |              3 | MISSING_UPLOADING |
|   3 | 000000030000000000000031 | 000000030000000000000031 |              1 |             FOUND |
[wal-verify] timeline check status: OK`
	if err := walVerifyResult(report); err != nil {
		t.Fatalf("segments in flight were treated as a gap: %v", err)
	}
}

// A lost segment is a hole wherever it sits.
func TestALostSegmentAtTheHeadIsAGap(t *testing.T) {
	report := `[wal-verify] integrity check status: FAILURE
| TLI | START                    | END                      | SEGMENTS COUNT |            STATUS |
|   3 | 000000030000000000000027 | 00000003000000000000002D |              7 |             FOUND |
|   3 | 00000003000000000000002E | 00000003000000000000002E |              1 |      MISSING_LOST |
[wal-verify] timeline check status: OK`
	if err := walVerifyResult(report); err == nil {
		t.Fatal("a lost segment at the head passed verification")
	}
}

// A healthy chain passes, and a report whose table this parser does not
// recognise still fails on the status line rather than reading as clean.
func TestAHealthyChainPassesAndAnUnreadableReportDoesNot(t *testing.T) {
	healthy := `[wal-verify] integrity check status: OK
| TLI | START                    | END                      | SEGMENTS COUNT | STATUS |
|   3 | 000000030000000000000027 | 00000003000000000000002E |              8 |  FOUND |
[wal-verify] timeline check status: OK`
	if err := walVerifyResult(healthy); err != nil {
		t.Fatalf("a healthy chain failed verification: %v", err)
	}
	if err := walVerifyResult("[wal-verify] timeline check status: FAILURE"); err == nil {
		t.Fatal("a failing check with no table read as clean")
	}
}

// The scheduled check has to reach the same verdict as the interactive one.
// wal-g exits 0 over a broken chain, so a unit that invoked it directly was
// marked successful by systemd for as long as nobody looked — which is the
// whole span a scheduled check exists to cover.
func TestTheScheduledVerificationJudgesTheReportRatherThanTheExitCode(t *testing.T) {
	script := backupVerifyScript("shop-database-1")
	for _, required := range []string{
		"wal-verify",
		"MISSING_(LOST|DELAYED)",
		"exit 1",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("the scheduled verification does not contain %q:\n%s", required, script)
		}
	}
	// A segment still uploading must not fail the unit, for the same reason it
	// does not fail the command: it resolves itself, and a check that flags it
	// flaps on every busy database.
	if strings.Contains(script, "MISSING_UPLOADING") {
		t.Fatalf("the scheduled verification fails on segments still in flight:\n%s", script)
	}
	// wal-g's own failure must still fail the unit.
	if !strings.Contains(script, `[ "$status" -eq 0 ] || exit "$status"`) {
		t.Fatalf("the scheduled verification swallows wal-g's own exit code:\n%s", script)
	}
}
