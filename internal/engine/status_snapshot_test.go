package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestStatusSnapshotCompleteAndJSONFriendly(t *testing.T) {
	f := statusFake("R2", "R2")
	var out bytes.Buffer
	now := time.Date(2026, 7, 12, 18, 30, 0, 0, time.FixedZone("test", -7*60*60))
	e := New(testConfig(), testProject(t), f, Options{
		Out:   &out,
		Sleep: noSleep,
		Now:   func() time.Time { return now },
	})

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snapshot.Complete || snapshot.Diverged {
		t.Fatalf("want complete, converged snapshot, got complete=%v diverged=%v warnings=%v", snapshot.Complete, snapshot.Diverged, snapshot.Warnings)
	}
	if snapshot.App != "sample" || snapshot.Host != "fake" || snapshot.RecordedRelease != "R2" {
		t.Fatalf("unexpected identity: %#v", snapshot)
	}
	if want := now.UTC(); !snapshot.CapturedAt.Equal(want) || snapshot.CapturedAt.Location() != time.UTC {
		t.Fatalf("captured_at = %v; want UTC %v", snapshot.CapturedAt, want)
	}
	if len(snapshot.Roles) != 2 || snapshot.Roles[0].Name != "web" || snapshot.Roles[1].Name != "worker" {
		t.Fatalf("roles must follow configured order: %#v", snapshot.Roles)
	}
	if got := snapshot.Roles[0]; got.Service != "web" || got.Mode != "rolling" || got.DesiredReplicas != 1 || len(got.Containers) != 1 || got.Containers[0].ID != "S1" {
		t.Fatalf("unexpected web status: %#v", got)
	}
	if len(snapshot.Services) != 1 || snapshot.Services[0].Name != "postgres" || snapshot.Services[0].Containers[0].ID != "PG1" {
		t.Fatalf("unexpected services: %#v", snapshot.Services)
	}
	if len(snapshot.Orphans) != 0 {
		t.Fatalf("converged snapshot must not contain orphans: %#v", snapshot.Orphans)
	}
	if snapshot.Incomplete != nil || snapshot.Proxy != nil {
		t.Fatalf("clean unmanaged app must omit incomplete/proxy: %#v", snapshot)
	}
	if out.Len() != 0 {
		t.Fatalf("machine snapshot must not render UI output: %q", out.String())
	}

	b, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"app", "host", "captured_at", "recorded_release", "roles", "services", "orphans", "schedules", "diverged", "complete"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("JSON missing %q: %s", key, b)
		}
	}
}

func TestStatusSnapshotAcceptsRetainedWorkloadFromEarlierRelease(t *testing.T) {
	f := statusFake("R2", "R2")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	webRevision := e.Compose.Services["web"].Labels[app.WorkloadRevisionLabel]
	workerRevision := e.Compose.Services["worker"].Labels[app.WorkloadRevisionLabel]
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "--format") && strings.Contains(cmd, "ob.app") {
			return transport.Result{Stdout: "S1|web|R2|" + webRevision + "|Up (healthy)\n" +
				"W1|worker|R1|" + workerRevision + "|Up (healthy)\n" +
				"PG1|postgres|R2||Up (healthy)\n"}, true
		}
		return base(cmd)
	}
	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Diverged || snapshot.Roles[1].Containers[0].Revision != workerRevision {
		t.Fatalf("retained workload reported as drift: %#v", snapshot.Roles[1])
	}
}

