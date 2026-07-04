package compose

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func renderDoc(t *testing.T) map[string]any {
	t.Helper()
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	out, err := Render(p, cfg, "20260702-120000-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	return doc["services"].(map[string]any)
}

func hc(t *testing.T, svcs map[string]any, name string) string {
	t.Helper()
	svc := svcs[name].(map[string]any)
	h, ok := svc["healthcheck"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no healthcheck rendered", name)
	}
	test := h["test"].([]any)
	parts := make([]string, len(test))
	for i, v := range test {
		parts[i] = v.(string)
	}
	return strings.Join(parts, " ")
}

func TestRenderWrapsHealthcheckWithDrainGuard(t *testing.T) {
	svcs := renderDoc(t)
	// server has ready.http -> generated wins, and it must carry the drain guard
	got := hc(t, svcs, "server")
	if !strings.Contains(got, DrainFile) {
		t.Fatalf("server healthcheck missing drain guard: %s", got)
	}
	if !strings.Contains(got, "/healthz") {
		t.Fatalf("server healthcheck should probe ready.http path: %s", got)
	}
}

func TestRenderTouchesOnlyRoleServices(t *testing.T) {
	svcs := renderDoc(t)
	pg := svcs["postgres"].(map[string]any)
	if labels, ok := pg["labels"]; ok {
		if s, _ := yaml.Marshal(labels); strings.Contains(string(s), "ob.") {
			t.Fatalf("accessory postgres must not receive ob labels: %s", s)
		}
	}
	web := svcs["server"].(map[string]any)
	s, _ := yaml.Marshal(web["labels"])
	if !strings.Contains(string(s), "ob.release") || !strings.Contains(string(s), "20260702-120000-abc1234") {
		t.Fatalf("server missing ob.release label: %s", s)
	}
}

func TestInjectProxyNetwork(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	InjectProxyNetwork(p, cfg, "ob-ingress")
	out, err := Render(p, cfg, "20260702-120000-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	svcs := doc["services"].(map[string]any)

	// role services join the ingress network AND keep default connectivity
	for _, name := range []string{"server", "worker"} {
		svc := svcs[name].(map[string]any)
		nets, ok := svc["networks"]
		if !ok {
			t.Fatalf("%s: no networks rendered", name)
		}
		s, _ := yaml.Marshal(nets)
		if !strings.Contains(string(s), "ob-ingress") || !strings.Contains(string(s), "default") {
			t.Fatalf("%s must be on default + ob-ingress: %s", name, s)
		}
	}
	// accessories and jobs stay untouched
	for _, name := range []string{"postgres", "migrate"} {
		svc := svcs[name].(map[string]any)
		if nets, ok := svc["networks"]; ok {
			if s, _ := yaml.Marshal(nets); strings.Contains(string(s), "ob-ingress") {
				t.Fatalf("%s must not join the ingress network: %s", name, s)
			}
		}
	}
	// the project references the network as external (the proxy project owns it)
	netsAny, ok := doc["networks"].(map[string]any)
	if !ok {
		t.Fatalf("no top-level networks: %v", doc["networks"])
	}
	ing, ok := netsAny["ob-ingress"].(map[string]any)
	if !ok {
		t.Fatalf("ob-ingress not declared: %v", netsAny)
	}
	if ext, _ := ing["external"].(bool); !ext {
		t.Fatalf("ob-ingress must be external: %v", ing)
	}
	if name, _ := ing["name"].(string); name != "ob-ingress" {
		t.Fatalf("ob-ingress must pin its host name: %v", ing)
	}
}
