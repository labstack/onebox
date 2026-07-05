package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func statusFake(webRelease, recorded string) *transport.Fake {
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: "releases/" + recorded + "\n"}, true
		case strings.Contains(cmd, "service='server'"):
			return transport.Result{Stdout: "S1\n"}, true
		case strings.Contains(cmd, "service='worker'"):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(cmd, "service='postgres'"):
			return transport.Result{Stdout: "PG1\n"}, true
		case strings.Contains(cmd, "ob.release") && strings.Contains(cmd, "S1"):
			return transport.Result{Stdout: webRelease + "\n"}, true
		case strings.Contains(cmd, "ob.release") && strings.Contains(cmd, "W1"):
			return transport.Result{Stdout: recorded + "\n"}, true
		case strings.Contains(cmd, "Health"):
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "ls -1"): // no journals
			return transport.Result{Stdout: ""}, true
		}
		return transport.Result{}, false
	}
	return f
}

func TestStatusInSync(t *testing.T) {
	f := statusFake("R2", "R2")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Status(context.Background()); err != nil {
		t.Fatalf("in-sync status must not error: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "all in sync") {
		t.Fatalf("expected in-sync:\n%s", out.String())
	}
}

func TestStatusFlagsDivergence(t *testing.T) {
	f := statusFake("R1", "R2") // web still runs old release
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	err := e.Status(context.Background())
	if err == nil {
		t.Fatalf("divergence must be an error:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "DIVERGED") {
		t.Fatalf("expected DIVERGED marker:\n%s", out.String())
	}
}