func TestStatusSnapshotReportsUndeclaredAppContainer(t *testing.T) {
	f := statusFake("R2", "R2")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "--format") && strings.Contains(cmd, "ob.app") {
			return transport.Result{Stdout: "S1|web|R2|Up (healthy)\n" +
				"W1|worker|R2|Up (healthy)\nPG1|postgres|R2|Up (healthy)\n" +
				"OLD2|frontend|R1|Up (healthy)\nOLD1|frontend|R1|Up (healthy)\n"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snapshot.Complete || !snapshot.Diverged {
		t.Fatalf("undeclared container must be observed divergence: %#v", snapshot)
	}
	if len(snapshot.Orphans) != 1 || snapshot.Orphans[0].Service != "frontend" {
		t.Fatalf("unexpected orphans: %#v", snapshot.Orphans)
	}
	containers := snapshot.Orphans[0].Containers
	if len(containers) != 2 || containers[0].ID != "OLD1" || containers[1].ID != "OLD2" || containers[0].Release != "R1" {
		t.Fatalf("orphan containers must be stable and complete: %#v", containers)
	}
}

func TestStatusSnapshotReportsObservedDivergenceAndIncompleteDeploy(t *testing.T) {
	journalOut := journalMarkerLine + "R3.jsonl\n" +
		`{"deploy_id":"R3","epoch":7,"phase":"deploy","event":"start","detail":"prev=R2","ts":"2026-07-12T18:00:00Z","operator":"v","git_sha":"abc123"}` + "\n" +
		`{"deploy_id":"R3","epoch":7,"phase":"transfer","event":"result","status":"ok","ts":"2026-07-12T18:01:00Z"}` + "\n" +
		`{"deploy_id":"R3","epoch":7,"phase":"release","role":"web","event":"result","status":"ok","ts":"2026-07-12T18:02:00Z"}` + "\n"
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/R2\n"}, true
		case strings.Contains(cmd, "--format") && strings.Contains(cmd, "ob.app"):
			// Deliberately reverse the web ids: the public result must be stable.
			return transport.Result{Stdout: "S2|web|R1|Up (healthy)\n" +
				"S1|web|R2|Up (unhealthy)\n" +
				"PG1|postgres|R2|Restarting (1) 2 seconds ago\n"}, true
		case strings.Contains(cmd, "for f in"):
			return transport.Result{Stdout: journalOut}, true
		}
		return transport.Result{}, false
	}}
	cfg := testConfig()
	web := cfg.Workloads["web"]
	web.Replicas = 2
	cfg.Workloads["web"] = web
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snapshot.Complete || !snapshot.Diverged {
		t.Fatalf("want complete divergent snapshot, got complete=%v diverged=%v warnings=%v", snapshot.Complete, snapshot.Diverged, snapshot.Warnings)
	}
	webStatus := snapshot.Roles[0]
	if !webStatus.Diverged || len(webStatus.Containers) != 2 || webStatus.Containers[0].ID != "S1" || webStatus.Containers[1].ID != "S2" {
		t.Fatalf("web divergence/ordering missing: %#v", webStatus)
	}
	if !strings.Contains(strings.Join(webStatus.Issues, "\n"), "health is unhealthy") || !strings.Contains(strings.Join(webStatus.Issues, "\n"), "runs release R1") {
		t.Fatalf("web issues do not explain divergence: %v", webStatus.Issues)
	}
	if worker := snapshot.Roles[1]; !worker.Diverged || !strings.Contains(strings.Join(worker.Issues, "\n"), "replica count") {
		t.Fatalf("missing worker must be explicit: %#v", worker)
	}
	if postgres := snapshot.Services[0]; !postgres.Diverged || !strings.Contains(strings.Join(postgres.Issues, "\n"), "is down") {
		t.Fatalf("down service must be explicit: %#v", postgres)
	}
	if snapshot.Incomplete == nil {
		t.Fatal("unfinished deployment must be present")
	}
	if got := snapshot.Incomplete; got.DeployID != "R3" || got.Epoch != 7 || got.PreviousRelease != "R2" || got.Operator != "v" || got.GitSHA != "abc123" {
		t.Fatalf("unexpected incomplete deployment: %#v", got)
	}
	if got := snapshot.Incomplete.Completed; len(got) != 2 || got[0] != "release:web" || got[1] != "transfer" {
		t.Fatalf("completed steps must be sorted: %v", got)
	}
}

func TestStatusSnapshotIncludesScheduledJobFailure(t *testing.T) {
	cfg := testConfig()
	cfg.Workloads["nightly"] = app.Workload{
		Role: app.RoleJob, When: "manual", DataEffect: "none",
		Schedule: &app.JobSchedule{Cron: "0 2 * * *", Timezone: "UTC", Timeout: "1h", CatchUp: true},
	}
	f := statusFake("R2", "R2")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "systemctl show") {
			return transport.Result{Stdout: `@@nightly:service
LoadState=loaded
ActiveState=failed
Result=exit-code
ExecMainStatus=9
@@nightly:timer
LoadState=loaded
ActiveState=active
`}, true
		}
		return base(cmd)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Complete || !snapshot.Diverged || len(snapshot.Schedules) != 1 {
		t.Fatalf("scheduled failure was not included as observed divergence: %#v", snapshot)
	}
	if got := snapshot.Schedules[0]; !got.Diverged || got.LastResult != "exit-code" || got.LastExitStatus != 9 {
		t.Fatalf("unexpected scheduled-job status: %#v", got)
	}
}

