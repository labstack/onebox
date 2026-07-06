package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/release"
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
		return fmt.Errorf("ob.yml changed since plan (config hash mismatch) — re-plan")
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
			e.warnf("%s stays unpinned (tag-bound): %s", svc, strings.TrimSpace(res.Stderr))
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
  config       exact — apply re-renders these same bytes and refuses on any drift.
               environment VALUES show as content hashes (secrets never leave the host)
  images       digest-pinned where the registry resolved them; tag-bound otherwise (stated per image)
  choreography the command list below; runtime branches shown as branches
  hooks        verbatim commands — their effects are unplannable`

// releaseLabelLine matches the one rendered line that changes on EVERY
// deploy by construction: the ob.release stamp.
var releaseLabelLine = regexp.MustCompile(`(?m)^\s*ob\.release: \S+\n?`)

// OnlyReleaseLabelsChanged reports whether two rendered composes are
// byte-identical once the ob.release label lines are removed — i.e. the
// planned deploy has no material change, only a new release identity. Used
// by the plan to say "nothing changed" plainly instead of encoding it as
// label-noise hunks. Empty inputs (first deploy) compare honestly: an empty
// live side is a real change.
func OnlyReleaseLabelsChanged(live, planned string) bool {
	return releaseLabelLine.ReplaceAllString(live, "") == releaseLabelLine.ReplaceAllString(planned, "")
}

// LocalPayloadDigest hashes everything in a staging dir EXCEPT compose.yaml
// (that's compared label-invariantly) — env files, secrets, snapshot, bind
// sources. Same line format and ordering as the remote shell pipeline in
// RemotePayloadDigest, so equal digests mean byte-equal payloads.
func LocalPayloadDigest(dir string) (string, error) {
	var lines []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "compose.yaml" || (strings.HasPrefix(rel, ".job-") && strings.HasSuffix(rel, "-result")) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%x  ./%s\n", sha256.Sum256(b), rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(lines) // LC_ALL=C sort equivalent: bytewise on the whole line
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return fmt.Sprintf("%x", sum), nil
}

// RemotePayloadDigest computes the same digest over a release dir on the
// host: per-file sha256 lines, bytewise-sorted, hashed together.
func (e *Engine) RemotePayloadDigest(ctx context.Context, releaseID string) (string, error) {
	dir := release.PathsFor(e.Cfg.App).Releases + "/" + releaseID
	cmd := "cd " + q(dir) + " && find . -type f ! -name compose.yaml ! -name '.job-*-result' -exec sha256sum {} + 2>/dev/null | LC_ALL=C sort | sha256sum | cut -d' ' -f1"
	res, err := e.T.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(res.Stdout), "-")), nil
}

// deploySeam is the set of hook names a deploy runs (see RunHook calls in
// deploy.go). Hooks keyed by any other name — e.g. bootstrap — belong to a
// different lifecycle and never run during a deploy.
var deploySeam = map[string]bool{"pre_release": true, "post_release": true, "post_deploy": true}

// Describe renders the choreography as the exact command list with runtime
// branches as branches. Placeholders <old>/<new> resolve at apply time.
func (e *Engine) Describe(remoteCompose string) []string {
	cc := e.composeCmd(remoteCompose)
	var out []string

	// Jobs run first, gated. A job with a same-named hook uses that command;
	// otherwise it auto-runs `compose run --rm`.
	steps := e.gateSteps()
	isStep := map[string]bool{}
	for _, job := range steps {
		isStep[job] = true
		cmdStr := cc + " run --rm --no-deps " + job
		if h, ok := e.Cfg.Hooks[job]; ok && h.Run != "" {
			cmdStr = h.Run
		}
		out = append(out, fmt.Sprintf("job %s (gated — changed=false keeps rollback open): %s", job, cmdStr))
	}

	// Only the hooks a deploy actually runs belong in a deploy plan; bootstrap
	// is a separate lifecycle (ob bootstrap), so listing it here would claim a
	// step that never runs — a fidelity violation.
	hooks := make([]string, 0, len(e.Cfg.Hooks))
	for name := range e.Cfg.Hooks {
		if isStep[name] || !deploySeam[name] {
			continue // shown as a job above, or not a deploy-lifecycle hook
		}
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
		head := fmt.Sprintf("release %s (%s", roleName, role.Mode)
		if n := role.Count(); n > 1 {
			head += fmt.Sprintf(", %d replicas → %s-1..%s-%d", n, svc, svc, n)
		}
		out = append(out, head+"):")
		out = append(out, "  "+cc+" pull --quiet "+svc)
		if role.Mode == "rolling" {
			step := "  per replica: "
			out = append(out,
				step+fmt.Sprintf("%s up -d --no-deps --no-recreate --scale %s=<+1> %s", cc, svc, svc),
				"  wait <new> healthy (ready gate)",
				"    ├─ healthy → converge → docker exec <old> touch /tmp/ob-drain → wait unhealthy → converge",
				fmt.Sprintf("    │    └─ docker stop -t %d <old> && docker rm <old> && rename <new> into the freed slot", role.StopGraceSeconds()),
				"    └─ unhealthy/timeout → docker rm -f <new>; existing keep serving; deploy halts",
			)
		} else {
			scale := ""
			if n := role.Count(); n > 1 {
				scale = fmt.Sprintf(" --scale %s=%d", svc, n)
			}
			out = append(out,
				fmt.Sprintf("  %s up -d --no-deps --force-recreate%s %s", cc, scale, svc),
				"  wait ready (or running) — brief gap, stated",
			)
		}
	}
	return out
}
