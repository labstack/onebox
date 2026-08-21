package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/transport"
)

var (
	ErrBackupConflict = errors.New("backup_conflict")
	ErrBackupFenced   = errors.New("backup operation fenced by a newer owner")
	backupIdentity    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
)

const backupLockPollInterval = 100 * time.Millisecond

type backupLockMeta struct {
	Owner       string `json:"owner"`
	OperationID string `json:"operation_id"`
	Service     string `json:"service"`
	Epoch       int    `json:"epoch"`
	TTLSeconds  int    `json:"ttl_s"`
	AcquiredAt  string `json:"acquired_at"`
}

// BackupConflictError is safe to serialize as a lifecycle failure. The
// code is stable and the holder identity is operational metadata, never a
// command, credential, or database value.
type BackupConflictError struct {
	Service     string
	OperationID string
	AgeSeconds  int
}

func (err *BackupConflictError) Error() string {
	return fmt.Sprintf("backup_conflict: service %s is held by operation %s (age %ds)", err.Service, err.OperationID, err.AgeSeconds)
}

func (err *BackupConflictError) Unwrap() error { return ErrBackupConflict }
func (err *BackupConflictError) Code() string  { return "backup_conflict" }
func (err *BackupConflictError) Retryable() bool {
	return true
}

func (e *Engine) backupLockDir() string { return e.base() + "/backup/locks" }
func (e *Engine) backupLockPath(service string) string {
	return e.backupLockDir() + "/" + service + ".lock"
}
func (e *Engine) backupEpochPath(service string) string {
	return e.backupLockDir() + "/" + service + ".epoch"
}
func (e *Engine) backupFencePath(service string) string {
	return e.backupLockDir() + "/" + service + ".fence"
}

// AcquireBackupLock acquires the per-service lock beneath the application
// lock. wait is a bounded contention budget; expiry returns backup_conflict.
// An expired lock or the same operation identity is reclaimed with a new epoch,
// fencing the former runner.
func (e *Engine) AcquireBackupLock(ctx context.Context, service, operationID string, wait time.Duration) (int, error) {
	if e.lockVal == "" {
		return 0, errors.New("backup lock requires the application lock")
	}
	if !backupIdentity.MatchString(service) || !backupIdentity.MatchString(operationID) {
		return 0, errors.New("backup lock service and operation identity are invalid")
	}
	if wait < 0 {
		return 0, errors.New("backup lock wait must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if res, err := e.T.Run(ctx, "mkdir -p "+q(e.backupLockDir())); err != nil {
		return 0, err
	} else if res.ExitCode != 0 {
		return 0, fmt.Errorf("create backup lock directory: %s", strings.TrimSpace(res.Stderr))
	}

	maxAttempts := 1
	if wait > 0 {
		maxAttempts += int((wait + backupLockPollInterval - 1) / backupLockPollInterval)
	}
	var conflict *BackupConflictError
	staleReclaims := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		epoch, err := e.nextBackupEpoch(ctx, service)
		if err != nil {
			return 0, err
		}
		meta := backupLockMeta{
			Owner: journal.DefaultOperator(), OperationID: operationID, Service: service,
			Epoch: epoch, TTLSeconds: int(e.lockTTL().Seconds()), AcquiredAt: e.Opts.Now().UTC().Format(time.RFC3339),
		}
		encoded, _ := json.Marshal(meta)
		lockValue := string(encoded)
		create := "set -C; echo " + q(lockValue) + " > " + q(e.backupLockPath(service)) + " 2>/dev/null"
		res, err := e.T.Run(ctx, create)
		if err != nil {
			return 0, err
		}
		if res.ExitCode == 0 {
			if e.backupLockVals == nil {
				e.backupLockVals = make(map[string]string)
				e.backupFenceVals = make(map[string]string)
			}
			e.backupLockVals[service] = lockValue
			if err := e.writeBackupFence(ctx, service, operationID, epoch, lockValue); err != nil {
				e.ReleaseBackupLock(service)
				return 0, err
			}
			return epoch, nil
		}

		observedResult, err := e.T.Run(ctx, "cat "+q(e.backupLockPath(service))+" 2>/dev/null || true")
		if err != nil {
			return 0, err
		}
		observed := strings.TrimSpace(observedResult.Stdout)
		if observed == "" {
			continue
		}
		var holder backupLockMeta
		_ = json.Unmarshal([]byte(observed), &holder)
		ageResult, err := e.T.Run(ctx, lockAgeCmd(e.backupLockPath(service)))
		if err != nil {
			return 0, err
		}
		age, _ := strconv.Atoi(strings.TrimSpace(ageResult.Stdout))
		if age > int(e.lockTTL().Seconds()) || holder.OperationID == operationID {
			if staleReclaims >= 4 {
				return 0, errors.New("could not reclaim stale backup lock")
			}
			staleReclaims++
			removeObserved := `if [ "$(cat ` + q(e.backupLockPath(service)) + ` 2>/dev/null)" = ` + q(observed) + ` ]; then rm -f ` + q(e.backupLockPath(service)) + `; else exit 75; fi`
			removed, err := e.T.Run(ctx, removeObserved)
			if err != nil {
				return 0, err
			}
			if removed.ExitCode == 0 || removed.ExitCode == 75 {
				attempt-- // a stale-holder race does not consume contention budget
				continue
			}
			return 0, fmt.Errorf("reclaim backup lock: %s", strings.TrimSpace(removed.Stderr))
		}
		conflict = &BackupConflictError{Service: service, OperationID: safeBackupHolder(holder.OperationID), AgeSeconds: age}
		if attempt+1 < maxAttempts {
			e.Opts.Sleep(backupLockPollInterval)
		}
	}
	if conflict == nil {
		conflict = &BackupConflictError{Service: service, OperationID: "unknown", AgeSeconds: 0}
	}
	return 0, conflict
}

