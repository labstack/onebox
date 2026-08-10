package journal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"

	"github.com/labstack/onebox/internal/app"
)

func TestAppendCommandShape(t *testing.T) {
	f := &transport.Fake{}
	w := &Writer{T: f, Names: app.Names{App: "sample", BasePath: app.DefaultBasePath}, DeployID: "R1", Epoch: 3, GitSHA: "abc1234", ConfigHash: "sha256:x"}
	if err := w.Append(context.Background(), Record{Phase: "release", Role: "web", Event: "result", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Commands) != 1 {
		t.Fatalf("commands: %v", f.Commands)
	}
	cmd := f.Commands[0]
	for _, want := range []string{
		"mkdir -p '/var/lib/ob/sample/journal'",
		">> '/var/lib/ob/sample/journal/R1.jsonl'",
		"sync '/var/lib/ob/sample/journal/R1.jsonl'",
		`"deploy_id":"R1"`,
		`"epoch":3`,
		`"role":"web"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("append cmd missing %q:\n%s", want, cmd)
		}
	}
	// the JSON line must be a single shell-quoted argument to printf
	if !strings.Contains(cmd, "printf '%s\\n' '") {
		t.Fatalf("expected quoted printf: %s", cmd)
	}
}

func TestAppendRedactsFailureDetails(t *testing.T) {
	f := &transport.Fake{}
	w := &Writer{T: f, Names: app.Names{App: "sample", BasePath: app.DefaultBasePath}, DeployID: "R1", Epoch: 1}
	if err := w.Append(context.Background(), Record{
		Phase: "verify", Event: "result", Status: "fail",
		Detail: "request failed: Authorization=Bearer super-secret-token",
	}); err != nil {
		t.Fatal(err)
	}
	command := strings.Join(f.Commands, "\n")
	if strings.Contains(command, "super-secret-token") {
		t.Fatalf("journal command leaked failure detail: %s", command)
	}
	for _, want := range []string{
		`"detail":"operation failed; inspect trusted local diagnostics"`,
		`"error_code":"execution_failed"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("journal command missing %s: %s", want, command)
		}
	}
}

