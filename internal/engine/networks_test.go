package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestApplicationNetworkIsCreatedWithOwnership(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "network inspect") && strings.Contains(command, "sample_default") {
			return transport.Result{ExitCode: 1, Stderr: "Error response from daemon: network sample_default not found"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.EnsureApplicationNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(f.Commands, "\n")
	if !strings.Contains(commands, "docker network create --label 'ob.app=sample' 'sample_default'") {
		t.Fatalf("application network was not created with ownership:\n%s", commands)
	}
}

func TestApplicationNetworkDoesNotTreatInspectFailureAsAbsence(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "network inspect") && strings.Contains(command, "sample_default") {
			return transport.Result{ExitCode: 1, Stderr: "permission denied"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.EnsureApplicationNetwork(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot inspect ownership") {
		t.Fatalf("inspect failure error = %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "network create") {
		t.Fatalf("inspect failure was treated as absence:\n%s", strings.Join(f.Commands, "\n"))
	}
}

func TestApplicationNetworkRefusesForeignOwner(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "network inspect") && strings.Contains(command, "sample_default") {
			return transport.Result{Stdout: "abc123|other-app|other-app\n"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.EnsureApplicationNetwork(context.Background())
	if err == nil || !strings.Contains(err.Error(), "owned by application other-app") {
		t.Fatalf("foreign network error = %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "network update") {
		t.Fatal("a foreign network must never be relabelled")
	}
}

func TestLegacyComposeNetworkIsAcceptedByIdentity(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "network inspect") && strings.Contains(command, "sample_default") {
			return transport.Result{Stdout: "abc123||sample\n"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.EnsureApplicationNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(f.Commands, "\n")
	if strings.Contains(commands, "network create") {
		t.Fatalf("legacy application network was replaced:\n%s", commands)
	}
}

func TestLegacyServiceNetworkRequiresServiceStateBeforeAcceptance(t *testing.T) {
	for _, tt := range []struct {
		name      string
		stateExit int
		wantErr   bool
	}{
		{name: "legacy state", stateExit: 0},
		{name: "no state", stateExit: 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := happyFake()
			base := f.Dynamic
			f.Dynamic = func(command string) (transport.Result, bool) {
				if strings.Contains(command, "network inspect") && strings.Contains(command, "ob_sample") {
					return transport.Result{Stdout: "def456||\n"}, true
				}
				if strings.Contains(command, "test -d '/var/lib/ob/sample/services'") {
					return transport.Result{ExitCode: tt.stateExit}, true
				}
				return base(command)
			}
			e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
			err := e.EnsureServiceConnections(context.Background())
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
					t.Fatalf("missing legacy state error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.Join(f.Commands, "\n"), "network create") {
				t.Fatalf("legacy service network was replaced:\n%s", strings.Join(f.Commands, "\n"))
			}
		})
	}
}

func TestRemoveOwnedNetworksRefusesAttachedEndpoints(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "docker network rm 'sample_default'") {
			return transport.Result{ExitCode: 1, Stderr: "network has active endpoints"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.removeOwnedNetworks(context.Background())
	if err == nil || !strings.Contains(err.Error(), "detach its remaining endpoints") {
		t.Fatalf("attached endpoint error = %v", err)
	}
	commands := strings.Join(f.Commands, "\n")
	if strings.Contains(commands, "docker network rm 'ob_sample'") {
		t.Fatalf("teardown continued after the application network could not be removed:\n%s", commands)
	}
}

func TestRemoveOwnedNetworksIgnoresServiceNameWithoutServiceState(t *testing.T) {
	cfg := testConfig()
	cfg.Services = nil
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "test -d '/var/lib/ob/sample/services'") {
			return transport.Result{ExitCode: 1}, true
		}
		if strings.Contains(command, "network inspect") && strings.Contains(command, "ob_sample") {
			return transport.Result{Stdout: "def456||\n"}, true
		}
		return base(command)
	}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.removeOwnedNetworks(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(f.Commands, "\n")
	if strings.Contains(commands, "network inspect") && strings.Contains(commands, "ob_sample") {
		t.Fatalf("destroy inspected an undeclared service-network name:\n%s", commands)
	}
	if strings.Contains(commands, "network rm 'ob_sample'") {
		t.Fatalf("destroy removed an undeclared service-network name:\n%s", commands)
	}
}
