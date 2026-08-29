package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func extensionConfig() *app.Resolved {
	cfg := testConfig()
	service := cfg.Services["postgres"]
	service.Features = &app.ServiceFeatures{Extensions: map[string]app.ServiceExtension{
		"pg_trgm": {},
		"vector":  {},
	}}
	cfg.Services["postgres"] = service
	return cfg
}

func TestApplyInstallsDeclaredExtensionsBeforeSuccess(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "pg_available_extensions") && strings.Contains(cmd, "pg_trgm"):
			return transport.Result{Stdout: "1.6\n"}, true
		case strings.Contains(cmd, "pg_available_extensions") && strings.Contains(cmd, "vector"):
			return transport.Result{Stdout: "0.8.6\n"}, true
		case strings.Contains(cmd, "pg_extension"):
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(extensionConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	if err := e.ApplyServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(f.Commands, "\n")
	for _, extension := range []string{"pg_trgm", "vector"} {
		if !strings.Contains(commands, `CREATE EXTENSION "`+extension+`"`) {
			t.Errorf("extension %s was not installed:\n%s", extension, commands)
		}
	}
}

func TestApplyRefusesAnUnavailableExtension(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "pg_available_extensions") {
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(extensionConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	err := e.ApplyServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not available in the selected PostgreSQL image") {
		t.Fatalf("unavailable extension error = %v", err)
	}
}

func TestApplyRefusesAnInstalledExtensionVersionMismatch(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker inspect --format '{{.State.Running}}'"):
			return transport.Result{Stdout: "true\n"}, true
		case strings.Contains(cmd, "docker run --rm --entrypoint sh") && strings.Contains(cmd, "vector.control"):
			return transport.Result{Stdout: "0.8.6\n"}, true
		case strings.Contains(cmd, "docker run --rm --entrypoint sh") && strings.Contains(cmd, "pg_trgm.control"):
			return transport.Result{Stdout: "1.6\n"}, true
		case strings.Contains(cmd, "pg_available_extensions") && strings.Contains(cmd, "vector"):
			return transport.Result{Stdout: "0.8.6\n"}, true
		case strings.Contains(cmd, "pg_extension") && strings.Contains(cmd, "vector"):
			return transport.Result{Stdout: "0.7.4\n"}, true
		case strings.Contains(cmd, "pg_available_extensions") && strings.Contains(cmd, "pg_trgm"):
			return transport.Result{Stdout: "1.6\n"}, true
		case strings.Contains(cmd, "pg_extension"):
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(extensionConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	err := e.ApplyServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing restart before an explicit extension upgrade") {
		t.Fatalf("version mismatch error = %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "docker compose") {
		t.Fatal("service was mutated before candidate extension compatibility was refused")
	}
}

func TestApplyRefusesToUnloadAnInstalledPreloadExtension(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker inspect --format '{{.State.Running}}'"):
			return transport.Result{Stdout: "true\n"}, true
		case strings.Contains(cmd, "pg_extension") && strings.Contains(cmd, "pgaudit"):
			return transport.Result{Stdout: "18.0\n"}, true
		case strings.Contains(cmd, "pg_extension"):
			return transport.Result{}, true
		}
		return base(cmd)
	}
	e := New(extensionConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	err := e.ApplyServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "installed extension pgaudit requires preload") {
		t.Fatalf("preload removal error = %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "docker compose") {
		t.Fatal("service was mutated before preload removal was refused")
	}
}

func TestDeployReconcilesExtensionsBeforeMigrationJobs(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "pg_available_extensions") && strings.Contains(cmd, "pg_trgm"):
			return transport.Result{Stdout: "1.6\n"}, true
		case strings.Contains(cmd, "pg_available_extensions") && strings.Contains(cmd, "vector"):
			return transport.Result{Stdout: "0.8.6\n"}, true
		case strings.Contains(cmd, "pg_extension"):
			return transport.Result{}, true
		}
		return base(cmd)
	}
	seedStagedApplicationManifest(f, engineTestDeployReleaseID)
	e := New(extensionConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	if err := e.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(f.Commands, "\n")
	created := strings.Index(commands, `CREATE EXTENSION "vector"`)
	migration := strings.Index(commands, "run --rm --no-deps")
	if created < 0 || migration < 0 || created > migration {
		t.Fatalf("extension was not reconciled before migration:\n%s", commands)
	}
}