func TestStatusSnapshotReadFailuresAreDeterministicWarnings(t *testing.T) {
	f := statusFake("R2", "R2")
	f.Err = func(cmd string) error {
		switch {
		case strings.Contains(cmd, "readlink"):
			return errors.New("release read failed")
		case strings.Contains(cmd, "for f in"):
			return errors.New("journal read failed")
		default:
			return nil
		}
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ordinary read failures must return partial data, got %v", err)
	}
	if snapshot.Complete {
		t.Fatal("failed reads must make the snapshot incomplete")
	}
	if snapshot.Diverged {
		t.Fatalf("unknown release/journal state must not be invented as divergence: %#v", snapshot)
	}
	if len(snapshot.Warnings) != 2 || snapshot.Warnings[0].Component != "current_release" || snapshot.Warnings[1].Component != "incomplete_deployment" {
		t.Fatalf("warnings must use stable component order: %#v", snapshot.Warnings)
	}
	if len(snapshot.Roles[0].Containers) != 1 {
		t.Fatalf("successful container observation must survive other read failures: %#v", snapshot.Roles)
	}
	commands := strings.Join(f.Commands, "\n")
	for _, want := range []string{"readlink", "docker ps", "for f in"} {
		if !strings.Contains(commands, want) {
			t.Fatalf("best-effort wave skipped %q after another failure:\n%s", want, commands)
		}
	}
}

func TestStatusSnapshotTreatsDockerCommandFailureAsPartial(t *testing.T) {
	f := statusFake("R2", "R2")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps") && strings.Contains(cmd, "--format") {
			return transport.Result{ExitCode: 1, Stderr: "daemon unavailable"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("command failure should produce a partial snapshot: %v", err)
	}
	if snapshot.Complete || snapshot.Diverged {
		t.Fatalf("unobserved containers are incomplete, not invented drift: %#v", snapshot)
	}
	if len(snapshot.Warnings) != 1 || snapshot.Warnings[0].Component != "containers" || !strings.Contains(snapshot.Warnings[0].Message, "daemon unavailable") {
		t.Fatalf("container read warning missing: %#v", snapshot.Warnings)
	}
}

func TestStatusSnapshotDoesNotHideReleaseOrJournalCommandFailures(t *testing.T) {
	f := statusFake("R2", "R2")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{ExitCode: 1, Stderr: "release permission denied"}, true
		case strings.Contains(cmd, "for f in"):
			return transport.Result{ExitCode: 1, Stderr: "journal permission denied"}, true
		default:
			return base(cmd)
		}
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("read command failures should produce partial status: %v", err)
	}
	if snapshot.Complete || snapshot.Diverged {
		t.Fatalf("unknown release/journal facts must be incomplete, not invented divergence: %#v", snapshot)
	}
	if len(snapshot.Warnings) != 2 || snapshot.Warnings[0].Component != "current_release" || snapshot.Warnings[1].Component != "incomplete_deployment" {
		t.Fatalf("expected deterministic component warnings: %#v", snapshot.Warnings)
	}
}

func TestStatusSnapshotDoesNotHideProxyReadCommandFailures(t *testing.T) {
	applied := ""
	e, f, _, _ := statusProxyEngine(t, &applied, "", "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "config.hash") {
			return transport.Result{ExitCode: 1, Stderr: "permission denied"}, true
		}
		return base(cmd)
	}

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Complete || snapshot.Proxy == nil || snapshot.Proxy.Complete {
		t.Fatalf("proxy read failure must be explicit partial state: %#v", snapshot)
	}
	if len(snapshot.Warnings) != 1 || snapshot.Warnings[0].Component != "proxy.applied_config" {
		t.Fatalf("proxy warning missing: %#v", snapshot.Warnings)
	}
}

