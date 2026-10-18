package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

func testProxyCompose(t *testing.T, e *Engine) []byte {
	t.Helper()
	dir := t.TempDir()
	if _, err := e.stageProxy(dir); err != nil {
		t.Fatal(err)
	}
	rendered, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}

func TestEnsureProxyApplyRestoresDiscoveryWhenRecreateFails(t *testing.T) {
	for _, fallbackFails := range []bool{false, true} {
		t.Run(fmt.Sprint(fallbackFails), func(t *testing.T) {
			f := &transport.Fake{}
			e, _, out := proxyFixture(t, f)
			ps := proxyPS(f, true)
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch {
				case strings.Contains(cmd, "up -d --force-recreate discovery"):
					return transport.Result{ExitCode: 1, Stderr: "recreate failed"}, true
				case strings.Contains(cmd, "docker start onebox-discovery"):
					return transport.Result{ExitCode: 1, Stderr: "container no longer exists"}, true
				case strings.Contains(cmd, "up -d discovery"):
					if fallbackFails {
						return transport.Result{ExitCode: 1, Stderr: "fallback up failed"}, true
					}
					return transport.Result{}, true
				}
				return ps(cmd)
			}
			err := e.EnsureProxy(context.Background(), "failed-apply", false)
			seq := strings.Join(f.Commands, "\n")
			recreate, start, fallback := strings.Index(seq, "up -d --force-recreate discovery"), strings.Index(seq, "docker start onebox-discovery"), strings.LastIndex(seq, "up -d discovery")
			if err == nil || !strings.Contains(err.Error(), "recreate failed") || recreate < 0 || start < recreate || fallback < start {
				t.Fatalf("failed recreate must start, then recreate a missing discovery container: %v\n%s", err, seq)
			}
			if fallbackFails && (!strings.Contains(out.String(), "fallback up failed") || strings.Contains(err.Error(), "fallback up failed")) {
				t.Fatalf("fallback failure must be logged without replacing the apply error: %v\n%s", err, out)
			}
		})
	}
}

func TestEnsureProxyApplyPullsDiscoveryBeforeRecreate(t *testing.T) {
	for _, pullFails := range []bool{false, true} {
		t.Run(fmt.Sprint(pullFails), func(t *testing.T) {
			f := &transport.Fake{}
			e, _, _ := proxyFixture(t, f)
			e.Opts.Runner.Version = "v1.2.3"
			image := q(proxy.DiscoveryImage(e.Opts.Runner.Version))
			ensureImage := "docker image inspect " + image + " >/dev/null 2>&1 || docker pull " + image
			ps := proxyPS(f, true)
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, ensureImage) {
					if pullFails {
						return transport.Result{ExitCode: 1, Stderr: "discovery pull failed"}, true
					}
					return transport.Result{}, true
				}
				return ps(cmd)
			}
			err := e.EnsureProxy(context.Background(), "apply-pull", false)
			seq := strings.Join(f.Commands, "\n")
			pull, recreate := strings.Index(seq, ensureImage), strings.Index(seq, "up -d --force-recreate discovery")
			if pull < 0 {
				t.Fatalf("apply must make the discovery image available:\n%s", seq)
			}
			if pullFails {
				if err == nil || !strings.Contains(err.Error(), "discovery pull failed") || recreate >= 0 {
					t.Fatalf("failed pull must precede any recreate: %v\n%s", err, seq)
				}
			} else if err != nil || recreate < pull {
				t.Fatalf("apply must pull before recreating discovery: %v\n%s", err, seq)
			}
		})
	}
}

