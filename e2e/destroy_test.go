package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

// A release may need an interpolation file the current project no longer
// declares after migrating a Compose-defined database to a managed service.
// Destroy must replay the release's declaration or Compose refuses to parse
// before removing even one container or volume.
func TestDestroyUsesReleaseRecordedInterpolationEnvironment(t *testing.T) {
	gate(t)
	ctx := context.Background()
	application := fmt.Sprintf("obissue77%d", os.Getpid())
	base := t.TempDir()
	releaseID := "20260821-120000-legacy"
	volume := application + "_legacy_data"

	currentBody := fmt.Sprintf(`api_version: onebox.run/v1
app: %s
base_path: %q
environments:
  production: {server: root@localhost}
workloads:
  web: {image: alpine:3}
`, application, base)
	current, err := app.LoadBytes([]byte(currentBody), filepath.Join(base, "current.yml"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := current.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}

	paths := release.PathsFor(resolved.NamesFor("production"))
	releaseDir := filepath.Join(paths.Releases, releaseID)
	if err := os.MkdirAll(releaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(releaseDir, "compose.yaml")
	envPath := filepath.Join(releaseDir, "legacy.env")
	composeBody := fmt.Sprintf(`services:
  legacy:
    image: alpine:3
    command: ["sh", "-c", "sleep 600"]
    environment:
      LEGACY_SECRET: ${LEGACY_SECRET:?LEGACY_SECRET is required}
    volumes: [%s:/data]
volumes:
  %s:
    name: %s
`, volume, volume, volume)
	snapshotBody := currentBody + "runtime:\n  env_files: [legacy.env]\n"
	for path, body := range map[string]string{
		composePath: composeBody,
		envPath:     "LEGACY_SECRET=recorded-value\n",
		filepath.Join(releaseDir, "ob.snapshot.yml"): snapshotBody,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/"+releaseID, paths.Current); err != nil {
		t.Fatal(err)
	}
	owner := resolved.NamesFor("production").HostOwnerPath()
	if err := os.MkdirAll(filepath.Dir(owner), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owner, []byte(application+" production\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	composeArgs := []string{"compose", "-p", application, "-f", composePath, "--env-file", envPath}
	cleanup := func() {
		args := append(append([]string{}, composeArgs...), "down", "--remove-orphans", "-v")
		_ = exec.Command("docker", args...).Run()
	}
	t.Cleanup(cleanup)
	up := append(append([]string{}, composeArgs...), "up", "-d")
	if out, err := exec.Command("docker", up...).CombinedOutput(); err != nil {
		t.Fatalf("start legacy release: %v\n%s", err, out)
	}

	e := engine.New(resolved, nil, transport.NewLocal(), engine.Options{Environment: "production", Out: os.Stderr})
	if err := e.Destroy(ctx, true, false); err != nil {
		t.Fatalf("destroy migrated release: %v", err)
	}
	containers, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+application).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(containers)) != "" {
		t.Fatalf("legacy containers survived destroy: %s", containers)
	}
	if err := exec.Command("docker", "volume", "inspect", volume).Run(); err == nil {
		t.Fatalf("legacy volume %s survived destroy --volumes", volume)
	}
}