// The JSON path must not assert drift about a file it never read: config_diverged
// is what an agent branches on, and an empty applied hash compared against a real
// local one manufactures divergence out of a failed read.
func TestStatusSnapshotDoesNotInventDriftFromARefusedHashRead(t *testing.T) {
	applied := ""
	e, f, _, _ := statusProxyEngine(t, &applied, "", "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "config.hash") {
			return transport.Result{ExitCode: app.ProbeUndetermined}, true
		}
		return base(cmd)
	}

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Proxy == nil {
		t.Fatal("proxy section was dropped entirely")
	}
	if snapshot.Proxy.ConfigDiverged {
		t.Errorf("drift asserted from an unread applied hash: %#v", snapshot.Proxy)
	}
	for _, issue := range snapshot.Proxy.Issues {
		if strings.Contains(issue, "hashes differ") {
			t.Errorf("hash-mismatch issue raised for an unread file: %#v", snapshot.Proxy.Issues)
		}
	}
	if snapshot.Proxy.AppliedConfigHash != "" {
		t.Errorf("applied hash published from a refused read: %q", snapshot.Proxy.AppliedConfigHash)
	}
}

// Pins what a refused certificate read reports. Note this passes with or
// without the acmeIssue gate in makeStatusProxy: CertExpiries returns no certs
// and no error for empty input, so the gate is defensive rather than
// load-bearing. The behaviour below is the contract regardless of which line
// currently produces it.
func TestStatusSnapshotDoesNotPublishCertificatesFromARefusedStore(t *testing.T) {
	applied := ""
	acme := acmeFixture(t, "app.example.com", time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC))
	e, f, _, _ := statusProxyEngine(t, &applied, acme, "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "acme.json") {
			return transport.Result{ExitCode: app.ProbeUndetermined}, true
		}
		return base(cmd)
	}

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Proxy == nil {
		t.Fatal("proxy section was dropped entirely")
	}
	if len(snapshot.Proxy.Certificates) != 0 {
		t.Errorf("certificates published from a refused store: %#v", snapshot.Proxy.Certificates)
	}
	if snapshot.Proxy.Complete {
		t.Errorf("proxy reported complete despite a refused certificate read: %#v", snapshot.Proxy)
	}
	var named bool
	for _, issue := range snapshot.Proxy.Issues {
		if strings.Contains(issue, "cannot be searched") {
			named = true
		}
	}
	if !named {
		t.Errorf("refusal was not named among the issues: %#v", snapshot.Proxy.Issues)
	}
}

// readHostOwner refuses any record that is not a valid application name, so
// status publishing one would be the "two answers about one file" the shared
// probe exists to eliminate: the JSON reads as correctly claimed while every
// mutation refuses the host.
func TestStatusSnapshotDoesNotPublishAnOwnerEveryMutationRefuses(t *testing.T) {
	applied := ""
	e, f, _, _ := statusProxyEngine(t, &applied, "", "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "_host/owner") {
			return transport.Result{Stdout: "Prod App\n"}, true
		}
		return base(cmd)
	}

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Proxy == nil {
		t.Fatal("proxy section was dropped entirely")
	}
	if snapshot.Proxy.Owner != "" {
		t.Errorf("status published owner %q, which every mutation refuses", snapshot.Proxy.Owner)
	}
	if snapshot.Proxy.Complete {
		t.Errorf("proxy reported complete with an unusable owner record: %#v", snapshot.Proxy)
	}
	var named bool
	for _, issue := range snapshot.Proxy.Issues {
		if strings.Contains(issue, "not a valid application name") {
			named = true
		}
	}
	if !named {
		t.Errorf("the invalid owner was not named among the issues: %#v", snapshot.Proxy.Issues)
	}
}

// The probe can exit outside its enumerated set — cat fails when the record is
// unlinked or replaced between the -r test and the read. Raising that would
// stop gather and render nothing, costing the operator every other fact about
// the host over one file.
func TestStatusSnapshotSurvivesAnUnenumeratedOwnerExit(t *testing.T) {
	applied := ""
	e, f, _, _ := statusProxyEngine(t, &applied, "", "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "_host/owner") {
			return transport.Result{ExitCode: 1, Stderr: "input/output error"}, true
		}
		return base(cmd)
	}

	// Status, not StatusSnapshot: the snapshot degrades per component, while
	// gather stops the human path at the first error and renders nothing.
	// A divergence error is expected and fine — it comes after rendering. A
	// read error is the abort, and it comes instead of rendering.
	if err := e.Status(context.Background()); err != nil && strings.Contains(err.Error(), "read host owner") {
		t.Fatalf("an unenumerated owner exit aborted the whole report: %v", err)
	}

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Proxy == nil || snapshot.Proxy.Container == nil {
		t.Fatalf("container health was lost to the owner read: %#v", snapshot.Proxy)
	}
	if snapshot.Proxy.Complete {
		t.Errorf("proxy reported complete despite an unreadable owner: %#v", snapshot.Proxy)
	}
}

