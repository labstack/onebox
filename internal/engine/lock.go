package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

// ErrFenced: a mutating command was refused host-side because a newer deploy
// owns this host, so a stale runner is rejected locally.
var ErrFenced = errors.New("fenced: a newer deploy owns this host — this runner is stale")

type lockMeta struct {
	Owner      string `json:"owner"`
	DeployID   string `json:"deploy_id"`
	Epoch      int    `json:"epoch"`
	GitSHA     string `json:"git_sha,omitempty"`
	ConfigHash string `json:"config_hash,omitempty"`
	TTLSeconds int    `json:"ttl_s"`
	AcquiredAt string `json:"acquired_at"`
	Token      string `json:"token,omitempty"`
}

type pinnedScheduleLeasePolicy struct {
	allow    bool
	conflict string
}

// scheduleRendezvousWaitSeconds lets a reader or writer already inside the
// short schedule/deploy handoff finish without turning ordinary concurrency
// into a missed firing or a refused operation. It does not wait for the
// durable application lock: that lock may cover a whole deploy or exec.
const scheduleRendezvousWaitSeconds = 10

func (e *Engine) base() string      { return release.PathsFor(e.names()).Base }
func (e *Engine) lockPath() string  { return e.base() + "/lock" }
func (e *Engine) epochPath() string { return e.base() + "/epoch" }
func (e *Engine) fencePath() string { return e.base() + "/fence" }

// AcquireLock takes the deploy lock at the authority (v1: the only host).
// Epoch is bumped under the winner. TTL runs on the HOST's clock; a fresh
// lock refuses (unless force, which prints the holder + its journal tail —
// the operator sees who they are trampling).
func (e *Engine) AcquireLock(ctx context.Context, deployID string, force bool) (int, error) {
	return e.acquireLock(ctx, deployID, force, pinnedScheduleLeasePolicy{})
}

