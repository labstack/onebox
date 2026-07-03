package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/labstack/yeet/internal/release"
)

// HostState is the drift set the plan binds to (design §02): live release id
// + running image ids per service. Not all of docker inspect — that would
// false-positive on every restart.
type HostState struct {
	Host           string            `json:"host"`
	CurrentRelease string            `json:"current_release"`
	ImageIDs       map[string]string `json:"image_ids"`
}

// Artifact is the plan: what was computed, what it was computed against, and
// the exact rendered bytes the apply will ship.
type Artifact struct {
	ID              string            `json:"id"`
	App             string            `json:"app"`
	Env             string            `json:"env"`
	CreatedAt       time.Time         `json:"created_at"`
	GitSHA          string            `json:"git_sha,omitempty"`
	ConfigHash      string            `json:"config_hash"`
	HostState       HostState         `json:"host_state"`
	PinnedImages    map[string]string `json:"pinned_images"`
	RenderedCompose string            `json:"rendered_compose"`
	Commands        []string          `json:"commands"`
}

func HashBytes(b []byte) string {
	return "sha256:" + HashBytesHex(b)
}

func HashBytesHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (a *Artifact) Save(path string) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func LoadArtifact(path string) (*Artifact, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a := &Artifact{}
	if err := json.Unmarshal(b, a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// VerifyBinding refuses the apply when anything the plan was computed
// against has changed: environment, config bytes, or host state.
func (a *Artifact) VerifyBinding(env string, configBytes []byte, fresh HostState) error {
	if env != a.Env {
		return fmt.Errorf("plan was computed for env %q, deploying %q — re-plan", a.Env, env)
	}
	if h := HashBytes(configBytes); h != a.ConfigHash {
		return fmt.Errorf("yeet.yml changed since plan (config hash mismatch) — re-plan")
	}
	if fresh.CurrentRelease != a.HostState.CurrentRelease {
		return fmt.Errorf("host drift: current release is %q, plan saw %q — re-plan",
			fresh.CurrentRelease, a.HostState.CurrentRelease)
	}
	for svc, id := range a.HostState.ImageIDs {
		if got := fresh.ImageIDs[svc]; got != id {
			return fmt.Errorf("host drift: %s runs image %s, plan saw %s — re-plan", svc, got, id)
		}
	}
	return nil
}

// Refresh gathers the drift set from the host. Nothing mutates.
func (e *Engine) Refresh(ctx context.Context) (HostState, error) {
	hs := HostState{Host: e.T.Host(), ImageIDs: map[string]string{}}
	cur, err := release.Current(ctx, e.T, e.Cfg.App)
	if err != nil {
		return hs, err
	}
	hs.CurrentRelease = cur
	svcs := map[string]bool{}
	for _, r := range e.Cfg.Roles {
		svcs[r.Service] = true
	}
	for _, j := range e.Cfg.Jobs {
		svcs[j] = true
	}
	for svc := range svcs {
		id, err := e.containerID(ctx, svc)
		if err != nil {
			return hs, err
		}
		if id == "" {
			continue // not running (jobs usually aren't)
		}
		res, err := e.T.Run(ctx, "docker inspect -f '{{.Image}}' "+id)
		if err != nil {
			return hs, err
		}
		hs.ImageIDs[svc] = strings.TrimSpace(res.Stdout)
	}
	return hs, nil
}

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// PinImages resolves each role/job image tag to a registry digest (host-side
// buildx) and rewrites the project in place. Unresolvable images stay
// tag-bound — stated, not hidden (the fidelity contract).
func (e *Engine) PinImages(ctx context.Context) (map[string]string, error) {
	pins := map[string]string{}
	svcs := map[string]bool{}
	for _, r := range e.Cfg.Roles {
		svcs[r.Service] = true
	}
	for _, j := range e.Cfg.Jobs {
		svcs[j] = true
	}
	for svc := range svcs {
		s, ok := e.Project.Services[svc]
		if !ok || s.Image == "" {
			continue
		}
		res, err := e.T.Run(ctx, "docker buildx imagetools inspect "+q(s.Image)+" --format '{{.Manifest.Digest}}'")
		if err != nil {
			return nil, err
		}
		digest := strings.TrimSpace(res.Stdout)
		if res.ExitCode != 0 || !digestRe.MatchString(digest) {
			e.logf("warn: %s stays unpinned (tag-bound): %s", svc, strings.TrimSpace(res.Stderr))
			pins[svc] = s.Image
			continue
		}
		pinned := withDigest(s.Image, digest)
		s.Image = pinned
		e.Project.Services[svc] = s
		pins[svc] = pinned
	}
	return pins, nil
}

func withDigest(ref, digest string) string {
	name := ref
	if i := strings.LastIndex(name, "@"); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name + "@" + digest
}

// FidelityContract is printed at the top of every plan (design §01/§11: the
// wedge survives honesty; it doesn't survive an overclaim).
const FidelityContract = `Plan fidelity (highest to lowest):
  config       exact — the rendered diff below is what ships, byte for byte
  images       digest-pinned where the registry resolved them; tag-bound otherwise (stated per image)
  choreography the command list below; runtime branches shown as branches
  hooks        verbatim commands — their effects are unplannable`

// Describe renders the choreography as the exact command list with runtime
// branches as branches. Placeholders <old>/<new> resolve at apply time.
func (e *Engine) Describe(remoteCompose string) []string {
	cc := e.composeCmd(remoteCompose)
	var out []string
	hooks := make([]string, 0, len(e.Cfg.Hooks))
	for name := range e.Cfg.Hooks {
		hooks = append(hooks, name)
	}
	sort.Strings(hooks)
	for _, name := range hooks {
		h := e.Cfg.Hooks[name]
		where := "host"
		if h.Local {
			where = "local"
		}
		out = append(out, fmt.Sprintf("hook %s (%s, unplannable): %s", name, where, h.Run))
	}
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		svc := role.Service
		out = append(out, fmt.Sprintf("release %s (%s):", roleName, role.Mode))
		out = append(out, "  "+cc+" pull --quiet "+svc)
		if role.Mode == "rolling" {
			out = append(out,
				fmt.Sprintf("  %s up -d --no-deps --no-recreate --scale %s=2 %s", cc, svc, svc),
				"  wait <new> healthy (ready gate)",
				"    ├─ healthy → converge → docker exec <old> touch /tmp/yeet-drain → wait unhealthy → converge",
				fmt.Sprintf("    │    └─ docker stop -t %d <old> && docker rm <old>", stopGraceSeconds),
				"    └─ unhealthy/timeout → docker rm -f <new>; old keeps serving; deploy halts",
			)
		} else {
			out = append(out,
				fmt.Sprintf("  %s up -d --no-deps --force-recreate %s", cc, svc),
				"  wait ready (or running) — brief gap, stated",
			)
		}
	}
	return out
}
