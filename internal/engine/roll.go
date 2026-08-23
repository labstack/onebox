package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
)

func (e *Engine) composeCmd(remoteComposePath string) string {
	cmd := "docker compose -p " + e.Spec.Name + " -f " + q(remoteComposePath)
	// The same files that fed interpolation when the document was parsed feed
	// it again when Compose reads it here. Without them a `${VAR}` carried in
	// verbatim from a referenced Compose source resolves to empty on the
	// target while it resolved to a value at plan time — the document would
	// then mean something different in the place it actually runs.
	//
	// Compose applies repeated --env-file in order, later winning, which is
	// the order the project declares them in.
	if e.Spec.Runtime != nil {
		dir := path.Dir(remoteComposePath)
		for _, entry := range e.Spec.Runtime.EnvFiles {
			// An encrypted entry is staged only when a workload resolves it, so
			// naming one here unconditionally passes `--env-file` for a file
			// that may never have been written and fails the whole invocation.
			// Interpolation is fed by the plaintext entries either way.
			if entry.Encrypted() {
				continue
			}
			cmd += " --env-file " + q(path.Join(dir, entry.StagedPath()))
		}
	}
	return cmd
}

// newcomerIDs finds containers of a specific release — the ob.release label
// render injects is what makes resume possible.
func (e *Engine) newcomerIDs(ctx context.Context, svc, releaseID string) ([]string, error) {
	res, err := e.T.Run(ctx,
		"docker ps -q --filter label=com.docker.compose.project="+q(e.Spec.Name)+
			" --filter label=com.docker.compose.service="+q(svc)+
			" --filter label=ob.release="+q(releaseID))
	if err != nil {
		return nil, err
	}
	return splitIDs(res.Stdout)
}

// RollRole rolls a role to its desired replica count of the new release with
// zero downtime using the traffic-shift protocol: join →
// converged → drain → converged → bleed → SIGTERM → remove). It surges ONE new
// replica at a time, waits it healthy, retires one old, and hands the newcomer
// a clean slot name — never compose's <project>-<svc>-<n> default. Repeats until
// every replica is the new release. Resume-aware: already-running newcomers of
// this release are adopted, not duplicated.
func (e *Engine) RollRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Spec.Workloads[roleName]
	svc := roleName
	cc := e.composeCmd(remoteComposePath)
	releaseID := filepath.Base(filepath.Dir(remoteComposePath))
	desired := role.Count()
	within, pollEvery := role.ReadyTiming()

	pulled := false
	// Each pass converges by one step: add a missing new replica, or retire a
	// surplus/old one. The guard bounds a pathological non-converging loop.
	for guard := 0; ; guard++ {
		news, err := e.newcomerIDs(ctx, svc, releaseID)
		if err != nil {
			return err
		}
		cur, err := e.containerIDs(ctx, svc)
		if err != nil {
			return err
		}
		olds := subtract(cur, news)
		if guard > 4*(desired+len(olds))+8 {
			return fmt.Errorf("role %s: roll did not converge (news=%d olds=%d)", roleName, len(news), len(olds))
		}

		if len(news) >= desired && len(olds) == 0 {
			if len(news) == desired {
				break
			}
			// surplus new replicas (count reduced) — drain one down to desired
			if err := e.retireContainer(ctx, role, news[desired], pollEvery); err != nil {
				return err
			}
			continue
		}

		// surge one new replica if we still need more of the new release
		if len(news) < desired {
			if !pulled {
				if err := e.pullBeforeRelease(ctx, svc, cc); err != nil {
					return err
				}
				pulled = true
			}
			known := idSet(news)
			scale := len(cur) + 1
			if res, err := e.mutate(ctx, fmt.Sprintf("%s up -d --no-deps --no-recreate --scale %s=%d %s", cc, svc, scale, svc)); err != nil {
				return err
			} else if res.ExitCode != 0 {
				return fmt.Errorf("up --scale %s: %s", svc, res.Stderr)
			}
			after, err := e.newcomerIDs(ctx, svc, releaseID)
			if err != nil {
				return err
			}
			newID := ""
			for _, id := range after {
				if !known[id] {
					newID = id
					break
				}
			}
			if newID == "" {
				return fmt.Errorf("role %s: scale up produced no new container", roleName)
			}
			// strip the <project>-<svc>-<n> name immediately so the project
			// prefix never appears; a transient name until it takes a slot.
			if err := e.renameContainer(ctx, newID, e.names().TransientContainer(roleName)); err != nil {
				return err
			}
			// join: the newcomer becomes a routable endpoint via its healthcheck
			if err := e.waitHealth(ctx, newID, "healthy", within, pollEvery); err != nil {
				e.logf("join failed for %s — removing new container, existing keep serving", roleName)
				cleanupErr := e.mutateChecked(ctx, "remove unhealthy newcomer "+newID, "docker rm -f "+newID)
				return errors.Join(fmt.Errorf("role %s: new container never became healthy: %w", roleName, err), cleanupErr)
			}
		}

		// retire one old, freeing its slot for the newcomer just added
		if len(olds) > 0 {
			if err := e.retireContainer(ctx, role, olds[0], pollEvery); err != nil {
				return err
			}
		}

		// hand clean slot names to any new container that doesn't have one yet
		if err := e.reslot(ctx, svc, releaseID, desired); err != nil {
			return err
		}
	}
	if err := e.reslot(ctx, svc, releaseID, desired); err != nil {
		return err
	}
	return nil
}

