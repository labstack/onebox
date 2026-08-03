package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (e *Engine) base() string      { return release.PathsFor(e.App.App).Base }
func (e *Engine) lockPath() string  { return e.base() + "/lock" }
func (e *Engine) epochPath() string { return e.base() + "/epoch" }
func (e *Engine) fencePath() string { return e.base() + "/fence" }

// AcquireLock takes the deploy lock at the authority (v1: the only host).
// Epoch is bumped under the winner. TTL runs on the HOST's clock; a fresh
// lock refuses (unless force, which prints the holder + its journal tail —
// the operator sees who they are trampling).
func (e *Engine) AcquireLock(ctx context.Context, deployID string, force bool) (int, error) {
	e.lockVal = ""
	if res, err := e.T.Run(ctx, "mkdir -p "+q(e.base())); err != nil {
		return 0, err
	} else if res.ExitCode != 0 {
		return 0, fmt.Errorf("mkdir %s: %s", e.base(), res.Stderr)
	}

	for attempt := 0; attempt < 4; attempt++ {
		// Read the epoch fresh on every attempt. A concurrent acquirer may have
		// persisted a higher epoch between our attempts; fencing relies on a
		// strictly increasing epoch, so a value read once before the loop could
		// be reused and collide.
		eres, err := e.T.Run(ctx, "cat "+q(e.epochPath())+" 2>/dev/null || echo 0")
		if err != nil {
			return 0, err
		}
		prev, _ := strconv.Atoi(strings.TrimSpace(eres.Stdout))
		epoch := prev + 1

		meta := lockMeta{
			Owner: journal.DefaultOperator(), DeployID: deployID, Epoch: epoch,
			TTLSeconds: int(e.lockTTL().Seconds()), AcquiredAt: time.Now().UTC().Format(time.RFC3339),
		}
		b, _ := json.Marshal(meta)
		// noclobber: the remote shell refuses the redirect if the lock exists
		create := "set -C; echo " + q(string(b)) + " > " + q(e.lockPath()) + " 2>/dev/null"

		res, err := e.T.Run(ctx, create)
		if err != nil {
			return 0, err
		}
		if res.ExitCode == 0 {
			e.lockVal = string(b)
			if res, err := e.T.Run(ctx, "echo "+strconv.Itoa(epoch)+" > "+q(e.epochPath())); err != nil || res.ExitCode != 0 {
				e.ReleaseLock(ctx)
				return 0, fmt.Errorf("persist epoch: %v %s", err, res.Stderr)
			}
			return epoch, nil
		}
		// held — inspect holder + age
		hres, err := e.T.Run(ctx, "cat "+q(e.lockPath())+" 2>/dev/null || true")
		if err != nil {
			return 0, err
		}
		observed := strings.TrimSpace(hres.Stdout)
		if observed == "" {
			continue
		}
		var holder lockMeta
		_ = json.Unmarshal([]byte(observed), &holder)
		ares, err := e.T.Run(ctx, lockAgeCmd(q(e.lockPath())))
		if err != nil {
			return 0, err
		}
		age, _ := strconv.Atoi(strings.TrimSpace(ares.Stdout))
		switch {
		case age > int(e.lockTTL().Seconds()):
			e.logf("lock: holder %s (deploy %s) expired %ds ago — taking over", holder.Owner, holder.DeployID, age-int(e.lockTTL().Seconds()))
		case holder.DeployID == deployID:
			// resume/abort of the very deploy that holds the lock: safe to
			// reclaim — the new epoch fences the previous runner regardless
			e.logf("lock: reclaiming for deploy %s (previous runner is fenced by the new epoch)", deployID)
		case force:
			e.logf("lock: FORCE-breaking %s (deploy %s, age %ds) — holder's journal tail:", holder.Owner, holder.DeployID, age)
			tres, _ := e.T.Run(ctx, "tail -5 "+q(e.base()+"/journal/"+sanitizeID(holder.DeployID)+".jsonl")+" 2>/dev/null || true")
			for _, l := range strings.Split(strings.TrimSpace(tres.Stdout), "\n") {
				if l != "" {
					e.logf("  %s", l)
				}
			}
		default:
			return 0, fmt.Errorf("deploy lock held by %s (deploy %s, age %ds, ttl %ds) — wait, or --force to break it",
				holder.Owner, holder.DeployID, age, int(e.lockTTL().Seconds()))
		}
		removeObserved := `if [ "$(cat ` + q(e.lockPath()) + ` 2>/dev/null)" = ` + q(observed) + ` ]; then rm -f ` + q(e.lockPath()) + `; else exit 75; fi`
		if res, err := e.T.Run(ctx, removeObserved); err != nil {
			return 0, fmt.Errorf("break lock: %w", err)
		} else if res.ExitCode == 75 {
			continue
		} else if res.ExitCode != 0 {
			return 0, fmt.Errorf("break lock: %v %s", err, res.Stderr)
		}
	}
	return 0, fmt.Errorf("could not acquire deploy lock")
}

func (e *Engine) ReleaseLock(ctx context.Context) {
	if e.lockVal == "" {
		return
	}
	expected := e.lockVal
	cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := e.T.Run(cleanupContext, `if [ "$(cat `+q(e.lockPath())+` 2>/dev/null)" = `+q(expected)+` ]; then rm -f `+q(e.lockPath())+`; fi`)
	if err != nil || res.ExitCode != 0 {
		e.warnf("release lock failed: %v %s", err, strings.TrimSpace(res.Stderr))
		return
	}
	e.lockVal = ""
}

func (e *Engine) lockTTL() time.Duration {
	if e.Opts.LockTTL > 0 {
		return e.Opts.LockTTL
	}
	return 10 * time.Minute
}

