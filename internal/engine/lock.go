package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/release"
	"github.com/labstack/yeet/internal/transport"
)

// ErrFenced: a mutating command was refused host-side because a newer deploy
// owns this host (design §05 — the zombie runner is rejected locally).
var ErrFenced = errors.New("fenced: a newer deploy owns this host — this runner is stale")

type lockMeta struct {
	Owner      string `json:"owner"`
	DeployID   string `json:"deploy_id"`
	Epoch      int    `json:"epoch"`
	GitSHA     string `json:"git_sha,omitempty"`
	ConfigHash string `json:"config_hash,omitempty"`
	TTLSeconds int    `json:"ttl_s"`
	AcquiredAt string `json:"acquired_at"`
}

func (e *Engine) base() string      { return release.PathsFor(e.Cfg.App).Base }
func (e *Engine) lockPath() string  { return e.base() + "/lock" }
func (e *Engine) epochPath() string { return e.base() + "/epoch" }
func (e *Engine) fencePath() string { return e.base() + "/fence" }

// AcquireLock takes the deploy lock at the authority (v1: the only host).
// Epoch is bumped under the winner. TTL runs on the HOST's clock; a fresh
// lock refuses (unless force, which prints the holder + its journal tail —
// the operator sees who they are trampling).
func (e *Engine) AcquireLock(ctx context.Context, deployID string, force bool) (int, error) {
	res, err := e.T.Run(ctx, "mkdir -p "+q(e.base())+" && cat "+q(e.epochPath())+" 2>/dev/null || echo 0")
	if err != nil {
		return 0, err
	}
	prev, _ := strconv.Atoi(strings.TrimSpace(res.Stdout))
	epoch := prev + 1

	meta := lockMeta{
		Owner: journal.DefaultOperator(), DeployID: deployID, Epoch: epoch,
		TTLSeconds: int(e.lockTTL().Seconds()), AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(meta)
	// noclobber: the remote shell refuses the redirect if the lock exists
	create := "set -C; echo " + q(string(b)) + " > " + q(e.lockPath()) + " 2>/dev/null"

	for attempt := 0; attempt < 2; attempt++ {
		res, err := e.T.Run(ctx, create)
		if err != nil {
			return 0, err
		}
		if res.ExitCode == 0 {
			if res, err := e.T.Run(ctx, "echo "+strconv.Itoa(epoch)+" > "+q(e.epochPath())); err != nil || res.ExitCode != 0 {
				return 0, fmt.Errorf("persist epoch: %v %s", err, res.Stderr)
			}
			return epoch, nil
		}
		if attempt == 1 {
			break
		}
		// held — inspect holder + age
		hres, err := e.T.Run(ctx, "cat "+q(e.lockPath())+" 2>/dev/null || true")
		if err != nil {
			return 0, err
		}
		var holder lockMeta
		_ = json.Unmarshal([]byte(strings.TrimSpace(hres.Stdout)), &holder)
		ares, err := e.T.Run(ctx, "echo $(( $(date +%s) - $(stat -c %Y "+q(e.lockPath())+" 2>/dev/null || echo 0) ))")
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
		if res, err := e.T.Run(ctx, "rm -f "+q(e.lockPath())); err != nil || res.ExitCode != 0 {
			return 0, fmt.Errorf("break lock: %v %s", err, res.Stderr)
		}
	}
	return 0, fmt.Errorf("could not acquire deploy lock")
}

func (e *Engine) ReleaseLock(ctx context.Context) {
	_, _ = e.T.Run(ctx, "rm -f "+q(e.lockPath()))
}

func (e *Engine) lockTTL() time.Duration {
	if e.Opts.LockTTL > 0 {
		return e.Opts.LockTTL
	}
	return 10 * time.Minute
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
				_, _ = e.T.Run(hctx, "touch "+q(e.lockPath()))
			}
		}
	}()
	return func() { cancel(); <-done }
}

// WriteFence stamps the host with this deploy's identity. Every mutating
// command checks it host-side (mutate); a newer deploy re-stamps and the old
// runner's next mutation dies with exit 97 — locally, no cross-host call.
func (e *Engine) WriteFence(ctx context.Context, deployID string, epoch int) error {
	val := deployID + " " + strconv.Itoa(epoch)
	res, err := e.T.Run(ctx, "echo "+q(val)+" > "+q(e.fencePath()))
	if err != nil {
		return err
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
	guarded := `if [ "$(cat ` + q(e.fencePath()) + ` 2>/dev/null)" = ` + q(e.fenceVal) + ` ]; then ` + cmd + `; else echo yeet-fenced >&2; exit 97; fi`
	res, err = e.T.Run(ctx, guarded)
	if err != nil {
		return res, err
	}
	if res.ExitCode == 97 && strings.Contains(res.Stderr, "yeet-fenced") {
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