// retireContainer drains one container out of rotation (poison its health so the
// proxy drops it BEFORE any signal), waits the drain, optionally bleeds long
// connections, then stops and removes it.
func (e *Engine) retireContainer(ctx context.Context, role app.Workload, id string, pollEvery time.Duration) error {
	e.sleepBusy("converge (proxy observes the newcomer)", e.Opts.ConvergeBuffer)

	// Poisoning health is what takes the container out of rotation before any
	// signal reaches it. A check that cannot read the drain file — an exec-list
	// check in an image with no shell — can never flip, so waiting for it would
	// burn the whole budget and then stop a container the proxy is still using.
	// Saying so is better than a warning that reads like a fault.
	guarded, probeEvery, probeRetries, err := e.bakedHealthcheck(ctx, id)
	if err != nil {
		return err
	}
	if !guarded {
		e.logf("%s: health check cannot be drain-guarded (no shell in the image); "+
			"relying on the converge buffer alone", id[:min(12, len(id))])
		e.sleepBusy("converge (proxy drops the container)", e.Opts.ConvergeBuffer)
		return e.stopAndRemove(ctx, role, id)
	}
	if err := e.mutateChecked(ctx, "mark container "+id+" draining", "docker exec "+id+" touch "+app.DrainFile); err != nil {
		return err
	}
	// Budget the drain wait off the ACTUAL flip cost — this container's own
	// retries × its own probe interval — plus two probes of slack, so raising
	// retries cannot make the flip exceed the budget. Timing out here SIGTERMs
	// a container the proxy may still be routing to, which is what the budget
	// exists to prevent.
	//
	// Both numbers come from the container, not from the spec: the spec
	// describes what is being started, while this is what is being drained. Nor
	// is either the poll cadence, which is only how often Onebox runs `docker
	// inspect` — a local query. Budgeting a container-side flip against a local
	// query's cadence is what made the budget expire before the flip could
	// happen at all.
	drainBudget := time.Duration(probeRetries+2) * probeEvery
	if err := e.waitHealth(ctx, id, "unhealthy", drainBudget, pollEvery); err != nil {
		e.warnf("container never reported unhealthy (%v); proceeding after buffer", err)
	}
	e.sleepBusy("converge (proxy drops the drained container)", e.Opts.ConvergeBuffer)

	if wait := role.DrainWait(); role.Drain != nil && role.Drain.Wait != "" && wait > 0 {
		if sig := role.DrainSignal(); sig != "TERM" {
			if err := e.mutateChecked(ctx, "signal draining container "+id, "docker kill --signal="+sig+" "+id); err != nil {
				return err
			}
		}
		e.sleepBusy("drain wait ("+wait.String()+")", wait)
	}

	return e.stopAndRemove(ctx, role, id)
}

// stopAndRemove is the end of a retirement: SIGTERM with the workload's grace,
// then removal.
func (e *Engine) stopAndRemove(ctx context.Context, role app.Workload, id string) error {
	if res, err := e.mutate(ctx, fmt.Sprintf("docker stop -t %d %s", role.StopGraceSeconds(), id)); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("stop %s: %s", id, res.Stderr)
	}
	return e.mutateChecked(ctx, "remove container "+id, "docker rm "+id)
}