func TestEnsureProxyRefusesPlannedActionDrift(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh=%v", fresh), func(t *testing.T) {
			f := &transport.Fake{}
			e, applied, _ := proxyFixture(t, f)
			e.Opts.ExpectedProxyAction = "unchanged"
			rendered := string(testProxyCompose(t, e))
			applied.Config = "sha256:" + strings.Repeat("a", 64)
			ps := proxyPS(f, !fresh)
			observed := false
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch cmd {
				case "cat '/var/lib/onebox/_host/proxy/config.hash'":
					if fresh {
						return transport.Result{ExitCode: 1}, true
					}
					return transport.Result{Stdout: applied.String()}, true
				case "cat '/var/lib/onebox/_host/proxy/compose.yaml'":
					if fresh {
						return transport.Result{ExitCode: 1}, true
					}
					return transport.Result{Stdout: rendered}, true
				case outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml"):
					observed = true
					if fresh {
						return transport.Result{}, true
					}
					return transport.Result{Stdout: "12"}, true
				}
				return ps(cmd)
			}
			err := e.EnsureProxy(context.Background(), "proxy-drift", false)
			current := "restart Traefik (configuration changed)"
			if fresh {
				current = "start Traefik"
			}
			if err == nil || !observed || !strings.Contains(err.Error(), "unchanged") || !strings.Contains(err.Error(), current) || !strings.Contains(err.Error(), "re-plan") {
				t.Fatalf("proxy action drift must name both plan lines and re-plan guidance: %v", err)
			}
			seq := strings.Join(f.Commands, "\n")
			for _, forbidden := range []string{"/proxy/acme", "-name '.staged-*'", "config.hash.tmp", "rm -f '/var/lib/onebox/_host/proxy/config.hash'", "docker pull", "up -d", " stop discovery"} {
				if strings.Contains(seq, forbidden) {
					t.Fatalf("refused plan mutated the host, found %s:\n%s", forbidden, seq)
				}
			}
			if len(f.Uploads) != 0 {
				t.Fatalf("refused plan uploaded proxy configuration: %v", f.Uploads)
			}
		})
	}
}

func TestObserveProxyFlagsRepulledTraefikImage(t *testing.T) {
	for _, configured := range []string{"", "registry.example.com/traefik:custom"} {
		for _, state := range []string{"equal", "different", "missing"} {
			t.Run(configured+"/"+state, func(t *testing.T) {
				f := &transport.Fake{}
				e, id, _ := proxyFixture(t, f)
				e.Spec.Proxy.Image = configured
				rendered := testProxyCompose(t, e)
				id.Compose = HashBytes(rendered)
				image := configured
				if image == "" {
					image = proxy.DefaultImage
				}
				ps := proxyPS(f, true)
				inspectedContainer, inspectedImage := false, false
				f.Dynamic = func(cmd string) (transport.Result, bool) {
					switch {
					case cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'":
						return transport.Result{Stdout: id.String()}, true
					case cmd == "cat '/var/lib/onebox/_host/proxy/compose.yaml'":
						return transport.Result{Stdout: string(rendered)}, true
					case strings.HasPrefix(cmd, "docker inspect -f '{{.Image}}'"):
						inspectedContainer = true
						return transport.Result{Stdout: "sha256:running\n"}, true
					case strings.HasPrefix(cmd, "docker image inspect -f '{{.Id}}'"):
						inspectedImage = true
						if !strings.HasSuffix(cmd, q(image)) {
							t.Fatalf("inspected the wrong configured image: %s", cmd)
						}
						if state == "missing" {
							return transport.Result{ExitCode: 1}, true
						}
						digest := "sha256:running\n"
						if state == "different" {
							digest = "sha256:repulled\n"
						}
						return transport.Result{Stdout: digest}, true
					}
					return ps(cmd)
				}
				o, err := e.observeProxy(context.Background(), rendered)
				if err != nil || !inspectedContainer || !inspectedImage || o.TraefikChanged != (state != "equal") {
					t.Fatalf("repulled image observation = %+v, %v", o, err)
				}
				want := proxyUnchanged
				if state != "equal" {
					want = proxyReplace
				}
				if got := decideProxy(o, id); got != want {
					t.Fatalf("image drift action = %v, want %v", got, want)
				}
			})
		}
	}
}

