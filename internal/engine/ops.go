package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/release"
)

// Destroy tears the app down: containers via compose, then ob's own state
// dir. Volumes survive unless removeVolumes — data loss is opt-in. The managed
// proxy belongs to the sole host owner and is removed only when requested.
func (e *Engine) Destroy(ctx context.Context, removeVolumes, removeProxy bool) error {
	if err := e.RequireHostOwner(ctx); err != nil {
		return err
	}
	if removeProxy && !e.Spec.Proxy.Managed {
		return fmt.Errorf("--proxy: this application's proxy is not managed — nothing to remove")
	}
	hp := proxy.HostPaths(e.names())
	epoch, err := e.AcquireLock(ctx, "destroy", e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	leased, err := release.ActiveScheduleLeases(ctx, e.T, e.names())
	if err != nil {
		return fmt.Errorf("destroy refused: cannot establish scheduled-job release leases: %w", err)
	}
	if len(leased) > 0 {
		return fmt.Errorf("destroy refused: scheduled jobs are still running from release(s) %s; stop or wait for them before destroying the application", strings.Join(leased, ", "))
	}
	if err := e.WriteFence(ctx, "destroy", epoch); err != nil {
		return err
	}
	cur, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return err
	}
	if cur != "" {
		// The document being removed and the environment that makes it parse are
		// one immutable release. The working-tree project may have migrated a
		// Compose-defined database to a managed service and no longer name the old
		// interpolation file; using it here strands the recorded release.
		recorded, err := e.engineFromReleaseSnapshotFor(ctx, cur, "destroy")
		if err != nil {
			return err
		}
		down := recorded.composeCmd(release.PathsFor(e.names()).Releases+"/"+cur+"/compose.yaml") + " down --remove-orphans"
		if removeVolumes {
			down += " -v"
		}
		if res, err := e.mutate(ctx, down); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("compose down: %s", strings.TrimSpace(res.Stderr))
		}
	} else {
		// no release ever activated: sweep by project label
		ids, err := e.T.Run(ctx, "docker ps -aq --filter label=com.docker.compose.project="+q(e.Spec.Name))
		if err != nil {
			return err
		}
		if ids.ExitCode != 0 {
			return fmt.Errorf("list application containers failed (exit %d): %s", ids.ExitCode, strings.TrimSpace(ids.Stderr))
		}
		for _, id := range strings.Fields(ids.Stdout) {
			if validID.MatchString(id) {
				// Fail closed, exactly as the service sweep does. Warning and
				// continuing would go on to delete the volumes, the schedules
				// and the state directory while the container is still alive,
				// and then report a clean teardown.
				if res, err := e.mutate(ctx, "docker rm -f "+id); err != nil {
					return err
				} else if res.ExitCode != 0 {
					return fmt.Errorf("cannot remove container %s: %s — nothing further was destroyed",
						id, strings.TrimSpace(res.Stderr))
				}
			}
		}
		// `docker rm -f` never removes named volumes — honor --volumes here
		// too (compose labels volumes with the project, same as containers)
		if removeVolumes {
			res, err := e.T.Run(ctx, "docker volume ls -q --filter label=com.docker.compose.project="+q(e.Spec.Name))
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("list application volumes failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			var vols []string
			for _, v := range strings.Fields(res.Stdout) {
				if volName.MatchString(v) { // volume names are never interpolated unvalidated
					vols = append(vols, v)
				}
			}
			if len(vols) > 0 {
				res, err := e.mutate(ctx, "docker volume rm "+strings.Join(vols, " "))
				if err != nil {
					return fmt.Errorf("volume rm: %w", err)
				}
				if res.ExitCode != 0 {
					return fmt.Errorf("volume rm failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
				}
			}
		}
	}
	// A timer outlives the release directory it invokes, so it has to be
	// removed explicitly or it fails every minute forever.
	if err := e.RemoveSchedules(ctx); err != nil {
		return err
	}
	// Supporting services run in their own Compose projects precisely so that
	// no release can stop them. Destroy is not a release: the app is going
	// away, and a database left listening with nothing to serve is not a
	// safety property, it is a leak. The volume still survives unless the
	// operator asked for it too.
	if err := e.removeServices(ctx, removeVolumes); err != nil {
		return err
	}
	// External means release-independent, not ownerless. A full destroy removes
	// both app-scoped networks before deleting the evidence that proves legacy
	// ownership. Docker refuses removal while any unmanaged endpoint remains;
	// propagate that refusal so state and host ownership stay recoverable.
	if removeVolumes {
		if err := e.removeOwnedNetworks(ctx); err != nil {
			return err
		}
	}
	// state dir last (takes the lock, fence, and journals with it — that is
	// the point of destroy)
	base := release.PathsFor(e.names()).Base
	sweep := "rm -rf " + q(base)
	keepingCredentials := !removeVolumes && len(e.Spec.Services) > 0
	if keepingCredentials {
		// A service credential is generated once, on the target, and exists
		// nowhere else. Deleting it while deliberately keeping the volume
		// leaves data nothing can ever open again: the next bootstrap
		// generates a fresh password, the database keeps the one baked into
		// its data directory, and the application fails to authenticate
		// against a service that reports itself perfectly healthy.
		//
		// So the key stays with the lock. Everything else — releases,
		// journals, locks, fences — goes.
		sweep = fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 ! -name services -exec rm -rf {} +", q(base))
	}
	if res, err := e.mutate(ctx, sweep); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("remove state dir: %s", res.Stderr)
	}
	if keepingCredentials {
		e.logf("kept %s — the service volumes survive and these credentials are the only thing that can open them", e.names().ServiceDir())
	}
	// Released only when the volumes go and the proxy is this application's to
	// remove. Keeping it while data survives preserves the operator's ability to
	// return and run `ob destroy --volumes`: RequireHostOwner gates every
	// command, so a host released early is one whose remaining data its owner
	// can no longer reach. Service credentials need no separate term — they are
	// kept only when the volumes are, so removeVolumes already covers them.
	releaseOwner := removeVolumes && (!e.Spec.Proxy.Managed || removeProxy)
	if e.Spec.Proxy.Managed || releaseOwner {
		// Host scope, after the app fence is gone: plain Run, under host lock.
		if err := e.acquireHostLock(ctx, e.Opts.ForceLock); err != nil {
			return err
		}
		defer e.releaseHostLock(ctx)
		// The app state (and its fence) is intentionally gone; subsequent
		// mutations are protected solely by the host-scoped lock token.
		e.fenceVal = ""
		if removeProxy {
			// An earlier `ob destroy --proxy` removes this directory, so a repeat
			// run must treat "already gone" as done. Failing here left the owner
			// record in place with no command able to remove it.
			//
			// "Already gone" is a fact about the containers, not about the
			// config file. Skipping teardown when only the file is missing
			// releases host ownership while onebox-proxy still holds :80/:443,
			// and nothing ob can run afterwards removes it: the next
			// application claims the host and then cannot bind. Fall back to
			// the project label, which is what the compose file would have
			// selected anyway.
			// The sweep runs in BOTH branches, not just when the compose
			// file is missing. `down` only removes services the
			// current file declares, so a container carrying the project
			// label that the file no longer names survives it and `down`
			// still exits 0 — the same end state as the missing-file case:
			// ownership released with something still holding :80/:443, and
			// the compose file deleted so no ob command can reach it.
			//
			// --remove-orphans is deliberately absent because a project label
			// alone is not proof of ownership: a user can independently run
			// `docker compose -p onebox-proxy up`. A fixed name alone is not
			// proof either: after a rename (or on a host installed later), the
			// old name is free for an unrelated container to use.
			//
			// The fallback therefore requires the exact generated container
			// name AND the Compose project/service labels emitted by ob's
			// generated two-service document. This still finds ob containers
			// after the file or Compose state disappears without seizing a
			// same-named foreign container.
			//
			// Without `|| exit $?` a failed docker ps yields an empty list,
			// the if is skipped, and teardown "succeeds" — the very outcome
			// this exists to prevent.
			//
			// Reaching here implies the proxy is managed, but that proves only
			// the requested operation's scope. The filters below separately
			// prove ownership of every container selected for force-removal.
			down := "if [ -f " + q(hp.Compose) + " ]; then docker compose -p " + proxy.Project + " -f " + q(hp.Compose) + " down || exit $?; fi; " +
				"proxy_orphans=$(docker ps -aq --filter name=^" + proxy.ContainerName + "$ --filter label=com.docker.compose.project=" + proxy.Project + " --filter label=com.docker.compose.service=proxy) || exit $?; " +
				"discovery_orphans=$(docker ps -aq --filter name=^" + proxy.DiscoveryContainerName + "$ --filter label=com.docker.compose.project=" + proxy.Project + " --filter label=com.docker.compose.service=discovery) || exit $?; " +
				"legacy_discovery_orphans=$(docker ps -aq --filter name=^" + proxy.LegacyDiscoveryContainerName + "$ --filter label=com.docker.compose.project=" + proxy.Project + " --filter label=com.docker.compose.service=discovery) || exit $?; " +
				"if [ -n \"$proxy_orphans\" ]; then docker rm -f $proxy_orphans || exit $?; fi; " +
				"if [ -n \"$discovery_orphans\" ]; then docker rm -f $discovery_orphans || exit $?; fi; " +
				"if [ -n \"$legacy_discovery_orphans\" ]; then docker rm -f $legacy_discovery_orphans; fi"
			if res, err := e.hostMutate(ctx, down); err != nil {
				return err
			} else if res.ExitCode != 0 {
				return fmt.Errorf("proxy down: %s", strings.TrimSpace(res.Stderr))
			}
			res, err := e.hostMutate(ctx, "rm -rf "+q(hp.Dir))
			if err != nil {
				return fmt.Errorf("remove proxy dir: %w", err)
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("remove proxy dir failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			e.logf("host proxy removed")
		} else if e.Spec.Proxy.Managed {
			e.logf("host proxy kept — `ob destroy --proxy` removes it")
		}
		if releaseOwner {
			res, err := e.hostMutate(ctx, "rm -f "+q(hp.Owner))
			if err != nil {
				return fmt.Errorf("release host ownership: %w", err)
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("release host ownership failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			e.logf("host ownership released — another application can now claim this host")
		}
	}
	if !releaseOwner {
		// releaseOwner is removeVolumes && (!Managed || removeProxy), so with
		// a managed proxy --proxy is ALWAYS part of the answer — including on
		// the run that just passed it and kept volumes. Keying the suffix on
		// !removeProxy told that operator to run `ob destroy --volumes`,
		// which retains ownership again.
		e.logf("host ownership kept for %s — run `ob destroy --volumes%s` to remove what remains and release the host",
			e.Spec.Name, map[bool]string{true: " --proxy", false: ""}[e.Spec.Proxy.Managed])
	}
	e.logf("destroyed %s (volumes %s)", e.Spec.Name, map[bool]string{true: "REMOVED", false: "kept"}[removeVolumes])
	return nil
}

const (
	RuntimeTargetWorkload = "workload"
	RuntimeTargetService  = "service"
)

type RuntimeTarget struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type RuntimeTargetError struct {
	Name  string
	Valid []RuntimeTarget
}

func (err *RuntimeTargetError) Error() string {
	valid := make([]string, 0, len(err.Valid))
	for _, target := range err.Valid {
		valid = append(valid, target.Name+" ("+target.Kind+")")
	}
	return fmt.Sprintf("unknown runtime target %q; valid targets: %s", err.Name, strings.Join(valid, ", "))
}

func (err *RuntimeTargetError) Code() string { return "unknown_runtime_target" }

// ResolveRuntimeTarget classifies the complete operator-visible runtime
// namespace. Supporting services run in their own Compose projects; they are
// never treated as services in the application release document.
func (e *Engine) ResolveRuntimeTarget(name string) (RuntimeTarget, error) {
	if _, ok := e.Spec.Workloads[name]; ok {
		return RuntimeTarget{Name: name, Kind: RuntimeTargetWorkload}, nil
	}
	if _, ok := e.Spec.Services[name]; ok {
		return RuntimeTarget{Name: name, Kind: RuntimeTargetService}, nil
	}
	valid := make([]RuntimeTarget, 0, len(e.Spec.Workloads)+len(e.Spec.Services))
	for _, workload := range sortedNames(e.Spec.Workloads) {
		valid = append(valid, RuntimeTarget{Name: workload, Kind: RuntimeTargetWorkload})
	}
	for _, service := range sortedNames(e.Spec.Services) {
		valid = append(valid, RuntimeTarget{Name: service, Kind: RuntimeTargetService})
	}
	return RuntimeTarget{}, &RuntimeTargetError{Name: name, Valid: valid}
}

// Logs streams compose logs for exactly one workload or Onebox-run service.
func (e *Engine) Logs(ctx context.Context, name string, follow bool, tail int, stdout, stderr io.Writer) error {
	target, err := e.ResolveRuntimeTarget(name)
	if err != nil {
		return err
	}
	var cmd string
	if target.Kind == RuntimeTargetService {
		names := e.names()
		cmd = "docker compose -p " + names.ServiceProject(name) + " -f " + q(names.ServiceFile(name))
	} else {
		cur, err := release.Current(ctx, e.T, e.names())
		if err != nil {
			return err
		}
		if cur == "" {
			return fmt.Errorf("nothing deployed")
		}
		cmd = e.composeCmd(release.PathsFor(e.names()).Releases + "/" + cur + "/compose.yaml")
	}
	cmd += " logs --tail " + strconv.Itoa(tail)
	if follow {
		cmd += " --follow"
	}
	cmd += " " + name
	return e.T.RunStream(ctx, cmd, stdout, stderr)
}

const maxExecReasonBytes = 256

// ValidateExecReason protects the durable journal from unbounded or multiline
// operator input. A reason is public operational metadata; callers must never
// put a credential in it.
func ValidateExecReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("exec requires --reason with a short operational justification")
	}
	if len(reason) > maxExecReasonBytes {
		return fmt.Errorf("exec reason exceeds %d bytes", maxExecReasonBytes)
	}
	for _, r := range reason {
		if r < 0x20 || r == 0x7f {
			return errors.New("exec reason must be a single line without control characters")
		}
	}
	return nil
}

// ExecInAudited runs the operator's command verbatim (the same trust level as
// their shell), but journals only safe invocation metadata. Command bytes and
// stdout/stderr are never persisted by Onebox.
func (e *Engine) ExecInAudited(ctx context.Context, operationID, name, command, reason string, stdout, stderr io.Writer) (containerID string, err error) {
	if err := ValidateExecReason(reason); err != nil {
		return "", err
	}
	if strings.TrimSpace(operationID) == "" {
		return "", errors.New("exec operation ID is required")
	}
	if strings.TrimSpace(command) == "" {
		return "", errors.New("exec command is required")
	}
	if err := e.RequireHostOwner(ctx); err != nil {
		return "", err
	}
	target, err := e.ResolveRuntimeTarget(name)
	if err != nil {
		return "", err
	}
	epoch, err := e.AcquireLock(ctx, operationID, e.Opts.ForceLock)
	if err != nil {
		return "", err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, operationID, epoch); err != nil {
		return "", err
	}
	stopHeartbeat := e.StartHeartbeat(ctx)
	defer stopHeartbeat()
	project := e.Spec.Name
	if target.Kind == RuntimeTargetService {
		project = e.names().ServiceProject(name)
	}
	ids, err := e.containerIDsForProject(ctx, project, name)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("%s: no running container", name)
	}
	sort.Strings(ids)
	containerID = ids[0]
	commandDigest := HashBytes([]byte(command))
	writer := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: operationID, Epoch: epoch, Operator: journal.DefaultOperator(),
		GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner,
	}
	invocation := journal.Record{
		Phase: "exec", Event: "start", Status: "ok", Target: target.Name,
		TargetKind: target.Kind, CommandDigest: commandDigest, ContainerID: containerID, Reason: reason,
	}
	if err := writer.Append(ctx, invocation); err != nil {
		return containerID, fmt.Errorf("journal exec start: %w", err)
	}
	defer func() {
		finish := invocation
		finish.Event = "finish"
		finish.Status = "ok"
		if err != nil {
			finish.Status = "fail"
			finish.ErrorCode = "exec_failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				finish.ErrorCode = "exec_cancelled"
			}
		}
		journalContext := ctx
		var cancel context.CancelFunc
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			journalContext, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
		}
		if journalErr := writer.Append(journalContext, finish); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal exec finish: %w", journalErr))
		}
	}()
	err = e.mutateStream(ctx, "docker exec "+containerID+" sh -c "+q(command), stdout, stderr)
	return containerID, err
}

// volName: docker volume names — never interpolated back into a shell
// command without matching this (injection rule).
var volName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
