package engine

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/transport"
)

func fakeEngine(t *testing.T, f *transport.Fake) *Engine {
	t.Helper()
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}})
	return e
}

func TestPreflightHappyPath(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`docker version`), Result: transport.Result{Stdout: "27.0.3\n"}},
		{Match: regexp.MustCompile(`docker compose version`), Result: transport.Result{Stdout: "2.29.1\n"}},
		{Match: regexp.MustCompile(`df -Pk`), Result: transport.Result{Stdout: "4194304\n"}}, // 4 GiB in KiB
		{Match: regexp.MustCompile(`docker ps .*postgres`), Result: transport.Result{Stdout: "abc123\n"}},
		{Match: regexp.MustCompile(`docker inspect .*abc123`), Result: transport.Result{Stdout: "healthy\n"}},
	}}
	if err := fakeEngine(t, f).Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v\ncommands:\n%s", err, strings.Join(f.Commands, "\n"))
	}
}

func TestPreflightFailsOnStoppedAccessory(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`docker version`), Result: transport.Result{Stdout: "27.0.3\n"}},
		{Match: regexp.MustCompile(`docker compose version`), Result: transport.Result{Stdout: "2.29.1\n"}},
		{Match: regexp.MustCompile(`df -Pk`), Result: transport.Result{Stdout: "4194304\n"}},
		{Match: regexp.MustCompile(`docker ps .*postgres`), Result: transport.Result{Stdout: "\n"}},
	}}
	err := fakeEngine(t, f).Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("want accessory-down error, got %v", err)
	}
}

func TestContainerIDsRejectsSuspiciousOutput(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`docker ps -q`), Result: transport.Result{Stdout: "abc123; rm -rf /\n"}},
	}}
	e := fakeEngine(t, f)
	if _, err := e.containerIDs(context.Background(), "server"); err == nil {
		t.Fatal("suspicious container id must be rejected")
	}
}