func runProxyStagingCleanup(t *testing.T, command, dir string) {
	t.Helper()
	start := strings.Index(command, "find '/var/lib/onebox/_host/proxy'")
	if start < 0 {
		t.Fatalf("no staging cleanup: %s", command)
	}
	script, _, ok := strings.Cut(command[start:], "; else echo onebox-host-fenced")
	if !ok {
		t.Fatalf("staging cleanup must hold the host lock: %s", command)
	}
	script = strings.ReplaceAll(script, q("/var/lib/onebox/_host/proxy"), q(dir))
	if output, err := exec.CommandContext(t.Context(), "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("staging cleanup: %v\n%s", err, output)
	}
}

func TestEnsureProxyRemovesFailedUploadsButKeepsRetired(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".staged-upload", ".staged-config/retired", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &transport.Fake{}
	e, _, _ := proxyFixture(t, f)
	ps := proxyPS(f, true)
	cleaned := false
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "-name '.staged-*'") {
			runProxyStagingCleanup(t, cmd, dir)
			cleaned = true
			return transport.Result{}, true
		}
		if strings.Contains(cmd, "mkdir -p '/var/lib/onebox/_host/proxy/acme'") {
			return transport.Result{ExitCode: 1, Stderr: "cannot prepare directories"}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "clean-upload", false); err == nil {
		t.Fatal("failed directory preparation must error")
	}
	if _, err := os.Stat(filepath.Join(dir, ".staged-upload")); !cleaned || !os.IsNotExist(err) {
		t.Fatalf("failed upload staging survived cleanup: cleaned=%v err=%v", cleaned, err)
	}
	for _, name := range []string{".staged-config/retired", "unrelated"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("startup cleanup removed protected %s: %v", name, err)
		}
	}
}

func TestEnsureProxyFailedReplaceIsPlannedAsReplace(t *testing.T) {
	f := &transport.Fake{}
	e, staged, _ := proxyFixture(t, f)
	rendered := string(testProxyCompose(t, e))
	live := strings.Replace(rendered, "image: ", "image: old-", 1)
	applied := staged
	applied.Compose = "sha256:" + strings.Repeat("a", 64)
	pending := proxy.Identity{Config: applied.Config, Compose: "sha256:" + strings.Repeat("0", 63) + "1"}
	oldPending := proxy.Identity{Config: applied.Config, Compose: pendingComposeDigest}
	marker := applied.String()
	fail := true
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'":
			return transport.Result{Stdout: marker}, true
		case strings.HasPrefix(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'"):
			return transport.Result{Stdout: live}, true
		case strings.Contains(cmd, "/compose.yaml' '/var/lib/onebox/_host/proxy/compose.yaml'"):
			live = rendered
			return transport.Result{}, true
		case strings.Contains(cmd, "up -d discovery") && fail:
			return transport.Result{ExitCode: 1, Stderr: "cannot start discovery"}, true
		}
		for _, id := range []proxy.Identity{pending, oldPending, staged} {
			if strings.Contains(cmd, testProxyIdentityWrite(id)) {
				marker = id.String()
				return transport.Result{}, true
			}
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "failed-replace", false); err == nil {
		t.Fatal("failed discovery up must error")
	}
	if marker != pending.String() || live != rendered || strings.Contains(strings.Join(f.Commands, "\n"), testProxyIdentityWrite(staged)) {
		t.Errorf("failed replacement must leave the new Compose and only its pending marker: %q", marker)
	}
	line, err := e.PlanProxy(context.Background())
	if err != nil || line != "recreate Traefik (its container definition changed)" {
		t.Fatalf("failed replacement plan = %q, %v", line, err)
	}
	beforeRetry := len(f.Commands)
	fail = false
	if err := e.EnsureProxy(context.Background(), "retry-replace", false); err != nil {
		t.Fatal(err)
	}
	retry := strings.Join(f.Commands[beforeRetry:], "\n")
	if !strings.Contains(retry, testProxyIdentityWrite(pending)) || marker != staged.String() ||
		strings.Contains(retry, "docker restart") || strings.Contains(retry, "/retired/") {
		t.Fatalf("retry must remain a replacement before committing the final identity:\n%s", retry)
	}
}

