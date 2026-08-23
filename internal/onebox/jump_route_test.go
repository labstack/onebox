package onebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func writeJumpProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ob.yml")
	body := `
api_version: onebox.run/v1
app: demo
environments:
  production:
    server: deploy@example.invalid
    jump: bastion@jump.invalid:2222
image: ghcr.io/example/app:v1
domain: demo.example.com
port: 8080
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The route is the trust boundary, so it is what the operator confirms: a
// binding naming only the private target would let the bastion be swapped
// under an approval that never mentioned it.
func TestExecutionBindingNamesTheJumpRoute(t *testing.T) {
	service := New(Options{
		ConfigPath: writeJumpProject(t),
		Connect: func(context.Context, transport.Route) (transport.Transport, error) {
			t.Fatal("resolving a binding must not connect")
			return nil, nil
		},
	})
	binding, err := service.ResolveExecutionBinding(context.Background(), KindDestroy)
	if err != nil {
		t.Fatal(err)
	}
	want := "deploy@example.invalid via bastion@jump.invalid:2222"
	if binding.Server != want {
		t.Fatalf("binding.Server = %q, want %q", binding.Server, want)
	}
}

func TestConnectorReceivesTheDeclaredJump(t *testing.T) {
	var got string
	service := New(Options{
		ConfigPath: writeJumpProject(t),
		Connect: func(_ context.Context, route transport.Route) (transport.Transport, error) {
			got = route.String()
			if route.Jump == nil {
				t.Fatal("connector route carries no jump")
			}
			return nil, errStopAfterConnect
		},
	})
	_, _ = service.PlanDeploy(context.Background(), PlanDeployRequest{})
	if !strings.Contains(got, "via bastion@jump.invalid:2222") {
		t.Fatalf("connector route = %q, want the declared jump", got)
	}
}

// errStopAfterConnect ends the operation at the connector: the route the
// connector was handed is the whole subject of the test.
var errStopAfterConnect = errors.New("stop after connect")

// The bastion is part of what was approved, so swapping it must invalidate the
// confirmation the operator already gave.
func TestChangingOnlyTheJumpChangesTheBinding(t *testing.T) {
	binding := func(jump string) ExecutionBinding {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "ob.yml")
		body := `
api_version: onebox.run/v1
app: demo
environments:
  production:
    server: deploy@example.invalid
    jump: ` + jump + `
image: ghcr.io/example/app:v1
domain: demo.example.com
port: 8080
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		resolved, err := New(Options{ConfigPath: path}).ResolveExecutionBinding(context.Background(), KindDestroy)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	first, second := binding("bastion@jump.invalid:2222"), binding("bastion@other.invalid:2222")
	// The digests differ because the file differs, which would pass even if
	// the route were dropped from the binding — so the server field is
	// asserted on its own.
	if first.Server == second.Server {
		t.Fatalf("binding server is unchanged by a different jump host: %q", first.Server)
	}
	if first == second {
		t.Fatalf("binding is unchanged by a different jump host: %#v", first)
	}
}
