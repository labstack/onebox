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

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/compose"
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
			// Loading validates: there is no separate step that can be
			// skipped, so no path reaches execution unvalidated.
			if _, err := app.Load(path); err != nil {
				t.Fatalf("load project: %v", err)
			}
		})
	}
}

func TestZeroDowntimeDeploy(t *testing.T) {
	if os.Getenv("OB_E2E") != "1" {
		t.Skip("set OB_E2E=1 (requires local docker)")
	}
	// Opting in is a promise that Docker is here. Skipping past a broken daemon
	// once OB_E2E=1 is set turns a gate into a green tick for work nobody did.
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("OB_E2E=1 was set but docker is not usable: %v", err)
	}
	dir, err := filepath.Abs("testdata/app")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	ctx := context.Background()
	tr := transport.NewLocal()
	t.Cleanup(func() {
		down := exec.Command("docker", "compose", "-p", "obe2e", "-f", filepath.Join(dir, "docker-compose.yaml"), "down", "-v", "--remove-orphans")
		down.Run()
	})

	// Captured so the payload-digest invariant can be checked against the live
	// host after a deploy.
	var lastResolved *app.Resolved
	var lastStaging, lastID string

	deploy := func(version string) error {
		t.Setenv("APP_VERSION", version)
		spec, err := app.Load(filepath.Join(dir, "ob.yml"))
		if err != nil {
			return err
		}
		// base_path is the field, not an environment variable: the engine now
		// has one path authority and it is the project's own.
		spec.BasePath = base
		resolved, err := spec.Resolve("production")
		if err != nil {
			return err
		}
		id := release.NewID(time.Now(), "")
		// ids need uniqueness at sub-second granularity across the two deploys
		id = id + "-" + version
		rendered, err := resolved.Render("production", id, nil)
		if err != nil {
			return err
		}
		p, err := compose.LoadBytes(ctx, rendered.Bytes, resolved.Name, dir, nil)
		if err != nil {
			return err
		}
		staging := t.TempDir()
		if err := release.Stage(staging, rendered.Bytes, releaseSnapshot(t, dir, "ob.yml", base)); err != nil {
			return err
		}
		lastResolved, lastStaging, lastID = resolved, staging, id
		e := engine.New(resolved, p, tr, engine.Options{Out: os.Stderr, Environment: "production"})
		return e.Deploy(ctx, id, staging)
	}

	// The local walk and the remote find must select the same files. When they
	// do not, every plan reports a change: no deploy short-circuits, pre-release
	// migrations re-run, and a rotated secret can hash identically to the one it
	// replaced. A unit test already runs the shell half against a local `find`;
	// this proves it against the host's own find over a real release directory.
	assertPayloadDigestsAgree := func(stage string) string {
		t.Helper()
		local, err := engine.LocalPayloadDigest(lastResolved, lastStaging)
		if err != nil {
			t.Fatalf("%s: local payload digest: %v", stage, err)
		}
		e := engine.New(lastResolved, nil, tr, engine.Options{Out: os.Stderr, Environment: "production"})
		remote, err := e.RemotePayloadDigest(ctx, lastID)
		if err != nil {
			t.Fatalf("%s: remote payload digest: %v", stage, err)
		}
		if local != remote {
			t.Fatalf("%s: payload digests disagree, so no deploy can ever be a no-op: local %s, remote %s",
				stage, local, remote)
		}
		return local
	}

	// The host proxy must be running before preflight.
	up := exec.Command("docker", "compose", "-p", "obe2e", "-f", filepath.Join(dir, "docker-compose.yaml"), "up", "-d", "traefik")
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("start traefik: %v\n%s", err, out)
	}
	waitHealthy(t, "obe2e", "traefik", 60*time.Second)
	bootstrapHost(t, dir, "ob.yml", base)

	if err := deploy("v1"); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	waitBody(t, "http://localhost:18080/", "v1\n", 30*time.Second)
	assertPayloadDigestsAgree("after v1")

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

	// The redeploy case matters more than the first: this release directory now
	// sits alongside a predecessor and carries the manifest the lifecycle wrote
	// after upload, which is where the local and remote selections diverged
	// before.
	rolled := assertPayloadDigestsAgree("after v2")

	// Unchanged content is the case the digest exists for, and the one a
	// changing v1 -> v2 roll cannot test: when both sides move together,
	// symmetry can hold while no-op detection is broken. Deploying the same
	// bytes again must produce the same digest, or nothing can ever
	// short-circuit and every redeploy re-runs pre-release migrations.
	if err := deploy("v2"); err != nil {
		t.Fatalf("redeploy v2: %v", err)
	}
	if again := assertPayloadDigestsAgree("after unchanged redeploy"); again != rolled {
		t.Fatalf("identical content produced a different payload digest: %s then %s", rolled, again)
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