// acquireLock serializes application operations with scheduled-job startup,
// then inspects the release leases while the new application lock prevents a
// later pinned runner from entering. Existing callers fail closed; deploy is
// the one operation that may supply an explicit compatible lease policy.
func (e *Engine) acquireLock(ctx context.Context, deployID string, force bool, leasePolicy pinnedScheduleLeasePolicy) (int, error) {
	e.lockVal = ""
	if err := e.claimAppDir(ctx); err != nil {
		return 0, err
	}

	ttl := int(e.lockTTL().Seconds())
	var epoch int
	var useScheduleLock bool
	lock := remoteLock[lockMeta]{
		label: "deploy lock", path: e.lockPath(), attempts: 4, force: force, ttl: ttl,
		breakHint: "confirm no deploy is running, then retry with --break-lock",
		next: func(ctx context.Context) (string, error) {
			// Read the epoch fresh on every attempt. A concurrent acquirer may
			// have persisted a higher epoch between our attempts; fencing relies
			// on a strictly increasing epoch, so a value read once before the
			// loop could be reused and collide.
			var err error
			if epoch, err = e.nextEpoch(ctx, e.epochPath()); err != nil {
				return "", err
			}
			jobs, err := e.Spec.ScheduledJobs()
			if err != nil {
				return "", err
			}
			useScheduleLock = e.hasScheduleFlock(ctx)
			if len(jobs) > 0 && !useScheduleLock {
				return "", errors.New("scheduled jobs require a compatible util-linux flock at /usr/bin/flock so lock contention can be distinguished from host failures; install util-linux or upgrade it and deploy again")
			}
			b, err := json.Marshal(lockMeta{
				Owner: journal.DefaultOperator(), DeployID: deployID, Epoch: epoch,
				TTLSeconds: ttl, AcquiredAt: time.Now().UTC().Format(time.RFC3339),
			})
			return string(b), err
		},
		wrapCreate: func(create string) string {
			if !useScheduleLock {
				return create
			}
			// An exclusive scheduled job holds this kernel lock for its whole
			// run; a pinned job holds it only until its immutable release lease
			// exists. Take it around atomic application-lock creation so
			// neither side can pass its check before the other publishes
			// ownership. Keep doing this after the last schedule is removed: an
			// old unit may already be starting while that removal deploy begins.
			return "/usr/bin/flock --exclusive --timeout " + strconv.Itoa(scheduleRendezvousWaitSeconds) + " --conflict-exit-code " + strconv.Itoa(release.FlockConflictExitCode) + " " +
				q(e.names().ScheduleRunLock()) + " /bin/sh -c " + q(create)
		},
		createFailed: func(res transport.Result) error {
			if useScheduleLock && res.ExitCode == release.FlockConflictExitCode {
				return fmt.Errorf("scheduling rendezvous remained busy — run `ob status` to see active jobs, then wait for the current job or application operation to finish")
			}
			return fmt.Errorf("acquire application lock: lock creation or schedule rendezvous failed (exit %d): %s", res.ExitCode, diagnostic(res))
		},
		decide: func(holder lockHolder[lockMeta]) (lockVerdict, error) {
			h := holder.Meta
			switch {
			case holder.Age > ttl:
				e.logf("lock: holder %s (deploy %s) expired %ds ago — taking over", h.Owner, h.DeployID, holder.Age-ttl)
			case h.DeployID == deployID:
				// resume/abort of the very deploy that holds the lock: safe to
				// reclaim — the new epoch fences the previous runner regardless
				e.logf("lock: reclaiming for deploy %s (previous runner is fenced by the new epoch)", deployID)
			case force:
				e.logf("lock: FORCE-breaking %s (deploy %s, age %ds) — holder's journal tail:", h.Owner, h.DeployID, holder.Age)
				tres, _ := e.T.Run(ctx, "tail -5 "+q(e.base()+"/journal/"+sanitizeID(h.DeployID)+".jsonl")+" 2>/dev/null || true")
				for _, l := range strings.Split(strings.TrimSpace(tres.Stdout), "\n") {
					if l != "" {
						e.logf("  %s", l)
					}
				}
			default:
				return lockRefuse, fmt.Errorf("deploy lock held by %s (deploy %s, age %ds, ttl %ds) — wait, or --break-lock to break it",
					h.Owner, h.DeployID, holder.Age, ttl)
			}
			return lockReclaim, nil
		},
	}
	value, err := lock.acquire(ctx, e)
	if err != nil {
		return 0, err
	}
	e.lockVal = value
	if useScheduleLock {
		leased, leaseErr := release.ActiveScheduleLeases(ctx, e.T, e.names())
		if leaseErr != nil {
			e.ReleaseLock(ctx)
			return 0, fmt.Errorf("acquire application lock: cannot establish scheduled-job release leases: %w", leaseErr)
		}
		if len(leased) > 0 && !leasePolicy.allow {
			detail := ""
			if leasePolicy.conflict != "" {
				detail = "; this deployment includes " + leasePolicy.conflict
			}
			e.ReleaseLock(ctx)
			return 0, fmt.Errorf("application operation refused: pinned scheduled jobs are running from release(s) %s%s — wait for them to finish",
				strings.Join(leased, ", "), detail)
		}
	}
	res, err := e.T.Run(ctx, atomicEpochWriteCmd(e.epochPath(), epoch))
	if err != nil {
		e.ReleaseLock(ctx)
		return 0, fmt.Errorf("persist epoch: %w", err)
	}
	if res.ExitCode != 0 {
		e.ReleaseLock(ctx)
		return 0, fmt.Errorf("persist epoch: %s", strings.TrimSpace(res.Stderr))
	}
	return epoch, nil
}

func (e *Engine) ReleaseLock(_ context.Context) {
	if e.lockVal != "" && e.releaseRemoteLock(e.lockPath(), e.lockVal, "lock") {
		e.lockVal = ""
	}
}

func (e *Engine) lockTTL() time.Duration {
	if e.Opts.LockTTL > 0 {
		return e.Opts.LockTTL
	}
	return 10 * time.Minute
}

// StartHeartbeat keeps the lock fresh while the deploy runs; a crashed
// runner stops touching it and the TTL frees the lock. A refresh failure is
// surfaced so it doesn't resurface later as a baffling ErrFenced.
func (e *Engine) StartHeartbeat(ctx context.Context) (stop func()) {
	return e.startLockRefresher(ctx, e.lockTTL()/10, "heartbeat: lock refresh failed", e.refreshLock)
}

// refreshLock refreshes the lock's mtime, but only while the fence still names
// this runner and only if the lock already exists — `touch -c` never creates,
// so a lock another runner deleted on takeover is never resurrected. Exit
// lockNotOwnedExitCode means the fence is no longer ours (a newer deploy
// re-fenced us): a no-op, not a failure.
func (e *Engine) refreshLock(ctx context.Context) (transport.Result, error) {
	return e.T.Run(ctx, "if "+fileHolds(e.fencePath(), e.fenceVal)+"; then touch -c "+q(e.lockPath())+"; else exit "+strconv.Itoa(lockNotOwnedExitCode)+"; fi")
}

