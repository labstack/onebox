// Operational recovery scenarios, run for real against local Docker:
//
//	A: kill the runner mid-release → `resume` recovers, zero downtime held
//	B: break a worker → the release halts, old keeps serving
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

func gate(t *testing.T) {
	t.Helper()
	if os.Getenv("OB_E2E") != "1" {
		t.Skip("set OB_E2E=1 (requires local docker)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
}

// buildDeploy loads config+compose fresh (env-sensitive) and returns an
// engine plus a staged release ready for Deploy.
func buildDeploy(t *testing.T, dir, cfgFile, version string) (*engine.Engine, string, string) {
	t.Helper()
	t.Setenv("APP_VERSION", version)
	ctx := context.Background()
	cfg, err := config.Load(filepath.Join(dir, cfgFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := compose.Load(ctx, filepath.Join(dir, "docker-compose.yaml"), cfg.App)
	if err != nil {
		t.Fatal(err)
	}
	if err := compose.Classify(p, cfg); err != nil {
		t.Fatal(err)
	}
	id := release.NewID(time.Now(), "") + "-" + version
	rendered, err := compose.Render(p, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if _, err := compose.StagePayload(p, staging); err != nil {
		t.Fatal(err)
	}
	if err := release.Stage(staging, rendered, []byte("snapshot")); err != nil {
		t.Fatal(err)
	}
	e := engine.New(cfg, p, transport.NewLocal(), engine.Options{Out: os.Stderr})
	return e, id, staging
}

func composeDown(project, dir string) {
	cmd := exec.Command("docker", "compose", "-p", project, "-f", filepath.Join(dir, "docker-compose.yaml"), "down", "-v", "--remove-orphans")
	cmd.Run()
}

func webContainers(project string) []string {
	out, _ := exec.Command("docker", "ps", "-q",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service=web").Output()
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			ids = append(ids, l)
		}
	}
	return ids
}

// Scenario A: the runner dies mid-roll (context cancelled the moment the
// newcomer appears); resume finishes the deploy; the edge never drops a
// request through crash + recovery.
func TestKillRunnerMidReleaseThenResume(t *testing.T) {
	gate(t)
	dir, _ := filepath.Abs("testdata/app")
	t.Setenv("OB_BASE_DIR", t.TempDir())
	composeDown("obe2e", dir)
	t.Cleanup(func() { composeDown("obe2e", dir) })

	// traefik accessory up + healthy, then v1
	up := exec.Command("docker", "compose", "-p", "obe2e", "-f", filepath.Join(dir, "docker-compose.yaml"), "up", "-d", "traefik")
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("traefik: %v\n%s", err, out)
	}
	waitHealthy(t, "obe2e", "traefik", 60*time.Second)
	e, id, staging := buildDeploy(t, dir, "ob.yml", "v1")
	if err := e.Deploy(context.Background(), id, staging); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	waitBody(t, "http://localhost:18080/", "v1\n", 30*time.Second)

	// probes run through crash + resume
	var failures, total atomic.Int64
	stop := make(chan struct{})
	donePolling := make(chan struct{})
	go func() {
		defer close(donePolling)
		client := &http.Client{Timeout: 2 * time.Second}
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := client.Get("http://localhost:18080/")
			total.Add(1)
			if err != nil || resp.StatusCode != 200 {
				failures.Add(1)
			}
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	// v2 with a runner that "dies" as soon as the newcomer exists
	e2, id2, staging2 := buildDeploy(t, dir, "ob.yml", "v2")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if len(webContainers("obe2e")) >= 2 {
				cancel() // the runner is dead
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	err := e2.Deploy(ctx, id2, staging2)
	if err == nil {
		t.Fatal("killed deploy should have failed")
	}
	t.Logf("runner killed as intended: %v", err)

	// resume with a fresh runner
	e3, _, _ := buildDeploy(t, dir, "ob.yml", "v2")
	if err := e3.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	close(stop)
	<-donePolling

	if f := failures.Load(); f > 0 {
		t.Fatalf("zero-downtime violated across crash+resume: %d/%d failed", f, total.Load())
	}
	waitBody(t, "http://localhost:18080/", "v2\n", 15*time.Second)
	if n := len(webContainers("obe2e")); n != 1 {
		t.Fatalf("expected exactly 1 web container after resume, got %d", n)
	}
	fmt.Printf("crash+resume proven: %d requests, 0 failures\n", total.Load())
}

// Scenario B: a broken worker halts the release; the untouched web role keeps
// serving the old version.
func TestBrokenWorkerHaltsDeployOldKeepsServing(t *testing.T) {
	gate(t)
	dir, _ := filepath.Abs("testdata/worker")
	t.Setenv("OB_BASE_DIR", t.TempDir())
	composeDown("obworker", dir)
	t.Cleanup(func() { composeDown("obworker", dir) })

	e, id, staging := buildDeploy(t, dir, "ob.yml", "v1")
	if err := e.Deploy(context.Background(), id, staging); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	waitBody(t, "http://localhost:18081/", "v1\n", 30*time.Second)

	e2, id2, staging2 := buildDeploy(t, dir, "ob-broken.yml", "v2")
	err := e2.Deploy(context.Background(), id2, staging2)
	if err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("broken worker must halt the release: %v", err)
	}
	t.Logf("halted as intended: %v", err)

	// the untouched web role still serves v1
	waitBody(t, "http://localhost:18081/", "v1\n", 10*time.Second)

	// and the journal knows
	var audit bytes.Buffer
	e3, _, _ := buildDeploy(t, dir, "ob.yml", "v1")
	e3.Opts.Out = &audit
	if err := e3.Audit(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(audit.String(), "failed") {
		t.Fatalf("audit must show the failed run:\n%s", audit.String())
	}
}
