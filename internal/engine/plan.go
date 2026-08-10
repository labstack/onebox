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

	"github.com/labstack/onebox/internal/imageref"
	"github.com/labstack/onebox/internal/release"
)

// HostState is the drift set the plan binds to: live release id
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
	ID           string            `json:"id"`
	App          string            `json:"app"`
	Env          string            `json:"env"`
	CreatedAt    time.Time         `json:"created_at"`
	GitSHA       string            `json:"git_sha,omitempty"`
	ConfigHash   string            `json:"config_hash"`
	HostState    HostState         `json:"host_state"`
	PinnedImages map[string]string `json:"pinned_images"`
	// BuildImages is what the render was given for build-sourced workloads,
	// before pinning. It is not PinnedImages: pinning resolves a tag to a
	// digest *after* the render, so re-rendering with the pins produces a
	// different document than the one this plan bound. Execution reloads with
	// these and applies the pins afterwards, exactly as planning did.
	BuildImages      map[string]string `json:"build_images,omitempty"`
	SecretGeneration string            `json:"secret_generation,omitempty"`
	RenderedCompose  string            `json:"rendered_compose"`
	Commands         []string          `json:"commands"`
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
	cur, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return hs, err
	}
	hs.CurrentRelease = cur
	svcs := map[string]bool{}
	for name := range e.Spec.Workloads {
		svcs[name] = true
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
		if res.ExitCode != 0 {
			return hs, fmt.Errorf("docker inspect image for service %q failed (exit %d): %s", svc, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		hs.ImageIDs[svc] = strings.TrimSpace(res.Stdout)
	}
	return hs, nil
}

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ImageResolutionError is the stable, branchable refusal returned when a
// release workload cannot be bound to an immutable image. ResolvingCommand is
// deliberately an ob command rather than the failed registry probe: it is the
// safe operator action that can make the plan executable.
type ImageResolutionError struct {
	Workload         string
	Image            string
	ResolvingCommand string
	Detail           string
}

func (err *ImageResolutionError) Error() string {
	message := fmt.Sprintf("image_unresolved: workload %q has no immutable runtime image", err.Workload)
	if err.Image != "" {
		message += fmt.Sprintf(" (input %q)", err.Image)
	}
	if err.Detail != "" {
		message += ": " + err.Detail
	}
	return message + "; resolve it with `" + err.ResolvingCommand + "`"
}

func (err *ImageResolutionError) Code() string { return "image_unresolved" }

func imageResolutionError(workload, image, detail string) *ImageResolutionError {
	return &ImageResolutionError{
		Workload:         workload,
		Image:            image,
		ResolvingCommand: "ob deploy --image " + workload + "=<digest-reference>",
		Detail:           detail,
	}
}

func pinnedImageDigest(image string) string {
	_, digest, found := strings.Cut(image, "@")
	if found && digestRe.MatchString(digest) {
		return digest
	}
	return ""
}

// PinImages resolves every application-release workload image to a registry
// digest and rewrites the parsed runtime in place. The workload set includes
// applications, workers, daemons, jobs, and adopted Compose services. A single
// plan resolves identical references once and fails closed if any runtime
// service has no image or a tag cannot be resolved.
func (e *Engine) PinImages(ctx context.Context) (map[string]string, error) {
	pins := map[string]string{}
	services := make([]string, 0, len(e.Spec.Workloads))
	for name := range e.Spec.Workloads {
		services = append(services, name)
	}
	sort.Strings(services)
	resolved := map[string]string{}
	for _, svc := range services {
		s, ok := e.Compose.Services[svc]
		if !ok {
			return nil, imageResolutionError(svc, "", "the generated runtime does not contain the workload service")
		}
		if s.Image == "" {
			return nil, imageResolutionError(svc, "", "the workload has only a build source and production does not build images")
		}
		if pinnedImageDigest(s.Image) != "" {
			pins[svc] = s.Image
			continue
		}
		if pinned := resolved[s.Image]; pinned != "" {
			s.Image = pinned
			e.Compose.Services[svc] = s
			pins[svc] = pinned
			continue
		}
		res, err := e.T.Run(ctx, "docker buildx imagetools inspect "+q(s.Image)+" --format '{{.Manifest.Digest}}'")
		if err != nil {
			return nil, imageResolutionError(svc, s.Image, err.Error())
		}
		digest := strings.TrimSpace(res.Stdout)
		if res.ExitCode != 0 || !digestRe.MatchString(digest) {
			detail := strings.TrimSpace(res.Stderr)
			if detail == "" {
				detail = fmt.Sprintf("registry inspection returned exit %d without a valid sha256 digest", res.ExitCode)
			}
			return nil, imageResolutionError(svc, s.Image, detail)
		}
		pinned, err := imageref.WithDigest(s.Image, digest)
		if err != nil {
			return nil, imageResolutionError(svc, s.Image, err.Error())
		}
		resolved[s.Image] = pinned
		s.Image = pinned
		e.Compose.Services[svc] = s
		pins[svc] = pinned
	}
	return pins, nil
}

// FidelityContract is printed at the top of every plan: the
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
	return LocalPayloadDigestContext(context.Background(), dir)
}