// A refused file read is reported, not raised: status is what an operator runs
// to find out what is wrong, and one unsearchable directory must not take the
// container health, the owner and the rest of the report with it. The snapshot
// still refuses to call the proxy complete.
func TestStatusSnapshotReportsRefusedProxyReadsWithoutLosingTheRest(t *testing.T) {
	applied := ""
	e, f, _, _ := statusProxyEngine(t, &applied, "", "healthy")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "config.hash") {
			return transport.Result{ExitCode: app.ProbeUndetermined}, true
		}
		return base(cmd)
	}

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("a refused read must not abort the snapshot: %v", err)
	}
	if snapshot.Proxy == nil {
		t.Fatal("proxy section was dropped entirely")
	}
	if snapshot.Proxy.Complete {
		t.Errorf("proxy reported complete despite a refused read: %#v", snapshot.Proxy)
	}
	if snapshot.Proxy.Container == nil {
		t.Errorf("container health was lost to an unrelated read: %#v", snapshot.Proxy)
	}
	var named bool
	for _, issue := range snapshot.Proxy.Issues {
		if strings.Contains(issue, "cannot be searched") {
			named = true
		}
	}
	if !named {
		t.Errorf("refusal was not named among the issues: %#v", snapshot.Proxy.Issues)
	}
}

func TestStatusSnapshotManagedProxy(t *testing.T) {
	applied := ""
	acme := acmeFixture(t, "app.example.com", time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC))
	e, _, out, localHash := statusProxyEngine(t, &applied, acme, "healthy")

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snapshot.Complete || !snapshot.Diverged || snapshot.Proxy == nil {
		t.Fatalf("overdue cert should be a complete, divergent proxy observation: %#v", snapshot)
	}
	px := snapshot.Proxy
	if !px.Complete || !px.Managed || px.Container == nil || px.Container.ID != "PX1" || px.Container.Health != "healthy" {
		t.Fatalf("unexpected proxy container: %#v", px)
	}
	if px.LocalConfigHash != localHash || px.AppliedConfigHash != localHash || px.ConfigDiverged {
		t.Fatalf("unexpected proxy hashes: %#v", px)
	}
	if px.Owner != "sample" {
		t.Fatalf("unexpected host owner: %q", px.Owner)
	}
	if len(px.Certificates) != 1 || px.Certificates[0].Domain != "app.example.com" || px.Certificates[0].DaysRemaining != 10 || !px.Certificates[0].RenewalOverdue {
		t.Fatalf("unexpected certificates: %#v", px.Certificates)
	}
	if out.Len() != 0 {
		t.Fatalf("snapshot must not render proxy UI: %q", out.String())
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), acme) {
		t.Fatal("raw ACME store must never appear in snapshot JSON")
	}
}

func TestStatusSnapshotMalformedProxyStoreIsPartial(t *testing.T) {
	applied := ""
	e, _, _, _ := statusProxyEngine(t, &applied, "{not-json", "healthy")

	snapshot, err := e.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatalf("malformed cert store is a partial observation, not a hard error: %v", err)
	}
	if snapshot.Complete || !snapshot.Diverged || snapshot.Proxy == nil || snapshot.Proxy.Complete {
		t.Fatalf("malformed cert store must be incomplete and divergent: %#v", snapshot)
	}
	if len(snapshot.Warnings) != 1 || snapshot.Warnings[0].Component != "proxy.certificates" {
		t.Fatalf("certificate parse warning missing: %#v", snapshot.Warnings)
	}
}

func TestStatusSnapshotCancellationIsHardError(t *testing.T) {
	f := statusFake("R2", "R2")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.StatusSnapshot(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context cancellation, got %v", err)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("pre-cancelled snapshot must not issue reads: %v", f.Commands)
	}
}
