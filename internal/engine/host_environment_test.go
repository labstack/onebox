package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
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
		{"sample production", hostOwner{Application: "sample", Environment: "production"}, true},
		{"sample", hostOwner{}, false},
		{"  sample   production  ", hostOwner{Application: "sample", Environment: "production"}, true},
		{"", hostOwner{}, false},
		{"sample production extra", hostOwner{}, false},
		{"Sample production", hostOwner{}, false},
		{"sample Production", hostOwner{}, false},
	} {
		got, ok := app.ParseHostOwnerRecord(tc.record)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("ParseHostOwnerRecord(%q) = %+v,%v want %+v,%v", tc.record, got, ok, tc.want, tc.ok)
		}
		if ok && got.String() != strings.Join(strings.Fields(tc.record), " ") {
			t.Fatalf("String() = %q, does not round-trip %q", got.String(), tc.record)
		}
	}
}

// The engine derives every host path from Opts.Environment. cmd/ob's connect()
// once omitted it, which meant `ob status`, `ob audit` and `ob logs` read the
// project's default base_path instead of the environment's. An engine built
// without one now takes the environment the project was resolved for.
func TestEnvironmentSelectsTheBasePath(t *testing.T) {
	spec, err := app.LoadBytes([]byte(`apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: sample
spec:
  basePath: /var/lib/onebox
  environments:
    production: {server: root@h}
    staging: {server: root@h2, basePath: /srv/staging}
  workloads:
    web: {role: Application, image: 'x:1'}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("staging")
	if err != nil {
		t.Fatal(err)
	}
	staging := New(resolved, nil, nil, Options{Out: io.Discard, Environment: "staging"}).names().AppDir()
	if staging != "/srv/staging/app" {
		t.Fatalf("staging AppDir = %q, want /srv/staging/app", staging)
	}
	empty := New(resolved, nil, nil, Options{Out: io.Discard}).names().AppDir()
	if empty != staging {
		t.Fatalf("an engine with no environment resolved %q, not the resolved environment's %q", empty, staging)
	}
}