func TestAppendScopesAuthorizationContextToEvidenceRecords(t *testing.T) {
	f := &transport.Fake{}
	w := &Writer{
		T: f, Names: app.Names{App: "sample", BasePath: app.DefaultBasePath}, DeployID: "R1", Epoch: 1,
		ApprovalDigest: "sha256:approval", ApprovedBy: "operator@example",
		MigrationBackup: &MigrationBackupEvidence{
			Mode: "override", OverrideReason: "incident INC-42", ProtectedResources: []string{"database/postgres"},
		},
	}
	if err := w.Append(context.Background(), Record{Phase: "deploy", Event: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(context.Background(), Record{Phase: "release", Role: "web", Event: "result", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Commands) != 2 {
		t.Fatalf("commands = %d", len(f.Commands))
	}
	for _, want := range []string{"sha256:approval", "operator@example", "incident INC-42"} {
		if !strings.Contains(f.Commands[0], want) {
			t.Fatalf("deploy start omitted %q: %s", want, f.Commands[0])
		}
		if strings.Contains(f.Commands[1], want) {
			t.Fatalf("release record repeated %q: %s", want, f.Commands[1])
		}
	}
}

func TestReadAndSummary(t *testing.T) {
	recs := []Record{
		{DeployID: "R2", Epoch: 4, Phase: "deploy", Event: "start", Detail: "prev=R1"},
		{DeployID: "R2", Epoch: 4, Phase: "transfer", Event: "result", Status: "ok"},
		{DeployID: "R2", Epoch: 4, Phase: "pre-release", SubStep: "job:migrate", Event: "result", Status: "ok", Detail: "changed=false"},
		{DeployID: "R2", Epoch: 4, Phase: "release", Role: "web", Event: "result", Status: "ok"},
		{DeployID: "R2", Epoch: 4, Phase: "release", Role: "worker", Event: "intent"},
	}
	var lines []string
	for _, r := range recs {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "cat ") {
			return transport.Result{Stdout: strings.Join(lines, "\n") + "\ngarbage-line\n"}, true
		}
		if strings.Contains(cmd, "ls -1") {
			return transport.Result{Stdout: "R1.jsonl\nR2.jsonl\n"}, true
		}
		return transport.Result{}, false
	}}
	got, err := Read(context.Background(), f, app.Names{App: "sample", BasePath: app.DefaultBasePath}, "R2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 { // garbage tolerated, not fatal
		t.Fatalf("records: %d", len(got))
	}
	s := Summarize(got)
	if !s.Started || s.Finished || s.Aborted {
		t.Fatalf("summary flags: %+v", s)
	}
	if s.PrevRelease != "R1" {
		t.Fatalf("prev: %q", s.PrevRelease)
	}
	if !s.GateOpen {
		t.Fatal("migrate reported changed=false — gate must be open")
	}
	if !s.Done["transfer"] || !s.Done["job:migrate"] || !s.Done["release:web"] || s.Done["release:worker"] {
		t.Fatalf("done: %+v", s.Done)
	}
	ids, err := List(context.Background(), f, app.Names{App: "sample", BasePath: app.DefaultBasePath})
	if err != nil || len(ids) != 2 || ids[1] != "R2" {
		t.Fatalf("list: %v %v", ids, err)
	}
}

func TestSummarizeReconstructsAggregateGate(t *testing.T) {
	tests := []struct {
		name    string
		recs    []Record
		open    bool
		covered bool
	}{
		{
			name: "explicit data-effect-none result",
			recs: []Record{
				{SubStep: "job:index", Event: "intent"},
				{SubStep: "job:index", Event: "result", Status: "ok", RollbackSafe: true},
			},
			open: true, covered: true,
		},
		{
			name: "key-value changed-false result",
			recs: []Record{
				{SubStep: "job:migrate", Event: "result", Status: "ok", Detail: "changed=false"},
			},
			open: true, covered: true,
		},
		{
			name: "one unsafe completed job closes aggregate",
			recs: []Record{
				{SubStep: "job:index", Event: "result", Status: "ok", RollbackSafe: true},
				{SubStep: "job:migrate", Event: "result", Status: "ok", Detail: "changed=unknown"},
			},
		},
		{
			name: "interrupted attempt closes aggregate",
			recs: []Record{
				{SubStep: "job:index", Event: "result", Status: "ok", RollbackSafe: true},
				{SubStep: "job:migrate", Event: "intent"},
			},
		},
		{
			name: "later safe retry cannot erase unsafe attempt",
			recs: []Record{
				{SubStep: "job:migrate", Event: "intent"},
				{SubStep: "job:migrate", Event: "result", Status: "fail"},
				{SubStep: "job:migrate", Event: "intent"},
				{SubStep: "job:migrate", Event: "result", Status: "ok", RollbackSafe: true},
			},
		},
		{
			name: "safe retry cannot erase interrupted attempt",
			recs: []Record{
				{Epoch: 1, SubStep: "job:migrate", Event: "intent"},
				{Epoch: 2, SubStep: "job:migrate", Event: "intent"},
				{Epoch: 2, SubStep: "job:migrate", Event: "result", Status: "ok", RollbackSafe: true},
			},
		},
		{
			name: "interrupted migration covered by original expand-only policy",
			recs: []Record{
				{Epoch: 1, SubStep: "job:migrate", Event: "intent", RollbackPolicySafe: true},
			},
			covered: true,
		},
		{
			name: "pre-effect baseline is explicitly safe",
			recs: []Record{
				{SubStep: EffectBaselineSubStep, Event: "result", Status: "ok", RollbackSafe: true},
			},
			open: true, covered: true,
		},
		{
			name: "untyped lifecycle hook is not covered",
			recs: []Record{
				{SubStep: "hook:pre_release", Event: "intent"},
				{SubStep: "hook:pre_release", Event: "result", Status: "ok"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Summarize(tt.recs)
			if s.GateOpen != tt.open {
				t.Fatalf("GateOpen=%v, want %v: %+v", s.GateOpen, tt.open, s)
			}
			if s.RollbackCovered != tt.covered {
				t.Fatalf("RollbackCovered=%v, want %v: %+v", s.RollbackCovered, tt.covered, s)
			}
			if !s.Done[DoneGateRecorded] {
				t.Fatalf("gate history must be marked for resume: %+v", s.Done)
			}
		})
	}
}

func TestSummarizeDeploySuccessSurvivesMaintenanceFailure(t *testing.T) {
	s := Summarize([]Record{
		{Phase: "deploy", Event: "start"},
		{Phase: "deploy", Event: "finish", Status: "ok"},
		{Phase: "secrets-push", Event: "start"},
		{Phase: "secrets-push", Event: "finish", Status: "fail"},
	})
	if !s.DeploySucceeded {
		t.Fatal("later maintenance must not erase the compatible deploy checkpoint")
	}
}

func TestSummarizeSuccessfulAutoRollbackClosesRecovery(t *testing.T) {
	summary := Summarize([]Record{
		{DeployID: "20260101-000000-failed", Phase: "deploy", Event: "start"},
		{DeployID: "20260101-000000-failed", Phase: "auto-rollback", Event: "intent"},
		{DeployID: "20260101-000000-failed", Phase: "auto-rollback", Event: "result", Status: "ok"},
		{DeployID: "20260101-000000-failed", Phase: "deploy", Event: "finish", Status: "fail"},
	})
	if !summary.Recovered || summary.Finished {
		t.Fatalf("auto-rollback terminal summary = %+v", summary)
	}
}

func TestSummarizeFailedTerminalAttemptsRemainRecoverable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		record Record
	}{
		{name: "deploy finish", record: Record{Phase: "deploy", Event: "finish", Status: "fail"}},
		{name: "abort", record: Record{Phase: "abort", Event: "abort", Status: "fail"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := Summarize([]Record{{Phase: "deploy", Event: "start"}, tt.record})
			if s.Finished || s.Aborted {
				t.Fatalf("failed terminal attempt closed recovery: %+v", s)
			}
			if !s.Failed {
				t.Fatalf("failed attempt evidence was lost: %+v", s)
			}
		})
	}
}

func TestSummarizeMaintenanceRecordsAreNotDeployTerminals(t *testing.T) {
	s := Summarize([]Record{
		{Phase: "service-apply", Event: "start"},
		{Phase: "service-apply", Event: "finish", Status: "ok"},
	})
	if s.Started || s.Finished || s.DeploySucceeded {
		t.Fatalf("maintenance journal was reduced as a deployment: %+v", s)
	}
}
