package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestApplyServicesPassesStopTimeout(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	if err := e.ApplyServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(f.Commands, "\n")
	if !strings.Contains(commands, "up -d --remove-orphans --timeout 120") {
		t.Fatalf("service apply has no stop timeout:\n%s", commands)
	}
}

func TestRemoveServicesStopsGracefullyBeforeRemoval(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
			if strings.Contains(cmd, "docker ps -aq") {
				return transport.Result{Stdout: "SVC1\n"}, true
			}
			if strings.Contains(cmd, "docker rm -f SVC1") {
				return transport.Result{ExitCode: exitCode, Stderr: "removal failed"}, true
			}
			return transport.Result{}, true
		}}
		e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
		err := e.removeServices(context.Background(), false)
		if (err != nil) != (exitCode != 0) {
			t.Fatalf("removal exit %d: %v", exitCode, err)
		}
		commands := strings.Join(f.Commands, "\n")
		if !strings.Contains(commands, "docker stop -t 120 SVC1 >/dev/null 2>&1; docker rm -f SVC1") {
			t.Fatalf("service was not stopped before removal:\n%s", commands)
		}
	}
}
