package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseIdentityRoundTrip(t *testing.T) {
	id := Identity{Config: "sha256:" + strings.Repeat("a", 64), Compose: "sha256:" + strings.Repeat("b", 64)}
	got, ok := ParseIdentity(id.String() + "\n")
	if !ok || got != id {
		t.Fatalf("got %+v %v", got, ok)
	}
}

func TestParseIdentityRejectsLegacyAndGarbage(t *testing.T) {
	a := "sha256:" + strings.Repeat("a", 64)
	for _, s := range []string{"", "deadbeef", a, a + " " + a + " " + a, a + " sha256:XYZ", strings.Repeat("a", 64) + " " + a} {
		if _, ok := ParseIdentity(s); ok {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestStageSeparatesComposeFromConfig(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "managed", true: "custom"}[custom], func(t *testing.T) {
			cfgDir := ""
			if custom {
				cfgDir = writeCfg(t, map[string]string{"traefik.yml": testSocketlessStatic})
			}
			a, err := StageForApp(cfgDir, t.TempDir(), "", DiscoveryImage("v2026.10.4-alpha"), "onebox", "", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			b, err := StageForApp(cfgDir, t.TempDir(), "", DiscoveryImage("v2026.10.5-alpha"), "onebox", "", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if a.Config != b.Config || a.Compose == b.Compose {
				t.Fatalf("compose change: %+v -> %+v", a, b)
			}
			if got, ok := ParseIdentity(a.String()); !ok || got != a {
				t.Fatalf("invalid staged identity: %+v", a)
			}
		})
	}
}

func TestStageConfigChangeMovesOnlyConfig(t *testing.T) {
	cfgDir := writeCfg(t, map[string]string{"traefik.yml": testSocketlessStatic})
	a, err := StageForApp(cfgDir, t.TempDir(), "", DiscoveryImage("v2026.10.4-alpha"), "onebox", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "traefik.yml"), []byte(testSocketlessStatic+"log:\n  level: DEBUG\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := StageForApp(cfgDir, t.TempDir(), "", DiscoveryImage("v2026.10.4-alpha"), "onebox", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Config == b.Config || a.Compose != b.Compose {
		t.Fatalf("config change: %+v -> %+v", a, b)
	}
}