// bakedHealthcheck is the healthcheck a running container was CREATED with,
// which is the only one that governs how it behaves now. A healthcheck is baked
// in at creation, so the spec being deployed describes the containers being
// started, never the ones being drained: reading it from the container is what
// makes the drain budget survive a change to the probe timing — including a
// change to Onebox's own default, which no operator asked for and would
// otherwise strand every replica of the first deploy after an upgrade.
//
// Omitted fields mean the runtime's defaults, not zero.
func (e *Engine) bakedHealthcheck(ctx context.Context, id string) (guarded bool, interval time.Duration, retries int, err error) {
	res, err := e.T.Run(ctx, "docker inspect -f '{{json .Config.Healthcheck}}' "+id)
	if err != nil {
		return false, 0, 0, err
	}
	if res.ExitCode != 0 {
		return false, dockerDefaultHealthInterval, dockerDefaultHealthRetries, nil
	}
	guarded = strings.Contains(res.Stdout, app.DrainFile)
	var baked struct {
		Interval int64 `json:"Interval"`
		Retries  int   `json:"Retries"`
	}
	interval, retries = dockerDefaultHealthInterval, dockerDefaultHealthRetries
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &baked); err == nil {
		// Clamped, because these come from a container rather than from a
		// validated project: one created by an older runner can carry values
		// that were never bounded, and a budget wrapped negative by them
		// expires instantly — stopping a container the proxy may still be
		// routing to, which is the failure this budget exists to prevent.
		if baked.Interval > 0 && time.Duration(baked.Interval) <= maxBakedHealthInterval {
			interval = time.Duration(baked.Interval)
		}
		if baked.Retries > 0 && baked.Retries <= maxBakedHealthRetries {
			retries = baked.Retries
		}
	}
	return guarded, interval, retries, nil
}

// The runtime's own defaults, applied when a healthcheck omits the field. They
// are the values a container created by an older Onebox is running with.
const (
	dockerDefaultHealthInterval = 30 * time.Second
	dockerDefaultHealthRetries  = 3
	// Ceilings for what a container reports, matching what the project file is
	// allowed to declare.
	maxBakedHealthInterval = 7 * 24 * time.Hour
	maxBakedHealthRetries  = 1000
)

// reslot gives each new-release container a clean, stable slot name: the plain
// <app>-<component>-1..<app>-<component>-N for every replica count.
// A slot still held by an old container counts as taken, so names never clash;
// as olds retire their slots free and the next reslot fills them.
func (e *Engine) reslot(ctx context.Context, svc, releaseID string, desired int) error {
	news, err := e.newcomerIDs(ctx, svc, releaseID)
	if err != nil {
		return err
	}
	all, err := e.containerIDs(ctx, svc)
	if err != nil {
		return err
	}
	slots := e.slotNames(svc, desired)
	slotSet := map[string]bool{}
	for _, s := range slots {
		slotSet[s] = true
	}
	newSet := idSet(news)
	nameByID := map[string]string{}
	taken := map[string]bool{}
	for _, id := range all {
		n, err := e.nameOf(ctx, id)
		if err != nil {
			return err
		}
		nameByID[id] = n
		if slotSet[n] {
			taken[n] = true
		}
	}
	for _, id := range all {
		if !newSet[id] || slotSet[nameByID[id]] {
			continue // not ours, or already correctly slotted
		}
		target := ""
		for _, s := range slots {
			if !taken[s] {
				target = s
				break
			}
		}
		if target == "" {
			continue // no free slot yet (an old still holds it)
		}
		if err := e.mutateChecked(ctx, "rename container "+id, "docker rename "+id+" "+target); err != nil {
			return err
		}
		taken[target] = true
	}
	return nil
}

// renameContainer gives a newcomer its temporary application-scoped name.
func (e *Engine) renameContainer(ctx context.Context, id, name string) error {
	if cur, err := e.nameOf(ctx, id); err != nil {
		return err
	} else if cur == name {
		return nil
	}
	return e.mutateChecked(ctx, "rename container "+id, "docker rename "+id+" "+name)
}

// nameOf returns a container's name without the leading slash docker reports.
func (e *Engine) nameOf(ctx context.Context, id string) (string, error) {
	res, err := e.T.Run(ctx, "docker inspect -f '{{.Name}}' "+id)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(res.Stdout), "/"), nil
}

