package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// ownerFake answers the host-owner probe with record and nothing else.
func ownerFake(record string) *transport.Fake {
	f := happyFake()
	inner := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "/_host/owner") {
			if record == "" {
				return transport.Result{ExitCode: 3}, true
			}
			return transport.Result{Stdout: record + "\n"}, true
		}
		return inner(cmd)
	}
	return f
}

func engineForEnv(t *testing.T, env string, f *transport.Fake) *Engine {
	t.Helper()
	return New(testConfig(), testProject(t), f, Options{Out: io.Discard, Sleep: noSleep, Environment: env})
}

// The bug this fixes: every runtime name an application derives is scoped to the
// application and not the environment, so a second environment pointed at the
// same host adopts the first one's containers and volumes rather than colliding
// with them. The owner record is the only place that difference is visible.
func TestRequireHostOwnerRefusesAnotherEnvironmentOfTheSameApplication(t *testing.T) {
	e := engineForEnv(t, "staging", ownerFake("sample production"))
	err := e.RequireHostOwner(context.Background())
	var mismatch *HostEnvironmentMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("staging against a production-claimed host = %v, want HostEnvironmentMismatchError", err)
	}
	if mismatch.Owner != "production" || mismatch.Requesting != "staging" {
		t.Fatalf("mismatch = %+v, want owner=production requesting=staging", mismatch)
	}
	if mismatch.Code() != "host_environment_mismatch" {
		t.Fatalf("code = %q", mismatch.Code())
	}
}

func TestRequireHostOwnerAcceptsItsOwnEnvironment(t *testing.T) {
	e := engineForEnv(t, "production", ownerFake("sample production"))
	if err := e.RequireHostOwner(context.Background()); err != nil {
		t.Fatalf("production against a production-claimed host: %v", err)
	}
}

// A record written before the environment field existed identifies the
// application and nothing more. Refusing on it would strand every host claimed
// by an older ob, so the application check still applies and the environment
// check waits for bootstrap to complete the record.
func TestRequireHostOwnerAcceptsARecordThatPredatesEnvironments(t *testing.T) {
	for _, env := range []string{"production", "staging"} {
		e := engineForEnv(t, env, ownerFake("sample"))
		if err := e.RequireHostOwner(context.Background()); err != nil {
			t.Fatalf("legacy record with env %q: %v", env, err)
		}
	}
}

// A different application is still refused with the code it always used; the
// new check must not swallow the older one.
func TestRequireHostOwnerStillRefusesADifferentApplication(t *testing.T) {
	e := engineForEnv(t, "production", ownerFake("other production"))
	err := e.RequireHostOwner(context.Background())
	var mismatch *HostOwnerMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("foreign application = %v, want HostOwnerMismatchError", err)
	}
}

func TestHostOwnerRecordRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		record string
		want   hostOwner
		ok     bool
	}{
		{"sample production", hostOwner{App: "sample", Environment: "production"}, true},
		{"sample", hostOwner{App: "sample"}, true},
		{"  sample   production  ", hostOwner{App: "sample", Environment: "production"}, true},
		{"", hostOwner{}, false},
		{"sample production extra", hostOwner{}, false},
		{"Sample production", hostOwner{}, false},
		{"sample Production", hostOwner{}, false},
	} {
		got, ok := parseHostOwner(tc.record)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("parseHostOwner(%q) = %+v,%v want %+v,%v", tc.record, got, ok, tc.want, tc.ok)
		}
		if ok && got.record() != strings.Join(strings.Fields(tc.record), " ") {
			t.Fatalf("record() = %q, does not round-trip %q", got.record(), tc.record)
		}
	}
}
