package onebox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

const bootstrapBuildProject = `api_version: onebox.run/v1
app: demo
environments: {production: {server: deploy@example.invalid}}
runtime:
  env_files: [app.env]
workloads:
  api:
    role: application
    build: {context: ., dockerfile: Dockerfile}
    env: {INLINE_SECRET: inline-application-secret}
proxy: {kind: none}
`

func writeBootstrapBuildProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"ob.yml":     bootstrapBuildProject,
		"app.env":    "BOOTSTRAP_MUST_NOT_STAGE=this-application-secret\n",
		"Dockerfile": "FROM scratch\n# application-source-must-not-stage\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "ob.yml")
}

func TestBootstrapAcceptsBuildSourceWithoutStagingApplicationPayload(t *testing.T) {
	fake := &transport.Fake{
		HostName:   "example.invalid",
		TargetName: "deploy@example.invalid",
		Dynamic: func(command string) (transport.Result, bool) {
			if strings.Contains(command, "/_host/owner") {
				return transport.Result{Stdout: "demo\n"}, true
			}
			if strings.Contains(command, "docker network inspect --format") {
				return transport.Result{ExitCode: 1, Stderr: "Error response from daemon: network demo_default not found"}, true
			}
			return transport.Result{}, false
		},
	}
	service := New(Options{
		ConfigPath:  writeBootstrapBuildProject(t),
		Environment: "production",
		Connect: func(context.Context, transport.Route) (transport.Transport, error) {
			return fake, nil
		},
	})
	if binding, err := service.ResolveExecutionBinding(context.Background(), KindBootstrap); err != nil {
		t.Fatalf("resolve image-free bootstrap binding: %v", err)
	} else if binding.ConfigDigest == "" || binding.ComposeDigest == "" {
		t.Fatalf("bootstrap binding is incomplete: %#v", binding)
	}

	result, err := service.Execute(context.Background(), ExecuteRequest{Kind: KindBootstrap})
	if err != nil {
		t.Fatalf("bootstrap with an unresolved build source: %v", err)
	}
	if result.Status != OperationStatusSuccess || result.ReleaseID == "" || result.EvidenceID != result.ReleaseID {
		t.Fatalf("bootstrap result = %#v", result)
	}
	if len(fake.Uploads) != 0 {
		t.Fatalf("bootstrap must not upload application payload: %v", fake.Uploads)
	}
	for _, command := range fake.Commands {
		if strings.Contains(command, "this-application-secret") ||
			strings.Contains(command, "inline-application-secret") ||
			strings.Contains(command, "application-source-must-not-stage") {
			t.Fatalf("application secret leaked into target command: %s", command)
		}
	}

	if _, err := service.PlanDeploy(context.Background(), PlanDeployRequest{}); err == nil || !strings.Contains(err.Error(), "image_unresolved") {
		t.Fatalf("deploy planning must remain strict about release images: %v", err)
	}
}