// slotNames is the target name set, from the naming contract.
//
// It is the contract's names and not Compose's, and not a local invention
// either. Container names are host-global: two applications that each have a
// `web` workload would both want `web-1`, and the second would fail to start
// or, worse, be renamed over the first. The contract carries the application
// in every name for exactly that reason, and preflight checks those names for
// collisions — so a rollout that used different ones would be checking for
// collisions it then does not create, and creating collisions it never checked.
func (e *Engine) slotNames(workload string, desired int) []string {
	n := e.names()
	out := make([]string, desired)
	for i := range out {
		out[i] = n.Container(workload, i+1)
	}
	return out
}

// names resolves the derived-name contract for the environment being executed.
func (e *Engine) names() app.Names { return e.Spec.NamesFor(e.Opts.Environment) }

// Names is the resolved layout for this environment: the one authority for
// where anything belonging to this application lives on the target.
func (e *Engine) Names() app.Names { return e.names() }

func idSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func subtract(all, remove []string) []string {
	drop := map[string]bool{}
	for _, r := range remove {
		drop[r] = true
	}
	var out []string
	for _, a := range all {
		if !drop[a] {
			out = append(out, a)
		}
	}
	return out
}

func (e *Engine) waitHealth(ctx context.Context, id, want string, budget, interval time.Duration) error {
	deadline := e.Opts.Now().Add(budget)
	_, stop := e.ui.Busy(fmt.Sprintf("waiting %.12s → %s", id, want))
	defer stop()
	for {
		h, err := e.healthOf(ctx, id)
		if err != nil {
			return err
		}
		if h == want {
			return nil
		}
		if h == "none" && want == "healthy" {
			return fmt.Errorf("container %s has no healthcheck, and rolling waits for one — declare health: on the workload, or strategy: recreate", id)
		}
		if e.Opts.Now().After(deadline) {
			if why := e.healthDiagnosis(ctx, id); why != "" {
				return fmt.Errorf("timeout waiting for %s to be %s: %s", id, want, why)
			}
			return fmt.Errorf("timeout waiting for %s to be %s (last: %s)", id, want, h)
		}
		e.Opts.Sleep(interval)
	}
}

// pullPolicyFor is the workload's declared `image.pull`, defaulted by the
// loader to "missing".
func (e *Engine) pullPolicyFor(roleName string) string {
	if e.Spec == nil || e.Spec.Spec == nil {
		return "missing"
	}
	role, ok := e.Spec.Workloads[roleName]
	if !ok || role.Image == nil || role.Image.Pull == "" {
		return "missing"
	}
	return role.Image.Pull
}

// pullBeforeRelease fetches the workload image unless it does not need
// fetching.
//
// `image.pull` was a schema key the loader defaulted, validation checked and
// the reference documented — and nothing read. Every release ran
// `docker compose pull` for every workload on every deploy, including for an
// image already pinned by digest and already on the host, which is a request to
// the registry that cannot change the outcome. A rate-limited registry then
// failed a deploy that had nothing to fetch, and a project declaring
// `pull: never` was pulled from anyway.
//
// always: fetch. never: do not, and let the release fail on a missing image
// rather than reaching out. missing (the default): fetch only what the host
// does not already hold, which for a digest-pinned image is an exact answer.
func (e *Engine) pullBeforeRelease(ctx context.Context, roleName, composeCommand string) error {
	policy := e.pullPolicyFor(roleName)
	if policy == "never" {
		return nil
	}
	// A nil Compose means nothing has been rendered to compare against, so the
	// only safe answer is to pull.
	if policy == "missing" && e.Compose != nil {
		service, ok := e.Compose.Services[roleName]
		if ok && containsDigest(service.Image) {
			held, err := e.imagePresentByDigest(ctx, service.Image)
			if err != nil {
				return err
			}
			if held {
				return nil
			}
		}
	}
	res, err := e.mutate(ctx, composeCommand+" pull --quiet "+roleName)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("pull %s: %s", roleName, res.Stderr)
	}
	return nil
}

// plannedPullLine is what pullBeforeRelease will do, for the plan preview. It
// answers from the plan's own resolved images rather than probing the host: the
// plan is already bound to those digests, and a preview that reached out would
// be doing the work it is describing.
func (e *Engine) plannedPullLine(roleName, composeCommand string) string {
	if e.pullPolicyFor(roleName) == "never" {
		return ""
	}
	return "  " + composeCommand + " pull --quiet " + roleName
}