// WriteFence stamps the host with this deploy's identity. Every mutating
// command checks it host-side (mutate); a newer deploy re-stamps and the old
// runner's next mutation dies with fencedExitCode — locally, no cross-host call.
func (e *Engine) WriteFence(ctx context.Context, deployID string, epoch int) error {
	if e.lockVal == "" {
		return fmt.Errorf("write fence: app lock is not owned")
	}
	val := deployID + " " + strconv.Itoa(epoch)
	res, err := e.T.Run(ctx, guardedCmd(fileHolds(e.lockPath(), e.lockVal), "echo "+q(val)+" > "+q(e.fencePath()), "onebox-lock-lost", lockLostExitCode))
	if err != nil {
		return err
	}
	if refusedBy(res, lockLostExitCode, "onebox-lock-lost") {
		return ErrFenced
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write fence: %s", res.Stderr)
	}
	e.fenceVal = val
	return nil
}

func (e *Engine) fenceGuarded(cmd string) string {
	return guardedCmd(fileHolds(e.fencePath(), e.fenceVal), cmd, "onebox-fenced", fencedExitCode)
}

// mutate runs a mutating command behind the fence guard. Read-only commands
// use e.T.Run directly.
func (e *Engine) mutate(ctx context.Context, cmd string) (res transport.Result, err error) {
	if e.fenceVal == "" {
		return e.T.Run(ctx, cmd)
	}
	res, err = e.T.Run(ctx, e.fenceGuarded(cmd))
	if err == nil && refusedBy(res, fencedExitCode, "onebox-fenced") {
		err = ErrFenced
	}
	return res, err
}

// mutateInput is mutate's stdin-carrying counterpart. Manifest/checkpoint
// bytes never appear in the remote command or process list, while the fence
// still rejects a stale runner before the writer consumes stdin.
func (e *Engine) mutateInput(ctx context.Context, cmd, input string) (res transport.Result, err error) {
	if e.fenceVal == "" {
		return e.T.RunInput(ctx, cmd, input)
	}
	res, err = e.T.RunInput(ctx, e.fenceGuarded(cmd), input)
	if err == nil && refusedBy(res, fencedExitCode, "onebox-fenced") {
		err = ErrFenced
	}
	return res, err
}

// mutateStream is mutate's streaming counterpart. It preserves stdout and
// stderr separation while refusing to start the command after fence ownership
// changes.
func (e *Engine) mutateStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	if e.fenceVal == "" {
		return e.T.RunStream(ctx, cmd, stdout, stderr)
	}
	return e.T.RunStream(ctx, e.fenceGuarded(cmd), stdout, stderr)
}

// mutateChecked is the default for mutations whose success is required. The
// transport reports a remote command's exit status separately from Go errors,
// so checking only err can otherwise turn a refused mutation into success.
func (e *Engine) mutateChecked(ctx context.Context, action, cmd string) error {
	res, err := e.mutate(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode == 0 {
		return nil
	}
	detail := strings.TrimSpace(res.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(res.Stdout)
	}
	if detail == "" {
		return fmt.Errorf("%s failed (exit %d)", action, res.ExitCode)
	}
	return fmt.Errorf("%s failed (exit %d): %s", action, res.ExitCode, detail)
}

func sanitizeID(s string) string {
	if !validID.MatchString(strings.ReplaceAll(s, "-", "")) {
		return "invalid"
	}
	return s
}

// DeployInProgressError refuses to revert a deploy whose runner is still
// refreshing the deploy lock: the journal cannot tell an interrupted deploy
// from one that is running, but the heartbeat can.
type DeployInProgressError struct {
	DeployID   string
	AgeSeconds int
}

func (err *DeployInProgressError) Error() string {
	return fmt.Sprintf("deploy %s is still running (its lock was refreshed %ds ago) — wait for it to finish, then `ob rollback`; use --break-lock only if you know its runner is gone", err.DeployID, err.AgeSeconds)
}

func (err *DeployInProgressError) Code() string { return "deploy_in_progress" }

// liveHeartbeatWindow is how fresh a deploy lock must be to count as held by
// a running deploy: three missed heartbeats (each lockTTL/10) mean its runner
// has stopped.
func (e *Engine) liveHeartbeatWindow() int {
	return int((3 * e.lockTTL() / 10).Seconds())
}

// refuseLiveDeploy returns DeployInProgressError when deployID itself holds a
// recently refreshed deploy lock. Anything else — no lock, another holder, a
// holder that cannot be read — is left to lock acquisition to judge.
func (e *Engine) refuseLiveDeploy(ctx context.Context, deployID string) error {
	holder, present, invalid, err := remoteLock[lockMeta]{label: "deploy lock", path: e.lockPath()}.observe(ctx, e)
	if err != nil {
		return err
	}
	if !present || invalid != nil || holder.Meta.DeployID != deployID {
		return nil
	}
	if holder.Age <= e.liveHeartbeatWindow() {
		return &DeployInProgressError{DeployID: deployID, AgeSeconds: holder.Age}
	}
	return nil
}