func TestEnsureProxyPullsDiscoveryBeforeStopping(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, pullFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("replace=%v/pullFails=%v", replace, pullFails), func(t *testing.T) {
				f := &transport.Fake{}
				e, applied, _ := proxyFixture(t, f)
				e.Opts.Runner.Version = "v1.2.3"
				live := string(testProxyCompose(t, e))
				if replace {
					live = strings.Replace(live, "image: ", "image: old-", 1)
				}
				applied.Compose = "sha256:" + strings.Repeat("a", 64)
				image := proxy.DiscoveryImage(e.Opts.Runner.Version)
				ensureImage := "docker image inspect " + q(image) + " >/dev/null 2>&1 || docker pull " + q(image)
				ps := proxyPS(f, true)
				f.Dynamic = func(cmd string) (transport.Result, bool) {
					switch {
					case cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'":
						return transport.Result{Stdout: applied.String()}, true
					case strings.HasPrefix(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'"):
						return transport.Result{Stdout: live}, true
					case strings.Contains(cmd, ensureImage):
						if pullFails {
							return transport.Result{ExitCode: 1, Stderr: "cannot pull discovery"}, true
						}
						return transport.Result{}, true // missing image was pulled
					}
					return ps(cmd)
				}
				err := e.EnsureProxy(context.Background(), "pull-discovery", false)
				seq := strings.Join(f.Commands, "\n")
				pull, stop := strings.Index(seq, ensureImage), strings.Index(seq, " stop discovery")
				if pull < 0 {
					t.Fatalf("discovery image must be available before stopping:\n%s", seq)
				}
				if pullFails {
					if err == nil || !strings.Contains(err.Error(), "cannot pull discovery") || stop >= 0 || strings.Contains(seq, "up -d discovery") || strings.Contains(seq, "docker start onebox-discovery") {
						t.Fatalf("failed pull must return before stopping discovery: %v\n%s", err, seq)
					}
				} else if err != nil || stop < pull || strings.Contains(seq, "docker start onebox-discovery") {
					t.Fatalf("pull must precede stop without a fallback start on success: %v\n%s", err, seq)
				}
			})
		}
	}
}

func TestEnsureProxyRestartsDiscoveryAfterFailedUp(t *testing.T) {
	for _, step := range []string{"up", "transport", "inode", "publication", "journal", "start fails"} {
		t.Run(step, func(t *testing.T) {
			f := &transport.Fake{}
			e, applied, out := proxyFixture(t, f)
			live := string(testProxyCompose(t, e))
			applied.Compose = "sha256:" + strings.Repeat("a", 64)
			ps := proxyPS(f, true)
			stopped, startAttempted := false, false
			transportErr := errors.New("discovery transport failed")
			if step == "transport" {
				f.Err = func(cmd string) error {
					if strings.Contains(cmd, "up -d discovery") {
						return transportErr
					}
					return nil
				}
			}
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch {
				case cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'":
					return transport.Result{Stdout: applied.String()}, true
				case strings.HasPrefix(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'"):
					return transport.Result{Stdout: live}, true
				case strings.Contains(cmd, " stop discovery"):
					stopped = true
					return transport.Result{}, true
				case strings.Contains(cmd, "docker start onebox-discovery"):
					startAttempted = true
					if step == "start fails" {
						return transport.Result{ExitCode: 1, Stderr: "cannot restart discovery"}, true
					}
					return transport.Result{}, true
				case strings.Contains(cmd, "up -d discovery") && (step == "up" || step == "start fails"):
					return transport.Result{ExitCode: 1, Stderr: "cannot start discovery"}, true
				case cmd == outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml") && stopped && step == "inode":
					return transport.Result{ExitCode: 1, Stderr: "cannot read inode"}, true
				case strings.HasPrefix(cmd, "i=0;") && step == "publication":
					return transport.Result{ExitCode: 1}, true
				case strings.Contains(cmd, "\"phase\":\"proxy-apply\",\"event\":\"finish\"") && step == "journal":
					return transport.Result{ExitCode: 1, Stderr: "cannot finish journal"}, true
				}
				return ps(cmd)
			}
			err := e.EnsureProxy(context.Background(), "restart-discovery", false)
			if err == nil || !stopped || !startAttempted {
				t.Fatalf("error after stop must restart discovery before returning: %v\n%s", err, strings.Join(f.Commands, "\n"))
			}
			if step == "transport" && !errors.Is(err, transportErr) {
				t.Fatalf("lost original transport error: %v", err)
			}
			if step == "start fails" && (!strings.Contains(out.String(), "cannot restart discovery") || strings.Contains(err.Error(), "cannot restart discovery") || !strings.Contains(err.Error(), "cannot start discovery")) {
				t.Fatalf("fallback failure must be logged without replacing the original error: %v\n%s", err, out)
			}
		})
	}
}

