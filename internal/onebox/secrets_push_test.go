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

const pushProjectYAML = `api_version: onebox.run/v1
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
`

// pushFake answers the reads `secrets push` makes: a current release, and a
// remote hash that matches nothing so every entry is treated as changed.
func pushFake() *transport.Fake {
	const oldGeneration = "sg-111111111111111111111111"
	containerGeneration := map[string]string{"WOLD": oldGeneration, "JOLD": oldGeneration}
	containerByWorkload := map[string]string{"web": "WOLD", "jobs": "JOLD"}
	return &transport.Fake{
		TargetName: "deploy@example.invalid",
		HostName:   "example.invalid",
		Dynamic: func(cmd string) (transport.Result, bool) {
			switch {
			case strings.Contains(cmd, "_host/owner"):
				return transport.Result{Stdout: "shop\n"}, true
			case strings.Contains(cmd, "readlink"):
				return transport.Result{Stdout: "releases/20260712-180000-current\n"}, true
			case strings.Contains(cmd, "/ob.snapshot.yml"):
				return transport.Result{Stdout: pushProjectYAML}, true
			case strings.HasPrefix(strings.TrimSpace(cmd), "cat ") && strings.Contains(cmd, "/compose.yaml"):
				return transport.Result{Stdout: `services:
  web:
    env_file: [.ob-secret-generations/sg-111111111111111111111111/.ob-decrypted-sops-api.enc.env]
    labels: {ob.secret-generation: sg-111111111111111111111111}
  jobs:
    env_file: [.ob-secret-generations/sg-111111111111111111111111/.ob-decrypted-sops-worker.enc.env]
    labels: {ob.secret-generation: sg-111111111111111111111111}
`}, true
			case strings.Contains(cmd, "cmp -s"):
				return transport.Result{ExitCode: 1}, true
			case strings.Contains(cmd, "docker compose") && strings.Contains(cmd, " up -d "):
				generation := generationFromSecretCommand(cmd)
				workload := "web"
				if strings.Contains(cmd, "jobs") {
					workload = "jobs"
				}
				identifier := strings.ToUpper(workload[:1]) + "NEW"
				containerByWorkload[workload] = identifier
				containerGeneration[identifier] = generation
				return transport.Result{}, true
			case strings.Contains(cmd, "docker ps -q"):
				workload := "web"
				if strings.Contains(cmd, "service=jobs") || strings.Contains(cmd, "service='jobs'") {
					workload = "jobs"
				}
				return transport.Result{Stdout: containerByWorkload[workload] + "\n"}, true
			case strings.Contains(cmd, "ob.secret-generation"):
				for identifier, generation := range containerGeneration {
					if strings.HasSuffix(cmd, " "+identifier) {
						return transport.Result{Stdout: generation + "\n"}, true
					}
				}
				return transport.Result{ExitCode: 1}, true
			case strings.Contains(cmd, "State.Status"):
				return transport.Result{Stdout: "running\n"}, true
			case strings.Contains(cmd, "docker inspect"):
				return transport.Result{Stdout: "healthy\n"}, true
			}
			return transport.Result{}, false
		},
	}
}

func generationFromSecretCommand(command string) string {
	const marker = "/.ob-secret-generations/"
	start := strings.Index(command, marker)
	if start < 0 {
		return ""
	}
	remainder := command[start+len(marker):]
	if end := strings.IndexByte(remainder, '/'); end >= 0 {
		return remainder[:end]
	}
	return ""
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
	write("ob.yml", pushProjectYAML)
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
	if len(f.Uploads) != 1 || !strings.Contains(f.Uploads[0], ".secret-upload-sg-") {
		t.Fatalf("complete generation was not uploaded once: %v", f.Uploads)
	}
	for _, workload := range []string{"web", "jobs"} {
		if !strings.Contains(seen, "--force-recreate") || !strings.Contains(seen, " "+workload) {
			t.Errorf("%s was not generation-force-replaced", workload)
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
