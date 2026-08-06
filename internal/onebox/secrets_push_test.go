package onebox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// pushFake answers the reads `secrets push` makes: a current release, and a
// remote hash that matches nothing so every entry is treated as changed.
func pushFake() *transport.Fake {
	return &transport.Fake{
		TargetName: "deploy@example.invalid",
		HostName:   "example.invalid",
		Dynamic: func(cmd string) (transport.Result, bool) {
			switch {
			case strings.Contains(cmd, "readlink"):
				return transport.Result{Stdout: "releases/R7\n"}, true
			case strings.Contains(cmd, "sha256sum"):
				return transport.Result{Stdout: "deadbeef\n"}, true
			case strings.Contains(cmd, "docker ps -q"):
				return transport.Result{Stdout: "C1\n"}, true
			case strings.Contains(cmd, "State.Status"):
				return transport.Result{Stdout: "running\n"}, true
			case strings.Contains(cmd, "docker inspect"):
				return transport.Result{Stdout: "healthy\n"}, true
			}
			return transport.Result{}, true
		},
	}
}

// pushProject writes a project whose two workloads each hold their own
// encrypted entry, targeting the address the test connector expects.
func pushProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("api.enc.env", "TOKEN=api-token\n")
	write("worker.enc.env", "TOKEN=worker-token\n")
	write("ob.yml", `api_version: onebox.run/v1
app: shop
environments:
  production:
    server: deploy@example.invalid
workloads:
  web:
    image: nginx
    port: 3000
    domain: shop.example.com
    env_files: [{file: api.enc.env, provider: sops}]
  jobs:
    role: worker
    image: nginx
    env_files: [{file: worker.enc.env, provider: sops}]
`)
	return filepath.Join(dir, "ob.yml")
}

func pushService(t *testing.T, f *transport.Fake) *Service {
	t.Helper()
	base := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	tick := 0
	return New(Options{
		ConfigPath: pushProject(t),
		Now: func() time.Time {
			tick++
			return base.Add(time.Duration(tick) * time.Minute)
		},
		Connect: func(_ context.Context, target string) (transport.Transport, error) {
			return f, nil
		},
	})
}

// `ob secrets push` rotates every encrypted entry, not the first one.
//
// The release carries one file per entry. Pushing one and stopping leaves the
// rest holding the values staged when the release was made — so rotating a
// leaked credential reports success while the workload that actually holds it
// keeps using the old one. That is the same failure the constant filename
// caused, one layer up, and it was equally invisible.
func TestSecretsPushRotatesEveryEntry(t *testing.T) {
	fakeSops(t)
	f := pushFake()
	s := pushService(t, f)

	_, err := s.Execute(context.Background(), ExecuteRequest{Kind: KindSecretsPush})
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	seen := strings.Join(f.Commands, "\n")
	for _, file := range []string{"api.enc.env", "worker.enc.env"} {
		name := app.EnvFile{File: file, Provider: "sops"}.StagedPath()
		if !strings.Contains(seen, name) {
			t.Errorf("%s was never pushed; its workload keeps the values the release was staged with", name)
		}
	}
	// Each name is written through its own temporary and moved into place, so
	// a half-finished push never leaves a container reading a truncated file.
	// That the bytes under each name are that entry's own is settled where the
	// release is staged, in TestEveryEncryptedEntryIsStagedUnderItsOwnName;
	// the fake transport does not carry upload contents.
	for _, file := range []string{"api.enc.env", "worker.enc.env"} {
		name := app.EnvFile{File: file, Provider: "sops"}.StagedPath()
		if !strings.Contains(seen, name+".tmp-") {
			t.Errorf("%s was not staged through a temporary before replacing the live file", name)
		}
	}
}

// A project with nothing encrypted is told so, rather than reporting a push.
func TestSecretsPushWithNothingEncryptedIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ob.yml"), []byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: deploy@example.invalid}}
workloads:
  web: {image: nginx, port: 3000, domain: shop.example.com}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := pushFake()
	s := New(Options{
		ConfigPath: filepath.Join(dir, "ob.yml"),
		Now:        func() time.Time { return time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC) },
		Connect: func(_ context.Context, _ string) (transport.Transport, error) {
			return f, nil
		},
	})
	if _, err := s.Execute(context.Background(), ExecuteRequest{Kind: KindSecretsPush}); err == nil {
		t.Fatal("a project declaring no encrypted entry must be told, not reported as pushed")
	}
}
