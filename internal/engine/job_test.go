package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func manualJobEngine(t *testing.T, target *transport.Fake) *Engine {
	t.Helper()
	config := testConfig()
	job := config.Workloads["migrate"]
	job.When = "manual"
	job.DataEffect = "none"
	config.Workloads["migrate"] = job
	return New(config, testProject(t), target, Options{
		Out: &bytes.Buffer{}, Sleep: noSleep,
		ApprovalDigest: "approval-digest", ApprovalClass: "one_time",
		ApprovedBy: "operator@example.test", ApprovalSource: "local_cli",
	})
}

func currentJobFake(runtime string) *transport.Fake {
	target := happyFake()
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + engineTestPreviousReleaseID + "\n"}, true
		case strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml"):
			return transport.Result{Stdout: runtime}, true
		default:
			return base(command)
		}
	}
	return target
}

func TestRunJobRejectsDeclarationDriftBeforeLock(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*Engine)
		request JobRunRequest
		want    string
	}{
		{
			name:    "unknown job",
			request: JobRunRequest{OperationID: "op-1", Job: "missing", ExpectedDataEffect: "none"},
			want:    "unknown job",
		},
		{
			name: "not manual",
			mutate: func(engine *Engine) {
				job := engine.Spec.Workloads["migrate"]
				job.When = "pre_release"
				engine.Spec.Workloads["migrate"] = job
			},
			request: JobRunRequest{OperationID: "op-2", Job: "migrate", ExpectedDataEffect: "none"},
			want:    "not a manual job",
		},
		{
			name:    "data effect changed",
			request: JobRunRequest{OperationID: "op-3", Job: "migrate", ExpectedDataEffect: "migration"},
			want:    "data effect changed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := happyFake()
			engine := manualJobEngine(t, target)
			if test.mutate != nil {
				test.mutate(engine)
			}
			_, _, err := engine.RunJobWithJournalID(context.Background(), test.request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if strings.Contains(strings.Join(target.Commands, "\n"), "set -C; echo") {
				t.Fatalf("declaration refusal acquired the app lock: %#v", target.Commands)
			}
		})
	}
}

func TestRunJobRechecksReleaseAndRuntimeUnderLock(t *testing.T) {
	const runtime = "services:\n  migrate:\n    image: ghcr.io/x/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"
	for _, test := range []struct {
		name    string
		request JobRunRequest
		want    string
	}{
		{
			name: "release changed",
			request: JobRunRequest{
				OperationID: "op-stale-release", Job: "migrate", ExpectedRelease: engineTestDeployReleaseID,
				ExpectedRuntimeDigest: HashBytes([]byte(runtime)), ExpectedDataEffect: "none",
			},
			want: "current release changed",
		},
		{
			name: "runtime changed",
			request: JobRunRequest{
				OperationID: "op-stale-runtime", Job: "migrate", ExpectedRelease: engineTestPreviousReleaseID,
				ExpectedRuntimeDigest: HashBytes([]byte("different runtime")), ExpectedDataEffect: "none",
			},
			want: "runtime changed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := currentJobFake(runtime)
			engine := manualJobEngine(t, target)
			_, _, err := engine.RunJobWithJournalID(context.Background(), test.request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			commands := strings.Join(target.Commands, "\n")
			if !strings.Contains(commands, "set -C; echo") || !strings.Contains(commands, "/fence") {
				t.Fatalf("post-lock recheck was not lock/fence protected:\n%s", commands)
			}
			if strings.Contains(commands, "ONEBOX_RESULT_FILE") {
				t.Fatalf("stale job created a container:\n%s", commands)
			}
		})
	}
}

func TestRunJobUsesPlanIdentityAndJournalsAuthorization(t *testing.T) {
	const runtime = "services:\n  migrate:\n    image: ghcr.io/x/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"
	target := currentJobFake(runtime)
	engine := manualJobEngine(t, target)
	request := JobRunRequest{
		OperationID: "op-job-run", Job: "migrate", ExpectedRelease: engineTestPreviousReleaseID,
		ExpectedRuntimeDigest: HashBytes([]byte(runtime)), ExpectedDataEffect: "none",
	}
	operationID, evidence, err := engine.RunJobWithJournalID(context.Background(), request)
	if err != nil {
		t.Fatalf("run job: %v\n%s", err, strings.Join(target.Commands, "\n"))
	}
	if operationID != request.OperationID || evidence != nil {
		t.Fatalf("job identity/evidence = %q/%+v", operationID, evidence)
	}
	commands := strings.Join(target.Commands, "\n")
	if strings.Count(commands, "ONEBOX_RESULT_FILE=/run/onebox/job-result") != 1 {
		t.Fatalf("job execution count differs from one:\n%s", commands)
	}
	for _, want := range []string{`"deploy_id":"op-job-run"`, `"phase":"job","event":"start"`, `"approval_digest":"approval-digest"`} {
		if !strings.Contains(commands, want) {
			t.Fatalf("job journal omitted %q:\n%s", want, commands)
		}
	}
}