func TestEnsureProxyUnchangedCleansStaleStaging(t *testing.T) {
	f := &transport.Fake{}
	e, id, _ := proxyFixture(t, f)
	rendered := string(testProxyCompose(t, e))
	ps := proxyPS(f, true)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch cmd {
		case "cat '/var/lib/onebox/_host/proxy/config.hash'":
			return transport.Result{Stdout: id.String()}, true
		case "cat '/var/lib/onebox/_host/proxy/compose.yaml'":
			return transport.Result{Stdout: rendered}, true
		}
		return ps(cmd)
	}
	if err := e.EnsureProxy(context.Background(), "unchanged-cleanup", false); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "-name '.staged-*' -exec rm -rf -- {} +") {
		t.Fatalf("unchanged proxy must clean stale staging:\n%s", seq)
	}
	if len(f.Uploads) != 0 || strings.Contains(seq, "up -d") || strings.Contains(seq, "docker restart") {
		t.Fatalf("unchanged proxy must only clean stale staging:\n%s", seq)
	}
}

func TestTraefikDefinitionChanged(t *testing.T) {
	live := "services:\n  proxy:\n    image: traefik:v3.6\n    ports: [80:80]\n  discovery:\n    image: discovery:old\nnetworks:\n  proxy:\n    external: true\n"
	for _, tt := range []struct {
		name     string
		live     string
		rendered string
		want     bool
	}{
		{"same", live, live, false},
		{"discovery image", live, strings.Replace(live, "discovery:old", "discovery:new", 1), false},
		{"traefik image", live, strings.Replace(live, "traefik:v3.6", "traefik:v3.7", 1), true},
		{"ports", live, strings.Replace(live, "80:80", "8080:80", 1), true},
		{"networks", live, strings.Replace(live, "external: true", "external: false", 1), true},
		{"formatting", live, "# compose\n" + strings.Replace(live, "ports: [80:80]", "ports:\n      - 80:80", 1), false},
		{"invalid live", "services: [", live, true},
		{"invalid rendered", live, "services: [", true},
		{"empty live", "", live, true},
		{"blank live", " \n", live, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := traefikDefinitionChanged([]byte(tt.live), []byte(tt.rendered)); got != tt.want {
				t.Fatalf("traefikDefinitionChanged = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecideProxyNamesTraefikReplacement(t *testing.T) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	staged := proxy.Identity{Config: digestA, Compose: digestB}
	o := proxyObservation{
		Applied: proxy.Identity{Config: digestA, Compose: digestA}, AppliedKnown: true,
		ProxyIDs: []string{"PX1"}, DiscoveryIDs: []string{"D1"}, OutputInode: "12", TraefikChanged: true,
	}
	if got := decideProxy(o, staged); got != proxyReplace {
		t.Fatalf("decideProxy = %v, want %v", got, proxyReplace)
	}
	if got := proxyReplace.String(); got != "recreate Traefik (its container definition changed)" {
		t.Fatalf("proxyReplace = %q", got)
	}
	o.AppliedKnown = false
	if got := decideProxy(o, staged); got != proxyApply {
		t.Fatalf("unknown config must still apply: %v", got)
	}
}

func TestPlanProxySaysStartWhenTraefikMissing(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			f := &transport.Fake{}
			e, id, _ := proxyFixture(t, f)
			rendered := testProxyCompose(t, e)
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch {
				case cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'":
					if known {
						return transport.Result{Stdout: id.String()}, true
					}
					return transport.Result{ExitCode: 1}, true
				case cmd == "cat '/var/lib/onebox/_host/proxy/compose.yaml'":
					return transport.Result{Stdout: string(rendered)}, true
				case strings.Contains(cmd, "service=discovery"):
					return transport.Result{Stdout: "D1"}, true
				case strings.Contains(cmd, "stat -c %i"):
					return transport.Result{Stdout: "12"}, true
				}
				return transport.Result{}, false
			}
			got, err := e.PlanProxy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != "start Traefik" {
				t.Fatalf("PlanProxy = %q, want start Traefik", got)
			}
		})
	}
}