func LocalPayloadDigestContext(ctx context.Context, dir string) (string, error) {
	var lines []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		jobResultDir := strings.SplitN(rel, "/", 2)[0]
		if rel == "compose.yaml" || strings.HasPrefix(jobResultDir, ".job-") && strings.HasSuffix(jobResultDir, "-result") {
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
	dir := release.PathsFor(e.names()).Releases + "/" + releaseID
	cmd := "cd " + q(dir) + " && find . -type f ! -name compose.yaml ! -path './.job-*-result' ! -path './.job-*-result/*' -exec sha256sum {} + 2>/dev/null | LC_ALL=C sort | sha256sum | cut -d' ' -f1"
	res, err := e.T.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("read payload digest for release %q failed (exit %d): %s", releaseID, res.ExitCode, strings.TrimSpace(res.Stderr))
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

	appendJobs := func(steps []string) {
		for _, job := range steps {
			cmdStr := cc + " run --rm --no-deps " + job
			if h, ok := e.Spec.Hooks[job]; ok && h.Run != "" {
				cmdStr = h.Run
			}
			out = append(out, fmt.Sprintf("job %s (gated — changed=false keeps rollback open): %s", job, cmdStr))
		}
	}
	appendHook := func(name string) {
		h, ok := e.Spec.Hooks[name]
		if !ok || h.Run == "" {
			return
		}
		where := "host"
		if h.Local {
			where = "local"
		}
		out = append(out, fmt.Sprintf("hook %s (%s, unplannable): %s", name, where, h.Run))
	}

	// The plan follows execution order. Manual jobs are intentionally absent.
	appendJobs(e.gateSteps())
	appendHook("pre_release")
	for _, roleName := range e.Spec.ReleaseOrder() {
		role := e.Spec.Workloads[roleName]
		svc := roleName
		head := fmt.Sprintf("release %s (%s", roleName, role.Mode())
		if n := role.Count(); n > 1 {
			// The contract's names, not a guess: a plan that shows names the
			// rollout will not create is a plan nobody can check against.
			slots := e.slotNames(svc, n)
			head += fmt.Sprintf(", %d replicas → %s..%s", n, slots[0], slots[len(slots)-1])
		}
		out = append(out, head+"):")
		out = append(out, "  "+cc+" pull --quiet "+svc)
		if role.Mode() == "rolling" {
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
			if wait := role.DrainWait(); role.Drain != nil && role.DrainSignal() != "TERM" && wait > 0 {
				out = append(out,
					fmt.Sprintf("  docker kill --signal=%s <current %s>; wait %s", role.DrainSignal(), svc, wait),
				)
			}
			out = append(out,
				fmt.Sprintf("  %s up -d --no-deps --force-recreate --timeout %d%s %s", cc, role.StopGraceSeconds(), scale, svc),
				"  wait ready (or running) — brief gap, stated",
			)
		}
	}
	appendJobs(e.Spec.JobOrderFor("post_release"))
	appendHook("post_release")
	appendHook("post_deploy")
	return out
}
