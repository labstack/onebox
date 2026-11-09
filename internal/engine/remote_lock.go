package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// Exit statuses of the remote lock protocol. Commands that run under flock
// normalize expected collisions so their status does not depend on shell or
// flock defaults; util-linux reserves 64–78 for its own errors, so the create
// sentinels sit above that range and stay distinct from infrastructure
// failures. The kernel-lock conflict status is release.FlockConflictExitCode.
const (
	lockHeldExitCode         = 79 // create: the lock path already exists
	lockCreateFailedExitCode = 80 // create: metadata could not be written or published
	lockChangedExitCode      = 75 // compare-and-remove: the holder changed since it was observed
	lockNotOwnedExitCode     = 3  // refresh: lock or fence is no longer ours — a no-op
	lockLostExitCode         = 96 // guarded write: the lock is no longer ours
	fencedExitCode           = 97 // app fence guard refused a stale runner
	backupFencedExitCode     = 98 // backup lock/fence guard refused a stale runner
)

// invalidHolderRetries bounds how often an empty, corrupt or unaged holder is
// re-read before it is refused: a legacy noclobber writer may be mid-write,
// but a damaged file does not heal by polling it.
const invalidHolderRetries = 2

// lockReleaseTimeout bounds best-effort release. It runs on a fresh context so
// a cancelled operation still gives its lock back.
const lockReleaseTimeout = 5 * time.Second

// fileHolds is the shell condition "path contains exactly value". Every
// ownership check — release, refresh, fence guards — is this comparison.
func fileHolds(path, value string) string {
	return `[ "$(cat ` + q(path) + ` 2>/dev/null)" = ` + q(value) + ` ]`
}

// guardedCmd runs cmd only while cond holds; otherwise it reports marker on
// stderr and exits code, so callers can tell the refusal from cmd's failure.
func guardedCmd(cond, cmd, marker string, code int) string {
	return "if " + cond + "; then " + cmd + "; else echo " + marker + " >&2; exit " + strconv.Itoa(code) + "; fi"
}

// refusedBy reports whether res is guardedCmd's refusal rather than the
// guarded command's own failure.
func refusedBy(res transport.Result, code int, marker string) bool {
	return res.ExitCode == code && strings.Contains(res.Stderr, marker)
}

type lockVerdict int

const (
	lockRefuse    lockVerdict = iota // return the error now
	lockReclaim                      // the holder may be removed; remove it and retry
	lockContended                    // keep the error, pause, and retry while attempts remain
)

// lockHolder is a readable holder: its decoded metadata, exact bytes (the
// compare-and-remove token) and age on the host's clock.
type lockHolder[H any] struct {
	Meta H
	Raw  string
	Age  int
}

// remoteLock is the one exclusive lock-file protocol on the host: publish
// complete metadata with a no-replace hard link; on conflict read the holder
// and its age; let the lock decide whether that holder is reclaimable; remove
// it only if it is unchanged; retry. The application, host and backup locks
// differ only in their metadata, what makes a holder reclaimable, and what
// they do once they own the file.
type remoteLock[H any] struct {
	label string // "deploy lock", used in messages
	path  string
	// ttl, when positive, lets a holder whose content cannot be read be
	// reclaimed once it is older than this many seconds. Nothing refreshes
	// such a file on its owner's behalf — a heartbeat touches the lock only
	// while its content or its fence still names the runner — so past the
	// TTL it is abandoned, the same judgement made of a readable holder.
	ttl      int
	dir      string // created before each attempt when set
	attempts int
	// freeReclaims, when positive, lets reclaiming a stale holder not consume
	// an attempt, at most this many times (a contention budget that only
	// counts live holders).
	freeReclaims int
	// force is the operator's --break-lock. It is the only thing that may
	// remove a holder whose content or age cannot be read.
	force     bool
	breakHint string // how an operator clears a holder that cannot be trusted
	pause     func() // between contended attempts
	// next returns this attempt's lock value; it is called afresh each
	// attempt so per-attempt state (an epoch) is never reused.
	next func(context.Context) (string, error)
	// wrapCreate optionally runs the create inside another lock.
	wrapCreate func(string) string
	// createFailed maps a create status other than success or held.
	createFailed func(transport.Result) error
	// decide classifies a readable holder.
	decide func(lockHolder[H]) (lockVerdict, error)
}

// lockNotAcquiredError: every attempt raced a holder that came and went.
type lockNotAcquiredError struct{ label string }

func (err *lockNotAcquiredError) Error() string { return "could not acquire " + err.label }