func TestObserveProxyTraefikDefinition(t *testing.T) {
	for _, tt := range []struct {
		name string
		live transport.Result
		want bool
	}{
		{"same", transport.Result{Stdout: "services:\n  proxy:\n    image: traefik:v3.6\n"}, false},
		{"changed", transport.Result{Stdout: "services:\n  proxy:\n    image: traefik:v3.7\n"}, true},
		{"unreadable", transport.Result{ExitCode: 1}, true},
		{"failed read with content", transport.Result{ExitCode: 1, Stdout: "services:\n  proxy:\n    image: traefik:v3.6\n"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if cmd == "cat '/var/lib/onebox/_host/proxy/compose.yaml'" {
					return tt.live, true
				}
				return transport.Result{}, false
			}}
			e, _, _ := proxyFixture(t, f)
			o, err := e.observeProxy(context.Background(), []byte("services:\n  proxy:\n    image: traefik:v3.6\n"))
			if err != nil {
				t.Fatal(err)
			}
			if o.TraefikChanged != tt.want {
				t.Fatalf("TraefikChanged = %v, want %v", o.TraefikChanged, tt.want)
			}
		})
	}
}

func TestEnsureProxyReplacementUsesConvergePath(t *testing.T) {
	f := &transport.Fake{}
	e, staged, _ := proxyFixture(t, f)
	rendered := testProxyCompose(t, e)
	applied := staged
	applied.Compose = "sha256:" + strings.Repeat("a", 64)
	live := strings.Replace(string(rendered), "image: ", "image: old-", 1)
	ps := proxyPS(f, true)
	started := false
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'":
			return transport.Result{Stdout: applied.String()}, true
		case strings.HasPrefix(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'"):
			return transport.Result{Stdout: live}, true
		case strings.Contains(cmd, "up -d proxy"):
			started = true
			return transport.Result{}, true
		case strings.Contains(cmd, "service=proxy") && started:
			return transport.Result{Stdout: "PX2"}, true
		case strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "PX2"):
			return transport.Result{Stdout: "healthy"}, true
		}
		return ps(cmd)
	}
	inspected := false
	e.T = &proxyUploadObserver{Fake: f, inspect: func(dir string) {
		inspected = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "compose.yaml" {
			t.Fatalf("replacement upload must hold only compose.yaml: %v", entries)
		}
	}}
	if err := e.EnsureProxy(context.Background(), "replace-proxy", false); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	pending := proxy.Identity{Config: applied.Config, Compose: pendingReplaceDigest}
	iPending := strings.Index(seq, testProxyIdentityWrite(pending))
	iStop := strings.Index(seq, " stop discovery")
	iSample := strings.LastIndex(seq, "\n"+outputInodeCommand("/var/lib/onebox/_host/proxy/dynamic/onebox.yml")+"\n")
	iUp := strings.Index(seq, " up -d discovery")
	iWait := strings.Index(seq, "i=0;")
	iProxy := strings.Index(seq, " up -d proxy")
	iHealth := strings.Index(seq, "docker inspect -f '{{if .State.Health}}")
	iFinal := strings.Index(seq, testProxyIdentityWrite(staged))
	if !inspected || iPending < 0 || iStop < iPending || iSample < iStop || iUp < iSample || iWait < iUp || iProxy < iWait || iHealth < iProxy || iFinal < iHealth {
		t.Fatalf("replacement must converge discovery and wait for the new proxy before committing:\n%s", seq)
	}
	for _, forbidden := range []string{"rm -f '/var/lib/onebox/_host/proxy/config.hash'", "/retired/", "docker restart", "--force-recreate"} {
		if strings.Contains(seq, forbidden) {
			t.Fatalf("replacement must use the converge path, found %s:\n%s", forbidden, seq)
		}
	}
}

