package onebox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func protectedRuntimeProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ob.yml")
	body := `api_version: onebox.run/v1
app: example
environments: {production: {server: root@host}}
workloads: {web: {image: nginx:1}}
services: {database: {driver: postgres, version: 17}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func protectedRuntimeState(t *testing.T, image string) string {
	t.Helper()
	initial, err := NewBackupLifecycleState("example", "production", "database", 1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := EnableBackup(initial, backupStateProjection(), image, "postgres:18", "enable-op", true, 2)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestProductionLoadInjectsLifecycleStateBeforeServiceRendering(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	image := "postgres@" + digest
	state := protectedRuntimeState(t, image)
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "database.lifecycle.json"):
			return transport.Result{Stdout: "present\n" + state}, true
		case strings.HasPrefix(command, "docker manifest inspect "):
			return transport.Result{ExitCode: 1}, true
		case strings.HasPrefix(command, "docker image inspect "):
			return transport.Result{Stdout: `["docker.io/library/postgres@` + digest + `"]`}, true
		default:
			return transport.Result{}, false
		}
	}}
	service := New(Options{
		ConfigPath: protectedRuntimeProject(t), Environment: "production",
		Connect: func(context.Context, string) (transport.Transport, error) { return fake, nil },
	})
	lp, err := service.loadProject(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := lp.resolved.ServiceImageForRuntime("database")
	if err != nil {
		t.Fatal(err)
	}
	if selection.Image != image || selection.Origin != app.OriginObserved {
		t.Fatalf("service image selection = %#v", selection)
	}
	services, err := lp.resolved.RenderServices("production")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(services["database"]), image) || strings.Contains(string(services["database"]), "postgres:17") {
		t.Fatalf("protected service did not render durable digest:\n%s", services["database"])
	}
}

func TestProductionLoadRefusesProtectedImageAbsentFromRegistryAndCache(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	image := "postgres@" + digest
	state := protectedRuntimeState(t, image)
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "database.lifecycle.json"):
			return transport.Result{Stdout: "present\n" + state}, true
		case strings.HasPrefix(command, "docker manifest inspect "), strings.HasPrefix(command, "docker image inspect "):
			return transport.Result{ExitCode: 1}, true
		default:
			return transport.Result{}, false
		}
	}}
	service := New(Options{
		ConfigPath: protectedRuntimeProject(t), Environment: "production",
		Connect: func(context.Context, string) (transport.Transport, error) { return fake, nil },
	})
	if _, err := service.loadProject(context.Background(), false); err == nil || !strings.Contains(err.Error(), "service_image_digest_unavailable") {
		t.Fatalf("unavailable protected image error = %v", err)
	}
}
