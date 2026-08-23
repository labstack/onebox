package onebox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/transport"
)

const execProjectYAML = `api_version: onebox.run/v1
app: shop
environments:
  production:
    server: deploy@example.invalid
workloads:
  api:
    image: nginx
    port: 3000
    domain: shop.example.com
`

func execService(t *testing.T, connect Connector) *Service {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ob.yml")
	if err := os.WriteFile(path, []byte(execProjectYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return New(Options{
		ConfigPath: path,
		Now: func() time.Time {
			return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
		},
		Entropy: bytes.NewReader(bytes.Repeat([]byte{0x4a}, 64)),
		Connect: connect,
	})
}

func TestExecRefusesInvalidReasonBeforeConnecting(t *testing.T) {
	connected := false
	service := execService(t, func(context.Context, transport.Route) (transport.Transport, error) {
		connected = true
		return nil, errors.New("must not connect")
	})
	result, err := service.Exec(context.Background(), ExecRequest{Target: "api", Command: "true"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || connected {
		t.Fatalf("invalid reason reached target: result=%+v err=%v connected=%v", result, err, connected)
	}
	if result.Outcome != OperationStatusError || result.CommandDigest != "" {
		t.Fatalf("invalid-reason result = %+v", result)
	}
}

func TestExecRefusesIncompleteRequestBeforeConnecting(t *testing.T) {
	for _, request := range []ExecRequest{
		{Command: "true", Reason: "inspect a stuck request"},
		{Target: "api", Reason: "inspect a stuck request"},
	} {
		connected := false
		service := execService(t, func(context.Context, transport.Route) (transport.Transport, error) {
			connected = true
			return nil, errors.New("must not connect")
		})
		if _, err := service.Exec(context.Background(), request, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || connected {
			t.Fatalf("incomplete exec request reached target: request=%+v err=%v", request, err)
		}
	}
}

func TestExecEnforcesEnvironmentAndRunnerPolicyBeforeConnecting(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*Service)
		want    string
	}{
		{name: "unknown environment", prepare: func(service *Service) { service.environment = "staging" }, want: "unknown_environment"},
		{name: "runner policy", prepare: func(service *Service) {
			service.configPath = writeExecProject(t, strings.Replace(execProjectYAML,
				"    server: deploy@example.invalid\n",
				"    server: deploy@example.invalid\n    policy: {min_onebox_version: v2026.8.3}\n", 1))
		}, want: "not a released Onebox CalVer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			connected := false
			service := execService(t, func(context.Context, transport.Route) (transport.Transport, error) {
				connected = true
				return nil, errors.New("must not connect")
			})
			test.prepare(service)
			_, err := service.Exec(context.Background(), ExecRequest{
				Target: "api", Command: "true", Reason: "inspect a stuck request",
			}, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.want) || connected {
				t.Fatalf("policy refusal = %v, connected=%v", err, connected)
			}
		})
	}
}

func writeExecProject(t *testing.T, project string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ob.yml")
	if err := os.WriteFile(path, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecLocksFencesAndJournalsTheExactContainer(t *testing.T) {
	fake := &transport.Fake{
		TargetName: "deploy@example.invalid",
		Dynamic: func(command string) (transport.Result, bool) {
			switch {
			case strings.Contains(command, "_host/owner"):
				return transport.Result{Stdout: "shop\n"}, true
			case strings.Contains(command, "docker ps -q"):
				return transport.Result{Stdout: "bbbbbbbbbbbb\naaaaaaaaaaaa\n"}, true
			case strings.Contains(command, "docker exec aaaaaaaaaaaa "):
				return transport.Result{Stdout: "done\n"}, true
			default:
				return transport.Result{}, false
			}
		},
	}
	service := execService(t, func(context.Context, transport.Route) (transport.Transport, error) { return fake, nil })
	var stdout bytes.Buffer
	const command = "printf secret-output"
	result, err := service.Exec(context.Background(), ExecRequest{
		Target: "api", Command: command, Reason: "inspect a stuck request",
	}, &stdout, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OperationStatusSuccess || result.ContainerID != "aaaaaaaaaaaa" || result.CommandDigest != engine.HashBytes([]byte(command)) {
		t.Fatalf("exec result = %+v", result)
	}
	if stdout.String() != "done\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	commands := strings.Join(fake.Commands, "\n")
	for _, required := range []string{"/lock", "/fence", "then docker exec aaaaaaaaaaaa"} {
		if !strings.Contains(commands, required) {
			t.Fatalf("exec omitted %s:\n%s", required, commands)
		}
	}
	var journalCommands []string
	for _, candidate := range fake.Commands {
		if strings.Contains(candidate, "/journal/") {
			journalCommands = append(journalCommands, candidate)
		}
	}
	journal := strings.Join(journalCommands, "\n")
	if !strings.Contains(journal, `"container_id":"aaaaaaaaaaaa"`) || !strings.Contains(journal, result.CommandDigest) {
		t.Fatalf("journal omitted exact container or digest: %s", journal)
	}
	if strings.Contains(journal, command) || strings.Contains(journal, "secret-output") {
		t.Fatalf("journal leaked command bytes: %s", journal)
	}
}

func TestExecClassifiesCancellation(t *testing.T) {
	fake := &transport.Fake{
		TargetName: "deploy@example.invalid",
		Dynamic: func(command string) (transport.Result, bool) {
			switch {
			case strings.Contains(command, "_host/owner"):
				return transport.Result{Stdout: "shop\n"}, true
			case strings.Contains(command, "docker ps -q"):
				return transport.Result{Stdout: "aaaaaaaaaaaa\n"}, true
			default:
				return transport.Result{}, false
			}
		},
		Err: func(command string) error {
			if strings.Contains(command, "docker exec aaaaaaaaaaaa ") {
				return context.Canceled
			}
			return nil
		},
	}
	service := execService(t, func(context.Context, transport.Route) (transport.Transport, error) { return fake, nil })
	result, err := service.Exec(context.Background(), ExecRequest{
		Target: "api", Command: "true", Reason: "stop a hung diagnostic",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) || result.Outcome != OperationStatusCancelled {
		t.Fatalf("cancel result=%+v err=%v", result, err)
	}
}