// acquire returns the published lock value.
func (l remoteLock[H]) acquire(ctx context.Context, e *Engine) (string, error) {
	var contended error
	invalidSeen, reclaims := 0, 0
	for attempt := 0; attempt < l.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		value, err := l.next(ctx)
		if err != nil {
			return "", err
		}
		create := atomicLockCreateCmd(l.path, value)
		if l.dir != "" {
			create = "mkdir -p " + q(l.dir) + " || exit " + strconv.Itoa(lockCreateFailedExitCode) + "; " + create
		}
		if l.wrapCreate != nil {
			create = l.wrapCreate(create)
		}
		res, err := e.T.Run(ctx, create)
		if err != nil {
			return "", err
		}
		if res.ExitCode == 0 {
			return value, nil
		}
		if res.ExitCode != lockHeldExitCode {
			if l.createFailed != nil {
				return "", l.createFailed(res)
			}
			return "", fmt.Errorf("acquire %s: lock creation failed (exit %d): %s", l.label, res.ExitCode, diagnostic(res))
		}

		holder, present, invalid, err := l.observe(ctx, e)
		if err != nil {
			return "", err
		}
		if !present {
			continue // released between our create and our read
		}
		if invalid != nil && l.ttl > 0 && holder.Age > l.ttl {
			e.logf("%s: reclaiming a holder that cannot be read and is %ds past its TTL (%v)", l.label, holder.Age-l.ttl, invalid)
		} else if invalid != nil {
			if !l.force {
				invalidSeen++
				if invalidSeen <= invalidHolderRetries && attempt+1 < l.attempts {
					continue
				}
				return "", fmt.Errorf("%s at %s cannot be trusted: %v — refusing to reclaim it; %s", l.label, l.path, invalid, l.breakHint)
			}
			e.logf("%s: FORCE-breaking a holder that cannot be read (%v)", l.label, invalid)
		} else {
			verdict, err := l.decide(holder)
			switch verdict {
			case lockRefuse:
				return "", err
			case lockContended:
				contended = err
				if attempt+1 < l.attempts && l.pause != nil {
					l.pause()
				}
				continue
			}
		}

		if l.freeReclaims > 0 {
			if reclaims >= l.freeReclaims {
				return "", fmt.Errorf("could not reclaim stale %s", l.label)
			}
			reclaims++
			attempt-- // a stale-holder race does not consume contention budget
		}
		// Remove only the exact holder we judged; a different one (exit
		// lockChangedExitCode) is judged afresh on the next attempt.
		removed, err := e.T.Run(ctx, "if "+fileHolds(l.path, holder.Raw)+"; then rm -f "+q(l.path)+"; else exit "+strconv.Itoa(lockChangedExitCode)+"; fi")
		if err != nil {
			return "", fmt.Errorf("reclaim %s: %w", l.label, err)
		}
		if removed.ExitCode != 0 && removed.ExitCode != lockChangedExitCode {
			return "", fmt.Errorf("reclaim %s: %s", l.label, diagnostic(removed))
		}
	}
	if contended != nil {
		return "", contended
	}
	return "", &lockNotAcquiredError{label: l.label}
}

// observe reads the holder. present is false only when the lock is gone. A
// present holder whose bytes are empty or not metadata, or whose age is not a
// number, is returned with invalid set: it is never reclaimed on identity
// grounds, which cannot be established. Its age is still measured, so an
// abandoned one can age out past the lock's TTL; an unmeasurable age is 0.
func (l remoteLock[H]) observe(ctx context.Context, e *Engine) (holder lockHolder[H], present bool, invalid, err error) {
	qpath := q(l.path)
	res, err := e.T.Run(ctx, "if [ -e "+qpath+" ] || [ -L "+qpath+" ]; then cat "+qpath+" 2>/dev/null; exit 0; fi; exit "+strconv.Itoa(app.ProbeAbsent))
	if err != nil {
		return holder, false, nil, err
	}
	switch res.ExitCode {
	case 0:
	case app.ProbeAbsent:
		return holder, false, nil, nil
	default:
		return holder, false, nil, fmt.Errorf("read %s holder failed (exit %d): %s", l.label, res.ExitCode, diagnostic(res))
	}
	holder.Raw = strings.TrimSpace(res.Stdout)
	ageRes, err := e.T.Run(ctx, lockAgeCmd(l.path))
	if err != nil {
		return holder, true, nil, err
	}
	rawAge := strings.TrimSpace(ageRes.Stdout)
	if holder.Age, err = strconv.Atoi(rawAge); err != nil {
		// Age 0 is what every unestablished age means elsewhere: fail closed,
		// never past a TTL.
		holder.Age = 0
		return holder, true, fmt.Errorf("lock age %q is not a number (exit %d: %s)", rawAge, ageRes.ExitCode, diagnostic(ageRes)), nil
	}
	if holder.Raw == "" {
		return holder, true, errors.New("the lock file is empty or unreadable (a write that failed after creating it, for example on a full disk, leaves it so)"), nil
	}
	if decodeErr := json.Unmarshal([]byte(holder.Raw), &holder.Meta); decodeErr != nil {
		return holder, true, fmt.Errorf("holder metadata is not valid JSON (%v)", decodeErr), nil
	}
	return holder, true, nil, nil
}