func TestDecideProxy(t *testing.T) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	digestC := "sha256:" + strings.Repeat("c", 64)
	staged := proxy.Identity{Config: digestA, Compose: digestB}
	tests := []struct {
		name   string
		mutate func(*proxyObservation)
		want   proxyAction
	}{
		{"unknown marker", func(o *proxyObservation) { o.AppliedKnown = false }, proxyApply},
		{"config drift", func(o *proxyObservation) { o.Applied.Config = digestC }, proxyApply},
		{"compose drift", func(o *proxyObservation) { o.Applied.Compose = digestC }, proxyConverge},
		{"traefik missing", func(o *proxyObservation) { o.ProxyIDs = nil }, proxyConverge},
		{"discovery missing", func(o *proxyObservation) { o.DiscoveryIDs = nil }, proxyConverge},
		{"output missing", func(o *proxyObservation) { o.OutputInode = "" }, proxyConverge},
		{"all present and equal", func(o *proxyObservation) {}, proxyUnchanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := proxyObservation{Applied: staged, AppliedKnown: true, ProxyIDs: []string{"PX1"}, DiscoveryIDs: []string{"D1"}, OutputInode: "12"}
			tt.mutate(&o)
			if got := decideProxy(o, staged); got != tt.want {
				t.Fatalf("decideProxy = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestObserveProxyIsReadOnly(t *testing.T) {
	f := &transport.Fake{}
	e, staged, _ := proxyFixture(t, f)
	rendered := testProxyCompose(t, e)
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "{{.Image}}") || strings.Contains(cmd, "{{.Id}}"):
			return transport.Result{Stdout: "sha256:proxy"}, true
		case cmd == "cat '/var/lib/onebox/_host/proxy/compose.yaml'":
			return transport.Result{Stdout: string(rendered)}, true
		case strings.HasPrefix(cmd, "cat "):
			return transport.Result{Stdout: staged.String() + "\n"}, true
		case strings.Contains(cmd, "service=proxy"):
			return transport.Result{Stdout: "PX1\n"}, true
		case strings.Contains(cmd, "service=discovery"):
			return transport.Result{Stdout: "D1\n"}, true
		case strings.Contains(cmd, "stat -c %i"):
			return transport.Result{Stdout: "12\n"}, true
		}
		return transport.Result{}, false
	}
	o, err := e.observeProxy(context.Background(), rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !o.AppliedKnown || o.Applied != staged || strings.Join(o.ProxyIDs, ",") != "PX1" || strings.Join(o.DiscoveryIDs, ",") != "D1" || o.OutputInode != "12" {
		t.Fatalf("observation = %+v", o)
	}
	if o.TraefikChanged {
		t.Fatal("matching Traefik definition was reported changed")
	}
	for _, cmd := range f.Commands {
		for _, mutation := range []string{"rm ", "mv ", "mkdir", "up -d", "docker restart", "> "} {
			if strings.Contains(cmd, mutation) {
				t.Fatalf("observe mutated host: %s", cmd)
			}
		}
	}
	if len(f.Commands) != 7 || len(f.Uploads) != 0 {
		t.Fatalf("commands = %v; uploads = %v", f.Commands, f.Uploads)
	}
}

func TestObserveProxyMarker(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, marker := range []string{"", digest, digest + " sha256:abc", "unparseable"} {
		t.Run(marker, func(t *testing.T) {
			f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
				if strings.HasPrefix(cmd, "cat ") {
					return transport.Result{Stdout: marker}, true
				}
				return transport.Result{}, false
			}}
			e, _, _ := proxyFixture(t, f)
			o, err := e.observeProxy(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if o.AppliedKnown {
				t.Fatalf("invalid marker accepted: %+v", o)
			}
			if o.OutputInode != "" {
				t.Fatalf("absent output inode = %q", o.OutputInode)
			}
		})
	}
}

