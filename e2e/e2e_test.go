// Package e2e proves the core live-deploy contract mechanically under
// load with zero failed requests. Gated: OB_E2E=1 + local docker.
package e2e

import (
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

// Keep the checked-in examples on the stable authoring contract even when the
// Docker-gated deployment tests are skipped.
func TestV1ConfigFixturesLoad(t *testing.T) {
	for _, path := range []string{
		"testdata/app/ob.yml",
		"testdata/worker/ob.yml",
		"testdata/worker/ob-broken.yml",
	} {
		t.Run(path, func(t *testing.T) {
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("load stable v1 config: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("validate stable v1 config: %v", err)
			}
		})
	}
}

func TestZeroDowntimeDeploy(t *testing.T) {
	if os.Getenv("OB_E2E") != "1" {
		t.Skip("set OB_E2E=1 (requires local docker)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	dir, err := filepath.Abs("testdata/app")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	t.Setenv("OB_BASE_DIR", base)
	ctx := context.Background()
	tr := transport.NewLocal()
	t.Cleanup(func() {
		down := exec.Command("docker", "compose", "-p", "obe2e", "-f", filepath.Join(dir, "docker-compose.yaml"), "down", "-v", "--remove-orphans")
		down.Run()
	})

	deploy := func(version string) error {
		t.Setenv("APP_VERSION", version)
		cfg, err := config.Load(filepath.Join(dir, "ob.yml"))
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		p, err := compose.Load(ctx, filepath.Join(dir, "docker-compose.yaml"), cfg.App)
		if err != nil {
			return err
		}
		if err := compose.Classify(p, cfg); err != nil {
			return err
		}
		id := release.NewID(time.Now(), "")
		// ids need uniqueness at sub-second granularity across the two deploys
		id = id + "-" + version
		rendered, err := compose.Render(p, cfg, id)
		if err != nil {
			return err
		}
		staging := t.TempDir()
		if err := release.Stage(staging, rendered, []byte("snapshot")); err != nil {
			return err
		}
		e := engine.New(cfg, p, tr, engine.Options{Out: os.Stderr})
		return e.Deploy(ctx, id, staging)
	}

	// The accessory proxy must be running before preflight.
	up := exec.Command("docker", "compose", "-p", "obe2e", "-f", filepath.Join(dir, "docker-compose.yaml"), "up", "-d", "traefik")
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("start traefik: %v\n%s", err, out)
	}
	waitHealthy(t, "obe2e", "traefik", 60*time.Second)

	if err := deploy("v1"); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	waitBody(t, "http://localhost:18080/", "v1\n", 30*time.Second)

	// hammer the edge during the v2 deploy; count failures
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
				if err != nil {
					t.Logf("probe error: %v", err)
				} else {
					t.Logf("probe status: %d", resp.StatusCode)
				}
			}
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	err = deploy("v2")
	close(stop)
	<-donePolling
	if err != nil {
		t.Fatalf("deploy v2: %v", err)
	}

	if f := failures.Load(); f > 0 {
		t.Fatalf("zero-downtime violated: %d/%d requests failed during roll", f, total.Load())
	}
	waitBody(t, "http://localhost:18080/", "v2\n", 10*time.Second)
	fmt.Printf("zero-downtime proven: %d requests, 0 failures\n", total.Load())
}

func waitHealthy(t *testing.T, project, svc string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		out := ""
		psOut, _ := exec.Command("docker", "ps", "-q",
			"--filter", "label=com.docker.compose.project="+project,
			"--filter", "label=com.docker.compose.service="+svc).Output()
		if id := strings.TrimSpace(strings.Split(string(psOut), "\n")[0]); id != "" {
			insOut, _ := exec.Command("docker", "inspect", "-f",
				"{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", id).Output()
			out = string(insOut)
		}
		if strings.TrimSpace(out) == "healthy" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s/%s never became healthy (last: %q)", project, svc, strings.TrimSpace(out))
		}
		time.Sleep(time.Second)
	}
}

func waitBody(t *testing.T, url, want string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := client.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET %s: want %q, last got %q", url, want, body)
			}
		} else if time.Now().After(deadline) {
			t.Fatalf("GET %s: %v", url, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