func diagnostic(res transport.Result) string {
	if detail := strings.TrimSpace(res.Stderr); detail != "" {
		return detail
	}
	return "no diagnostic output"
}

// releaseRemoteLock removes path only while it still holds value. A lock
// another runner already took over is left alone.
func (e *Engine) releaseRemoteLock(path, value, label string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), lockReleaseTimeout)
	defer cancel()
	res, err := e.T.Run(ctx, "if "+fileHolds(path, value)+"; then rm -f "+q(path)+"; fi")
	if err != nil || res.ExitCode != 0 {
		e.warnf("release %s failed: %v %s", label, err, strings.TrimSpace(res.Stderr))
		return false
	}
	return true
}

// startLockRefresher runs refresh every interval until stopped. A refresh
// that exits lockNotOwnedExitCode (ownership moved on) is expected; any other
// non-zero status is a genuine failure (transport down, EACCES, full disk)
// that leaves the lock silently going stale, so it is surfaced. A transport
// error during shutdown is not a failure.
func (e *Engine) startLockRefresher(ctx context.Context, interval time.Duration, failure string, refresh func(context.Context) (transport.Result, error)) (stop func()) {
	hctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				if res, err := refresh(hctx); err == nil && res.ExitCode != 0 && res.ExitCode != lockNotOwnedExitCode {
					e.warnf("%s (exit %d): %s", failure, res.ExitCode, strings.TrimSpace(res.Stderr))
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

// atomicLockCreateCmd writes complete metadata before publishing the lock
// path. A noclobber redirect (`set -C; echo … > lock`) creates the lock before
// its write, so a failed write (for example on ENOSPC) leaves an EMPTY lock
// that looks like contention and blocks every later runner. A same-directory
// hard link is an atomic no-replace claim, and removing the temporary name
// leaves the claimed inode at lockPath. Exit lockHeldExitCode means the path
// exists; anything else is an infrastructure failure.
func atomicLockCreateCmd(lockPath, value string) string {
	tmpPattern := lockPath + ".candidate.XXXXXX"
	failed := strconv.Itoa(lockCreateFailedExitCode)
	return "umask 077; tmp=$(mktemp " + q(tmpPattern) + ") || exit " + failed + "; " +
		"cleanup() { rm -f \"$tmp\" || true; }; trap cleanup 0; trap 'exit 129' 1; trap 'exit 130' 2; trap 'exit 143' 15; " +
		"printf '%s\\n' " + q(value) + " >\"$tmp\" || exit " + failed + "; " +
		// Unlike ln, the POSIX link utility treats its second operand as the
		// exact new path even when that path names a directory.
		"if link \"$tmp\" " + q(lockPath) + "; then exit 0; fi; " +
		"{ [ -e " + q(lockPath) + " ] || [ -L " + q(lockPath) + " ]; } && exit " + strconv.Itoa(lockHeldExitCode) + "; exit " + failed
}

// lockAgeCmd prints a lock file's age in seconds, structured so every caller
// (age > ttl → reclaimable, else refuse) fails CLOSED wherever the shell can
// observe the lock:
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
// by `-e`/`-L` and refused rather than misread as absent. Absence itself is
// established rather than assumed: if a directory on the way is unsearchable,
// neither stat nor `-e`/`-L` can observe the lock, and that used to read as
// absent and be taken over (fail open). UndeterminedArm now turns it into age
// 0, so the guarantee is "fail closed", not "fail closed while observable".
//
// `stat -c %Y` is GNU; `stat -f %m` is the BSD/macOS fallback (the e2e suite
// drives a macOS box through the Local transport). path is quoted here, so
// callers pass it raw.
func lockAgeCmd(path string) string {
	qpath := q(path)
	// A dangling symlink is "present but not resolvable" → fail closed (age 0).
	// Detect it FIRST and portably: `test -e` follows the link (false when the
	// target is gone) while `test -L` stays true. Relying on stat to fail here is
	// NOT portable — BSD stat (macOS) lstat's a symlink and returns the LINK's
	// own mtime, so a dangling lock would read as fresh. GNU stat dereferences.
	return "if [ -L " + qpath + " ] && [ ! -e " + qpath + " ]; then echo 0; " +
		"elif M=$(stat -c %Y " + qpath + " 2>/dev/null || stat -f %m " + qpath + " 2>/dev/null); then echo $(( $(date +%s) - M )); " +
		"elif [ -e " + qpath + " ] || [ -L " + qpath + " ]; then echo 0; " +
		// Absence has to be established, not assumed. If a directory on the
		// way cannot be searched, neither stat nor -e/-L can see the lock,
		// and answering "maximal age" hands the deploy a lock that may be
		// held right now — the one false absence whose cost is two
		// concurrent mutators rather than a misleading report. The arm runs
		// in a subshell so its refusal becomes age 0 (fail closed) rather
		// than an exit status this command's callers do not read.
		"else if ( " + app.UndeterminedArm(path) + "true ); then date +%s; else echo 0; fi; fi"
}
