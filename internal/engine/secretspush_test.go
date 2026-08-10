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
				case strings.Contains(command, "_host/owner"):
					return transport.Result{Stdout: "shop\n"}, true
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
			if len(f.Commands) != 3 {
				t.Fatalf("drift performed work beyond current+snapshot reads: %#v", f.Commands)
			}
		})
	}
}

func TestValidateSecretPayloadsRefusesIncompleteOrUnsafeGraphs(t *testing.T) {
	graph := resolvedSecretGraph(t, secretGraphProject).SecretDeclarationGraph()
	valid := make([]SecretPayload, 0, len(graph))
	for _, declaration := range graph {
		valid = append(valid, SecretPayload{Path: declaration.OutputPath, Bytes: []byte("value")})
	}
	tests := map[string][]SecretPayload{
		"missing":   append([]SecretPayload(nil), valid[:len(valid)-1]...),
		"duplicate": append(append([]SecretPayload(nil), valid...), valid[0]),
		"unknown":   append(append([]SecretPayload(nil), valid...), SecretPayload{Path: ".ob-unknown", Bytes: []byte("value")}),
		"absolute":  {{Path: "/tmp/secret", Bytes: []byte("value")}},
		"traversal": {{Path: "../secret", Bytes: []byte("value")}},
	}
	for name, payloads := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := validateSecretPayloads(graph, payloads); err == nil {
				t.Fatalf("unsafe payload graph was accepted: %#v", payloads)
			}
		})
	}
}

func TestCurrentSecretEngineKeepsDeployedOperationalSettings(t *testing.T) {
	deployed := strings.Replace(secretGraphProject, "  web:\n    image: nginx\n", "  web:\n    image: nginx\n    replicas: 3\n", 1)
	target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "/ob.snapshot.yml") {
			return transport.Result{Stdout: deployed}, true
		}
		return transport.Result{}, false
	}}
	engine := New(resolvedSecretGraph(t, secretGraphProject), nil, target, Options{Environment: "production", Out: io.Discard})
	runtimeEngine, err := engine.currentSecretEngine(context.Background(), "20260809-120000-current")
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeEngine.Spec.Workloads["web"].Replicas; got != 3 {
		t.Fatalf("rotation runtime replicas = %d, want deployed value 3", got)
	}
	if got := engine.Spec.Workloads["web"].Replicas; got != 1 {
		t.Fatalf("test did not preserve distinct working-tree value: %d", got)
	}
}
