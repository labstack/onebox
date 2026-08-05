package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mergeFixture(t *testing.T, service string, ov overlay) (map[string]any, error) {
	t.Helper()
	svc, _, err := mergeComposeRef("testdata", "compose.yaml#"+service, ov)
	return svc, err
}

func mergeFixtureDeps(t *testing.T, service string, ov overlay) definitions {
	t.Helper()
	_, deps, err := mergeComposeRef("testdata", "compose.yaml#"+service, ov)
	if err != nil {
		t.Fatal(err)
	}
	return deps
}

// TestMergePreservesWhatTheUserWrote is the promise of the escape hatch: a
// workload the declaration cannot express keeps every setting it declared.
func TestMergePreservesWhatTheUserWrote(t *testing.T) {
	got, err := mergeFixture(t, "postgres", overlay{
		Labels: map[string]any{"ob.app": "ledger", "ob.workload": "db", "ob.release": "r1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["image"] != "postgres:18.4-alpine" {
		t.Errorf("image = %v", got["image"])
	}
	if got["healthcheck"] == nil {
		t.Error("the authored healthcheck must survive")
	}
	if got["volumes"] == nil {
		t.Error("the authored bind mount must survive")
	}
	if got["environment"] == nil {
		t.Error("the authored environment must survive")
	}
	labels := labelMap(got["labels"])
	if labels["ob.app"] != "ledger" || labels["ob.release"] != "r1" {
		t.Errorf("identity labels missing: %v", labels)
	}
}

// TestMergeAppendsIngressPreservingOrder: existing networks are kept, in order.
func TestMergeAppendsIngressPreservingOrder(t *testing.T) {
	got, err := mergeFixture(t, "redis", overlay{Network: "ob-ingress"})
	if err != nil {
		t.Fatal(err)
	}
	nets := networkNames(got["networks"])
	if len(nets) != 2 || nets[0] != "default" || nets[1] != "ob-ingress" {
		t.Fatalf("networks = %v, want [default ob-ingress]", nets)
	}
}

// TestMergeRefusesConflicts: Onebox names the key and the file rather than
// overwriting or silently dropping something the author wrote.
func TestMergeRefusesConflicts(t *testing.T) {
	cases := []struct {
		service string
		ov      overlay
		code    string
	}{
		{"named", overlay{}, "compose_container_name"},
		{"hostnet", overlay{Network: "ob-ingress"}, "compose_network_mode"},
		{"labelled", overlay{HasRoute: true}, "compose_traefik_label"},
		{"owned", overlay{}, "compose_ob_label"},
		{"attached", overlay{Network: "ob-ingress"}, "compose_ingress_attached"},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			_, err := mergeFixture(t, c.service, c.ov)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			var e *Error
			if !asError(err, &e) || e.Code != c.code {
				t.Fatalf("got %v, want %s", err, c.code)
			}
			if !strings.Contains(e.Message, "compose.yaml") {
				t.Errorf("the refusal should name the file: %s", e.Message)
			}
		})
	}
}

// TestMergeToleratesHarmlessCases: network_mode only conflicts when a network
// would actually be attached, and traefik labels only when we also route.
func TestMergeToleratesHarmlessCases(t *testing.T) {
	if _, err := mergeFixture(t, "hostnet", overlay{}); err != nil {
		t.Errorf("network_mode with no ingress network should be preserved: %v", err)
	}
	if _, err := mergeFixture(t, "labelled", overlay{HasRoute: false}); err != nil {
		t.Errorf("traefik labels with no declared route should be preserved: %v", err)
	}
}

// TestMissingServiceNamesWhatExists so the fix is obvious from the message.
func TestMissingServiceNamesWhatExists(t *testing.T) {
	_, err := mergeFixture(t, "ghost", overlay{})
	var e *Error
	if !asError(err, &e) || e.Code != "compose_service_missing" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(e.Message, "postgres") {
		t.Errorf("the error should list the services that do exist: %s", e.Message)
	}
}

