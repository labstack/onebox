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
		if s, _ := yaml.Marshal(labels); strings.Contains(string(s), "yeet.") {
			t.Fatalf("accessory postgres must not receive yeet labels: %s", s)
		}
	}
	web := svcs["server"].(map[string]any)
	s, _ := yaml.Marshal(web["labels"])
	if !strings.Contains(string(s), "yeet.release") || !strings.Contains(string(s), "20260702-120000-abc1234") {
		t.Fatalf("server missing yeet.release label: %s", s)
	}
}