func (e *Engine) nextBackupEpoch(ctx context.Context, service string) (int, error) {
	return e.nextEpoch(ctx, e.backupEpochPath(service))
}

func (e *Engine) writeBackupFence(ctx context.Context, service, operationID string, epoch int, lockValue string) error {
	fenceValue := operationID + " " + strconv.Itoa(epoch)
	command := `if [ "$(cat ` + q(e.backupLockPath(service)) + ` 2>/dev/null)" = ` + q(lockValue) + ` ]; then ` +
		atomicEpochWriteCmd(e.backupEpochPath(service), epoch) + `; echo ` + q(fenceValue) + ` > ` + q(e.backupFencePath(service)) +
		`; else echo ob-backup-lock-lost >&2; exit 96; fi`
	result, err := e.T.Run(ctx, command)
	if err != nil {
		return err
	}
	if result.ExitCode == 96 && strings.Contains(result.Stderr, "ob-backup-lock-lost") {
		return ErrBackupFenced
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write backup fence: %s", strings.TrimSpace(result.Stderr))
	}
	if e.backupLockVals == nil {
		e.backupLockVals = make(map[string]string)
		e.backupFenceVals = make(map[string]string)
	}
	e.backupLockVals[service] = lockValue
	e.backupFenceVals[service] = fenceValue
	return nil
}

func (e *Engine) ReleaseBackupLock(service string) {
	expected := e.backupLockVals[service]
	if expected == "" {
		return
	}
	cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := e.T.Run(cleanupContext, `if [ "$(cat `+q(e.backupLockPath(service))+` 2>/dev/null)" = `+q(expected)+` ]; then rm -f `+q(e.backupLockPath(service))+`; fi`)
	if err != nil || result.ExitCode != 0 {
		e.warnf("release backup lock failed: %v %s", err, strings.TrimSpace(result.Stderr))
		return
	}
	delete(e.backupLockVals, service)
	delete(e.backupFenceVals, service)
}

// StartBackupHeartbeat keeps a service lock fresh only while both its
// exact lock value and fence still belong to this runner.
func (e *Engine) StartBackupHeartbeat(ctx context.Context, service string) (func(), error) {
	lockValue := e.backupLockVals[service]
	fenceValue := e.backupFenceVals[service]
	if lockValue == "" || fenceValue == "" {
		return nil, errors.New("backup heartbeat requires service lock ownership")
	}
	heartbeatContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(e.lockTTL() / 10)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatContext.Done():
				return
			case <-ticker.C:
				command := `if [ "$(cat ` + q(e.backupLockPath(service)) + ` 2>/dev/null)" = ` + q(lockValue) + ` ] && [ "$(cat ` + q(e.backupFencePath(service)) + ` 2>/dev/null)" = ` + q(fenceValue) + ` ]; then touch -c ` + q(e.backupLockPath(service)) + `; else exit 3; fi`
				if result, err := e.T.Run(heartbeatContext, command); err == nil && result.ExitCode != 0 && result.ExitCode != 3 {
					e.warnf("backup heartbeat for %s failed (exit %d): %s", service, result.ExitCode, strings.TrimSpace(result.Stderr))
				}
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}

// BackupMutate nests the exact service lock/fence guard inside the app
// fence guard. A runner that loses either authority cannot mutate service data.
func (e *Engine) BackupMutate(ctx context.Context, service, command string) (transport.Result, error) {
	lockValue := e.backupLockVals[service]
	fenceValue := e.backupFenceVals[service]
	if e.lockVal == "" || e.fenceVal == "" || lockValue == "" || fenceValue == "" {
		return transport.Result{}, errors.New("backup mutation requires application and service lock ownership")
	}
	guarded := `if [ "$(cat ` + q(e.backupLockPath(service)) + ` 2>/dev/null)" = ` + q(lockValue) + ` ] && [ "$(cat ` + q(e.backupFencePath(service)) + ` 2>/dev/null)" = ` + q(fenceValue) + ` ]; then ` + command + `; else echo ob-backup-fenced >&2; exit 98; fi`
	result, err := e.mutate(ctx, guarded)
	if err != nil {
		return result, err
	}
	if result.ExitCode == 98 && strings.Contains(result.Stderr, "ob-backup-fenced") {
		return result, ErrBackupFenced
	}
	return result, nil
}

func safeBackupHolder(value string) string {
	if backupIdentity.MatchString(value) {
		return value
	}
	return "unknown"
}