// TestPathEscapeRefused: a reference may not read outside the repository, and
// the check is not lexical because `a/../../etc` is legal to write.
func TestPathEscapeRefused(t *testing.T) {
	for _, ref := range []string{"../outside.yaml#x", "/etc/compose.yaml#x"} {
		_, _, err := mergeComposeRef("testdata", ref, overlay{})
		var e *Error
		if !asError(err, &e) {
			t.Fatalf("%s: got %v", ref, err)
		}
		if e.Code != "path_escapes_repository" && e.Code != "path_absolute" {
			t.Errorf("%s: got %s, want a path refusal", ref, e.Code)
		}
	}
}

// TestComposeRefRendersEndToEnd puts the merge through generation.
func TestComposeRefRendersEndToEnd(t *testing.T) {
	y := `api_version: onebox.run/v1
app: ledger
environments:
  production: {server: root@1.2.3.4}
workloads:
  web:
    role: application
    image: nginx
    domain: ledger.example.com
    port: 8080
  db:
    role: daemon
    compose: compose.yaml#postgres
`
	p, err := LoadBytes([]byte(y), "testdata/ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Render("production", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := string(r.Bytes)
	for _, want := range []string{"postgres:18.4-alpine", "pg_isready", "ob.workload: db"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in rendered runtime\n%s", want, out)
		}
	}
	if strings.Contains(out, "x-ob-compose-ref") {
		t.Error("the reference marker should be replaced by the merged service")
	}
}

// TestCarriesNetworkAndVolumeDefinitions is a correctness fix, not a coverage
// one. A survey of real projects found 85 declaring non-default network
// topology and 35 with volume driver options. Dropping them would put a
// segmented service on the default network and turn an NFS mount into a local
// directory — silently, and only visible once data went to the wrong place.
func TestCarriesNetworkAndVolumeDefinitions(t *testing.T) {
	deps := mergeFixtureDeps(t, "segmented", overlay{})

	net, ok := deps.Networks["backend"]
	if !ok {
		t.Fatal("the segmented network definition was dropped")
	}
	if m, _ := net.(map[string]any); m["internal"] != true {
		t.Errorf("network settings were dropped: %v", net)
	}

	vol, ok := deps.Volumes["nfsdata"]
	if !ok {
		t.Fatal("the volume definition was dropped")
	}
	m, _ := vol.(map[string]any)
	if m["driver_opts"] == nil {
		t.Errorf("volume driver options were dropped: %v", vol)
	}
}

// TestExtendsRefused: the referenced file is read as plain YAML, so extends is
// not followed. Rendering a service without what it inherits would be worse
// than refusing.
func TestExtendsRefused(t *testing.T) {
	_, err := mergeFixture(t, "inherited", overlay{})
	var e *Error
	if !asError(err, &e) || e.Code != "compose_extends" {
		t.Fatalf("got %v, want compose_extends", err)
	}
}

// TestBindMountsNeedNoDefinition keeps the common case free of noise.
func TestBindMountsNeedNoDefinition(t *testing.T) {
	deps := mergeFixtureDeps(t, "postgres", overlay{})
	if len(deps.Volumes) != 0 {
		t.Errorf("a bind mount needs no top-level definition, got %v", deps.Volumes)
	}
}

// A declared health check reaches a Compose-referenced service.
//
// It did not, and the silence was the problem: the workload declared `health:`,
// generation dropped it, and the rollout then refused to roll a workload whose
// author had declared exactly the thing rolling needs. The whole Docker-gated
// end-to-end suite failed on this, and had been skipping.
func TestADeclaredHealthCheckReachesAReferencedService(t *testing.T) {
	dir := t.TempDir()
	ref := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(ref, []byte("services:\n  web:\n    image: busybox\n    command: [\"sleep\",\"1\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ob.yml")
	if err := os.WriteFile(path, []byte(`api_version: onebox.run/v1
app: shop
environments:
  production: {server: root@203.0.113.10}
workloads:
  web:
    role: application
    compose: "docker-compose.yaml#web"
    health: {http: /healthz, port: 8080}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered.Bytes), "healthcheck:") {
		t.Fatalf("the declared health check did not reach the referenced service:\n%s", rendered.Bytes)
	}
	if !strings.Contains(string(rendered.Bytes), "8080") {
		t.Error("the probe reached the service without the port it was declared with")
	}
	// And it is what the rollout will gate on, so the workload rolls.
	if mode := r.Spec.Workloads["web"].Mode(); mode != "rolling" {
		t.Errorf("a compose-referenced workload declaring health should roll, got %q", mode)
	}
}
