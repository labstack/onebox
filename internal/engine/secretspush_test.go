package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

const secretGraphProject = `api_version: onebox.run/v1
app: shop
environments:
  production: {server: deploy@example.invalid}
runtime:
  env_files:
    - {file: shared.enc.env, provider: sops}
workloads:
  web:
    image: nginx
    port: 3000
    domain: shop.example.com
    env_files:
      - {file: first.enc.env, provider: sops}
      - {file: second.enc.env, provider: sops}
  worker:
    role: worker
    image: nginx
`

func resolvedSecretGraph(t *testing.T, project string) *app.Resolved {
	t.Helper()
	spec, err := app.LoadBytes([]byte(project), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestSecretsPushRefusesExactDeclarationGraphDriftBeforeMutation(t *testing.T) {
	variants := map[string]string{
		"added": strings.Replace(secretGraphProject,
			"      - {file: second.enc.env, provider: sops}\n",
			"      - {file: second.enc.env, provider: sops}\n      - {file: third.enc.env, provider: sops}\n", 1),
		"removed": strings.Replace(secretGraphProject,
			"      - {file: second.enc.env, provider: sops}\n", "", 1),
		"reordered": strings.Replace(secretGraphProject,
			"      - {file: first.enc.env, provider: sops}\n      - {file: second.enc.env, provider: sops}\n",
			"      - {file: second.enc.env, provider: sops}\n      - {file: first.enc.env, provider: sops}\n", 1),
		"provider removed": strings.Replace(secretGraphProject,
			"{file: first.enc.env, provider: sops}", "{file: first.enc.env}", 1),
		"scope changed": strings.Replace(secretGraphProject,
			"    env_files:\n      - {file: first.enc.env, provider: sops}\n      - {file: second.enc.env, provider: sops}\n  worker:\n    role: worker\n    image: nginx\n",
			"  worker:\n    role: worker\n    image: nginx\n    env_files:\n      - {file: first.enc.env, provider: sops}\n      - {file: second.enc.env, provider: sops}\n", 1),
	}

	for name, deployedProject := range variants {
		t.Run(name, func(t *testing.T) {
			if deployedProject == secretGraphProject {
				t.Fatal("test variant did not change the project")
			}
			f := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				switch {
				case strings.Contains(command, "readlink"):
					return transport.Result{Stdout: "releases/20260809-120000-current\n"}, true
				case strings.Contains(command, "/ob.snapshot.yml"):
					return transport.Result{Stdout: deployedProject}, true
				default:
					return transport.Result{}, true
				}
			}}
			e := New(resolvedSecretGraph(t, secretGraphProject), nil, f, Options{
				Environment: "production",
				Out:         io.Discard,
			})

			_, err := e.SecretsPushBatch(context.Background(), []SecretPayload{
				{Path: ".ob-decrypted-sops-shared.enc.env", Bytes: []byte("SHARED=changed\n")},
				{Path: ".ob-decrypted-sops-first.enc.env", Bytes: []byte("FIRST=changed\n")},
				{Path: ".ob-decrypted-sops-second.enc.env", Bytes: []byte("SECOND=changed\n")},
			})
			var drift *SecretDeclarationDriftError
			if !errors.As(err, &drift) {
				t.Fatalf("got %v, want SecretDeclarationDriftError", err)
			}
			if drift.Code() != "secret_declaration_not_deployed" || !strings.Contains(err.Error(), "ob deploy") {
				t.Fatalf("drift error lacks stable code or deploy guidance: %v", err)
			}
			if len(f.Commands) != 2 {
				t.Fatalf("drift performed work beyond current+snapshot reads: %#v", f.Commands)
			}
		})
	}
}
