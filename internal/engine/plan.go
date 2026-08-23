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

	"github.com/labstack/onebox/internal/app"
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

// GuidanceCommand names the exact workload to pin, which is more useful than
// the generic command published for the code.
func (err *ImageResolutionError) GuidanceCommand() string { return err.ResolvingCommand }

func imageResolutionError(workload, image, detail string) *ImageResolutionError {
	return &ImageResolutionError{
		Workload: workload,
		Image:    image,
		// `ob plan`, not `ob deploy`: a structured deploy without --plan is
		// refused with plan_required, so guidance naming it hands an agent a
		// refusal instead of progress. Pinning the workload in a plan is the
		// step that actually unblocks either path.
		ResolvingCommand: "ob plan --image " + workload + "=<digest-reference>",
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

// A release directory is a staging directory plus two things the lifecycle
// writes to it afterwards: the state manifest, and the result directory of any
// job run against the serving release. Counting those as payload makes the
// local and remote digests permanently unequal — every plan then reports a
// change, no deploy short-circuits, and pre-release migrations re-run.
//
// `.ob-secret-generations/` is deliberately NOT excluded. Staging writes the
// bound generation's decrypted payload there (see stageExecution), so
// excluding it would drop every secret byte from the digest: a rotated secret
// reusing the live generation would hash identically, the deploy would
// short-circuit as a no-op, and the new value would never reach the host —
// while a sealed plan would stop binding the secret material it was approved
// against.
//
// `compose.yaml` is excluded only at the top level, because it is compared
// separately and label-invariantly. A compose file nested anywhere else is
// payload.
//
// The job-result exclusions are exact paths built from the declared job names
// rather than a `.job-*-result` glob. `find -path` matches with fnmatch and no
// FNM_PATHNAME, so `*` crosses `/` there while a Go first-segment test does
// not — the glob quietly excluded `./.job-a/b-result` on the host and kept it
// locally, which is the same permanent inequality this code exists to prevent.
// Exact names cannot diverge, and payloadExclusionsFor is the one place both
// renderings come from.
// jobResultDirName is the release-relative directory a job writes its result
// into. Declared here because the payload digest must exclude exactly what
// gate.go creates, and a second spelling would silently stop matching.
func jobResultDirName(job string) string { return ".job-" + job + "-result" }

type payloadExclusion struct {
	name    string // exact top-level entry name
	subtree bool   // also exclude everything beneath it
}

func (x payloadExclusion) findArgs() string {
	args := "! -path './" + x.name + "'"
	if x.subtree {
		args += " ! -path './" + x.name + "/*'"
	}
	return args
}

func (x payloadExclusion) excludes(rel string) bool {
	if rel == x.name {
		return true
	}
	return x.subtree && strings.HasPrefix(rel, x.name+"/")
}

// payloadExclusionsFor is derived from the spec so the job-result names are
// exact. A leftover result directory for a job the project no longer declares
// is deliberately treated as payload: it is genuine drift on the host, and
// reporting it is better than hiding it behind a wildcard.
func payloadExclusionsFor(spec *app.Resolved) []payloadExclusion {
	exclusions := []payloadExclusion{
		{name: "compose.yaml"},
		{name: release.ManifestFileName},
	}
	// Only a job writes a result directory. Excluding the name for every
	// workload would hide a live `.job-web-result` belonging to a non-job
	// workload from drift detection.
	names := make([]string, 0, len(spec.Workloads))
	for name, workload := range spec.Workloads {
		if workload.IsJob() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		exclusions = append(exclusions, payloadExclusion{name: jobResultDirName(name), subtree: true})
	}
	return exclusions
}

func payloadFindArgs(exclusions []payloadExclusion) string {
	args := make([]string, 0, len(exclusions))
	for _, exclusion := range exclusions {
		args = append(args, exclusion.findArgs())
	}
	return strings.Join(args, " ")
}

func isPayloadMember(exclusions []payloadExclusion, rel string) bool {
	for _, exclusion := range exclusions {
		if exclusion.excludes(rel) {
			return false
		}
	}
	return true
}

// LocalPayloadDigest hashes everything in a staging dir except the entries
// payloadExclusionsFor names — env files, the decrypted secret generation, the
// snapshot, bind sources. Same line format and ordering as the remote shell
// pipeline in RemotePayloadDigest, so equal digests mean byte-equal payloads.
//
// The exclusion set depends on the declared job names, so this takes the spec.
func LocalPayloadDigest(spec *app.Resolved, dir string) (string, error) {
	return LocalPayloadDigestContext(context.Background(), spec, dir)
}

func LocalPayloadDigestContext(ctx context.Context, spec *app.Resolved, dir string) (string, error) {
	exclusions := payloadExclusionsFor(spec)
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
		if !isPayloadMember(exclusions, filepath.ToSlash(rel)) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%x  ./%s\n", sha256.Sum256(b), filepath.ToSlash(rel)))
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
	// The exclusion arguments are rendered from the same table the local walk
	// matches against, so for a given spec the two select the same set. Nothing is
	// redirected to /dev/null: an unreadable file must fail the read rather
	// than silently shrink the digest to one taken over the readable subset.
	// The listing is staged in a file, not a shell variable. A pipeline's exit
	// status is its last stage's, so `find … | sort | sha256sum | cut`
	// reported success even when find could not read part of the tree —
	// yielding a well-formed digest over the readable subset only. Staging
	// lets `||` propagate find's own status.
	//
	// A variable would do that too, but a payload includes bind sources: a
	// project that binds a large tree makes the listing tens of megabytes,
	// held in the shell and re-emitted through printf, which is where memory
	// and ARG_MAX limits start deciding whether a deploy works. The file
	// streams instead, and an empty payload leaves an empty file, which
	// hashes as the local walk's empty input without a special case.
	//
	// The temp file lives outside the release directory on purpose: a file
	// inside it would be counted by the very find that is digesting it.
	// Every stage is staged, not piped, for the same reason: a pipeline's exit
	// status is its last stage's. Folding through `sort | sha256sum | cut`
	// would reintroduce the defect one stage over — the large payload that
	// motivates staging is exactly what makes sort spill to $TMPDIR, and a
	// full disk there has sort emit partial output and exit 2 while sha256sum
	// happily hashes the truncation. That returns a well-formed wrong digest,
	// which reads downstream as "live payload changed since plan" on a host
	// where nothing changed.
	// The trap is installed BEFORE the first mktemp: installing it after both
	// leaves a window where the second mktemp fails, or the shell is
	// interrupted between them, and the first temp file survives. Digests run
	// on every plan and every deploy precondition, so that accumulates in
	// $TMPDIR. rm -f tolerates the unset variables.
	// `cd` is its own guarded statement. Written as `cd DIR && trap …; …` the
	// && binds to the trap alone, the ; ends the and-list, and a failed cd —
	// release directory missing, renamed, unreadable — falls through to `find .`
	// in the login shell's home directory. That exits 0 with a well-formed
	// digest of the wrong tree, which reads downstream as "the payload
	// changed" on a host where nothing did.
	cmd := "cd " + q(dir) + " || exit $?; " +
		"trap 'rm -f \"$listing\" \"$sorted\"' EXIT INT TERM; " +
		"listing=$(mktemp) || exit $?; sorted=$(mktemp) || exit $?; " +
		"find . -type f " + payloadFindArgs(payloadExclusionsFor(e.Spec)) +
		" -exec sha256sum {} + > \"$listing\" || exit $?; " +
		"LC_ALL=C sort \"$listing\" > \"$sorted\" || exit $?; " +
		"digest=$(sha256sum < \"$sorted\") || exit $?; printf '%s\\n' \"$digest\""
	res, err := e.T.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("read payload digest for release %q failed (exit %d): %s", releaseID, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(res.Stdout), "-")), nil
}

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
		// The plan shows the pull only when the release will actually run one,
		// because a plan listing a command that does not happen is a plan
		// nobody can check against.
		if line := e.plannedPullLine(svc, cc); line != "" {
			out = append(out, line)
		}
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
			// Gated exactly as recreate gates it — an authored `drain.wait`,
			// nothing else. recreate signals every container and then sleeps,
			// whatever the signal is, so excluding the default TERM here hid a
			// kill and a pause the deploy certainly takes. A plan that shows a
			// step execution skips, or hides one it takes, is a plan nobody can
			// check against.
			if wait := role.DrainWait(); role.Drain != nil && role.Drain.Wait != "" && wait > 0 {
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
