package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/transport"
)

// The application default network outlives a release. An unmanaged proxy may
// still be attached when the release Compose document is taken down, and a
// hand-created network at the derived name must never be adopted silently.
func TestApplicationNetworkOwnershipAndExternalLifecycle(t *testing.T) {
	gate(t)
	ctx := context.Background()
	application := fmt.Sprintf("obnet%d", os.Getpid())
	network := application + "_default"

	projectBody := fmt.Sprintf(`api_version: onebox.run/v1
app: %s
environments:
  production: {server: root@localhost}
workloads:
  web: {image: alpine:3}
`, application)
	project, err := app.LoadBytes([]byte(projectBody), filepath.Join(t.TempDir(), "ob.yml"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := project.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(resolved, nil, transport.NewLocal(), engine.Options{Environment: "production", Out: &bytes.Buffer{}})

	if out, err := exec.Command("docker", "network", "create", network).CombinedOutput(); err != nil {
		t.Fatalf("create foreign network: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	if err := e.EnsureApplicationNetwork(ctx); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("foreign network was not refused: %v", err)
	}
	if out, err := exec.Command("docker", "network", "rm", network).CombinedOutput(); err != nil {
		t.Fatalf("remove foreign network: %v\n%s", err, out)
	}
	if err := e.EnsureApplicationNetwork(ctx); err != nil {
		t.Fatalf("create owned application network: %v", err)
	}
	owner, err := exec.Command("docker", "network", "inspect", "-f", `{{index .Labels "ob.app"}}`, network).Output()
	if err != nil || strings.TrimSpace(string(owner)) != application {
		t.Fatalf("new network owner = %q, %v", owner, err)
	}
	if out, err := exec.Command("docker", "network", "rm", network).CombinedOutput(); err != nil {
		t.Fatalf("remove owned network fixture: %v\n%s", err, out)
	}

	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "legacy.yaml")
	legacy := `services:
  proxy:
    image: alpine:3
    command: ["sh", "-c", "sleep 600"]
`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyArgs := []string{"compose", "-p", application, "-f", legacyPath}
	t.Cleanup(func() {
		args := append(append([]string{}, legacyArgs...), "down", "--remove-orphans")
		_ = exec.Command("docker", args...).Run()
	})
	up := append(append([]string{}, legacyArgs...), "up", "-d")
	if out, err := exec.Command("docker", up...).CombinedOutput(); err != nil {
		t.Fatalf("start legacy proxy: %v\n%s", err, out)
	}

	if err := e.EnsureApplicationNetwork(ctx); err != nil {
		t.Fatalf("migrate legacy Compose network: %v", err)
	}

	runtimePath := filepath.Join(dir, "runtime.yaml")
	runtime := fmt.Sprintf(`name: %s
services:
  web:
    image: alpine:3
    command: ["sh", "-c", "sleep 600"]
networks:
  default:
    external: true
    name: %s
`, application, network)
	if err := os.WriteFile(runtimePath, []byte(runtime), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeArgs := []string{"compose", "-p", application, "-f", runtimePath}
	up = append(append([]string{}, runtimeArgs...), "up", "-d")
	if out, err := exec.Command("docker", up...).CombinedOutput(); err != nil {
		t.Fatalf("start external-network release: %v\n%s", err, out)
	}
	down := append(append([]string{}, runtimeArgs...), "down")
	if out, err := exec.Command("docker", down...).CombinedOutput(); err != nil {
		t.Fatalf("tear down release with proxy attached: %v\n%s", err, out)
	}
	if err := exec.Command("docker", "network", "inspect", network).Run(); err != nil {
		t.Fatalf("external application network was removed: %v", err)
	}
	proxy, err := exec.Command("docker", "ps", "-q", "--filter", "label=com.docker.compose.service=proxy", "--filter", "network="+network).Output()
	if err != nil || strings.TrimSpace(string(proxy)) == "" {
		t.Fatalf("unmanaged proxy endpoint did not survive release teardown: %q, %v", proxy, err)
	}
}