// lockAgeCmd prints a lock file's age in seconds, structured so BOTH callers
// (AcquireLock and acquireHostLock: age > ttl → take over, else refuse) fail
// CLOSED wherever the shell can observe the lock:
//
//   - dangling symlink     → age 0 → caller refuses; checked FIRST because BSD
//     stat can't detect it (see the inline note on -L/-e below)
//   - stat succeeds       → now − mtime, the real age
//   - stat fails, present  → age 0 → caller refuses; we won't break another
//     holder's lock we can't read (a torn/ESTALE stat)
//   - truly absent         → maximal age (`date +%s`) → take over (the holder
//     released it between our failed create and this check)
//
// Dangling-check first, then stat, so a present-but-unstattable lock is caught
// by `-e`/`-L` and refused rather than misread as absent. Limit: if the lock's PARENT dir is
// unsearchable, neither stat nor `-e`/`-L` can observe the lock, so it reads as
// absent and is taken over (fail open). Not a live path — the runner owns
// /var/lib/ob/<app> (Preflight asserts it writable) — so the guarantee is "fail
// closed while the lock is observable", not absolute.
//
// `stat -c %Y` is GNU; `stat -f %m` is the BSD/macOS fallback (the e2e suite
// drives a macOS box through the Local transport). qpath must already be
// shell-quoted. `--force` breaks the lock in every state (force precedes the
// refuse default in both callers); in AcquireLock a same-deploy holder is
// reclaimed rather than refused — breaking your own lock is authorized.
func lockAgeCmd(qpath string) string {
	// A dangling symlink is "present but not resolvable" → fail closed (age 0).
	// Detect it FIRST and portably: `test -e` follows the link (false when the
	// target is gone) while `test -L` stays true. Relying on stat to fail here is
	// NOT portable — BSD stat (macOS) lstat's a symlink and returns the LINK's
	// own mtime, so a dangling lock would read as fresh. GNU stat dereferences.
	return "if [ -L " + qpath + " ] && [ ! -e " + qpath + " ]; then echo 0; " +
		"elif M=$(stat -c %Y " + qpath + " 2>/dev/null || stat -f %m " + qpath + " 2>/dev/null); then echo $(( $(date +%s) - M )); " +
		"elif [ -e " + qpath + " ] || [ -L " + qpath + " ]; then echo 0; " +
		"else date +%s; fi"
}

// StartHeartbeat keeps the lock fresh while the deploy runs; a crashed
// runner stops touching it and the TTL frees the lock.
func (e *Engine) StartHeartbeat(ctx context.Context) (stop func()) {
	hctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(e.lockTTL() / 10)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				// A no-op (exit 3, fence no longer ours) is expected; any OTHER
				// non-zero is a genuine refresh failure (transport down, EACCES,
				// full disk) that leaves the lock silently going stale — surface it
				// so it doesn't resurface later as a baffling ErrFenced. A transport
				// error during shutdown (hctx cancelled) is not a failure.
				if res, err := e.refreshLock(hctx); err == nil && res.ExitCode != 0 && res.ExitCode != 3 {
					e.warnf("heartbeat: lock refresh failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

// refreshLock refreshes the lock's mtime, but only while the fence still names
// this runner and only if the lock already exists — `touch -c` never creates,
// so a lock another runner deleted on takeover is never resurrected. Exit 3
// means the fence is no longer ours (a newer deploy re-fenced us): a no-op, not
// a failure.
func (e *Engine) refreshLock(ctx context.Context) (transport.Result, error) {
	cmd := `if [ "$(cat ` + q(e.fencePath()) + ` 2>/dev/null)" = ` + q(e.fenceVal) + ` ]; then touch -c ` + q(e.lockPath()) + `; else exit 3; fi`
	return e.T.Run(ctx, cmd)
}

// WriteFence stamps the host with this deploy's identity. Every mutating
// command checks it host-side (mutate); a newer deploy re-stamps and the old
// runner's next mutation dies with exit 97 — locally, no cross-host call.
func (e *Engine) WriteFence(ctx context.Context, deployID string, epoch int) error {
	if e.lockVal == "" {
		return fmt.Errorf("write fence: app lock is not owned")
	}
	val := deployID + " " + strconv.Itoa(epoch)
	cmd := `if [ "$(cat ` + q(e.lockPath()) + ` 2>/dev/null)" = ` + q(e.lockVal) + ` ]; then echo ` + q(val) + ` > ` + q(e.fencePath()) + `; else echo ob-lock-lost >&2; exit 96; fi`
	res, err := e.T.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode == 96 && strings.Contains(res.Stderr, "ob-lock-lost") {
		return ErrFenced
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write fence: %s", res.Stderr)
	}
	e.fenceVal = val
	return nil
}

// mutate runs a mutating command behind the fence guard. Read-only commands
// use e.T.Run directly.
func (e *Engine) mutate(ctx context.Context, cmd string) (res transport.Result, err error) {
	if e.fenceVal == "" {
		return e.T.Run(ctx, cmd)
	}
	guarded := `if [ "$(cat ` + q(e.fencePath()) + ` 2>/dev/null)" = ` + q(e.fenceVal) + ` ]; then ` + cmd + `; else echo ob-fenced >&2; exit 97; fi`
	res, err = e.T.Run(ctx, guarded)
	if err != nil {
		return res, err
	}
	if res.ExitCode == 97 && strings.Contains(res.Stderr, "ob-fenced") {
		return res, ErrFenced
	}
	return res, nil
}

func sanitizeID(s string) string {
	if !validID.MatchString(strings.ReplaceAll(s, "-", "")) {
		return "invalid"
	}
	return s
}
