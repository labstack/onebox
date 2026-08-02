package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/transport"
)

func TestLocalHookRunsOnRunnerNotHost(t *testing.T) {
	f := &transport.Fake{}
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Hooks["publish"] = config.Hook{Run: "echo $OB_RELEASE_ID > out.txt", Local: true}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: dir})
	if err := e.RunHook(context.Background(), "publish", "/var/lib/ob/sample/releases/R9", "x"); err != nil {
		t.Fatal(err)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("local hook must not touch the host transport: %v", f.Commands)
	}
	b, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "R9" {
		t.Fatalf("local hook env/cwd wrong: %q err=%v", b, err)
	}
}

func TestDeploySeamOrdering(t *testing.T) {
	f := happyFake()
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Hooks["pre_release"] = config.Hook{Run: "touch pre", Local: true}
	cfg.Hooks["post_release"] = config.Hook{Run: "touch post", Local: true}
	cfg.Hooks["post_deploy"] = config.Hook{Run: "touch done", Local: true}
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, LocalDir: dir})
	if err := e.Deploy(context.Background(), "R1", t.TempDir()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	for _, name := range []string{"pre", "post", "done"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("seam hook %s did not run: %v", name, err)
		}
	}
}

func TestAdvisoryURLCheckWarnsButPasses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	f := happyFake()
	var out bytes.Buffer
	cfg := testConfig()
	cfg.Verify = append(cfg.Verify, config.VerifyCheck{URL: srv.URL, Advisory: true})
	e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Verify(context.Background()); err != nil {
		t.Fatalf("advisory failure must not fail verify: %v", err)
	}
	if !strings.Contains(out.String(), "advisory") {
		t.Fatalf("advisory failure should warn: %s", out.String())
	}
}

func TestAuthoritativeURLCheckFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<html>nope</html>"))
	}))
	defer srv.Close()
	f := happyFake()
	cfg := testConfig()
	cfg.Verify = append(cfg.Verify, config.VerifyCheck{URL: srv.URL, Contains: `id="root"`})
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, HTTPTimeout: 2 * time.Second})
	if err := e.Verify(context.Background()); err == nil {
		t.Fatal("non-advisory url check with missing substring must fail")
	}
}