func TestProxyActionString(t *testing.T) {
	for action, want := range map[proxyAction]string{
		proxyUnchanged: "unchanged",
		proxyConverge:  "update changed containers only, no restart",
		proxyReplace:   "recreate Traefik (its container definition changed)",
		proxyApply:     "restart Traefik (configuration changed)",
	} {
		if got := action.String(); got != want {
			t.Fatalf("action %d = %q, want %q", action, got, want)
		}
	}
}

func TestOutputInodeCommand(t *testing.T) {
	const path = "/proxy with spaces/onebox.yml"
	want := "if [ -s '/proxy with spaces/onebox.yml' ]; then stat -c %i '/proxy with spaces/onebox.yml'; fi"
	if got := outputInodeCommand(path); got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestStageProxy(t *testing.T) {
	e, want, _ := proxyFixture(t, &transport.Fake{})
	got, err := e.stageProxy(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

func TestPlanProxyReportsEachAction(t *testing.T) {
	for _, tt := range []struct {
		name string
		want string
	}{
		{"unchanged", "unchanged"},
		{"compose drift", "update changed containers only, no restart"},
		{"traefik drift", "recreate Traefik (its container definition changed)"},
		{"config drift", "restart Traefik (configuration changed)"},
		{"unmanaged", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &transport.Fake{}
			e, applied, _ := proxyFixture(t, f)
			live := string(testProxyCompose(t, e))
			switch tt.name {
			case "compose drift":
				applied.Compose = "sha256:" + strings.Repeat("c", 64)
				live = strings.Replace(live, proxy.DiscoveryImage("dev"), "discovery:old", 1)
			case "traefik drift":
				applied.Compose = "sha256:" + strings.Repeat("c", 64)
				live = strings.Replace(live, "image: ", "image: old-", 1)
			case "config drift":
				applied.Config = "sha256:" + strings.Repeat("c", 64)
			case "unmanaged":
				e.Spec.Proxy.Managed = false
			}
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch {
				case strings.Contains(cmd, "{{.Image}}") || strings.Contains(cmd, "{{.Id}}"):
					return transport.Result{Stdout: "sha256:proxy"}, true
				case cmd == "cat '/var/lib/onebox/_host/proxy/compose.yaml'":
					return transport.Result{Stdout: live}, true
				case strings.HasPrefix(cmd, "cat "):
					return transport.Result{Stdout: applied.String() + "\n"}, true
				case strings.Contains(cmd, "service=proxy"):
					return transport.Result{Stdout: "PX1\n"}, true
				case strings.Contains(cmd, "service=discovery"):
					return transport.Result{Stdout: "D1\n"}, true
				case strings.Contains(cmd, "stat -c %i"):
					return transport.Result{Stdout: "12\n"}, true
				}
				return transport.Result{}, false
			}
			got, err := e.PlanProxy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("PlanProxy = %q, want %q", got, tt.want)
			}
			for _, cmd := range f.Commands {
				for _, mutation := range []string{"rm ", "mv ", "mkdir", "up -d", "docker restart", "> "} {
					if strings.Contains(cmd, mutation) {
						t.Fatalf("plan mutated host: %s", cmd)
					}
				}
			}
			wantCommands := 7
			if tt.name == "unmanaged" {
				wantCommands = 0
			}
			if len(f.Commands) != wantCommands || len(f.Uploads) != 0 {
				t.Fatalf("commands = %v; uploads = %v", f.Commands, f.Uploads)
			}
		})
	}
}
