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
// fencing the former runner. Backup locks do not expose --break-lock.
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
	ttl := int(e.lockTTL().Seconds())
	var epoch int
	lock := remoteLock[backupLockMeta]{
		label: "backup lock", path: e.backupLockPath(service), attempts: maxAttempts, freeReclaims: 4, ttl: ttl,
		breakHint: "confirm no backup or restore of " + service + " is running, then remove " + e.backupLockPath(service) + " by hand",
		pause:     func() { e.Opts.Sleep(backupLockPollInterval) },
		next: func(ctx context.Context) (string, error) {
			var err error
			if epoch, err = e.nextBackupEpoch(ctx, service); err != nil {
				return "", err
			}
			encoded, err := json.Marshal(backupLockMeta{
				Owner: journal.DefaultOperator(), OperationID: operationID, Service: service,
				Epoch: epoch, TTLSeconds: ttl, AcquiredAt: e.Opts.Now().UTC().Format(time.RFC3339),
			})
			return string(encoded), err
		},
		// Anything but an expired holder or this very operation is bounded,
		// retryable contention.
		decide: func(holder lockHolder[backupLockMeta]) (lockVerdict, error) {
			if holder.Age > ttl || holder.Meta.OperationID == operationID {
				return lockReclaim, nil
			}
			return lockContended, &BackupConflictError{Service: service, OperationID: safeBackupHolder(holder.Meta.OperationID), AgeSeconds: holder.Age}
		},
	}
	lockValue, err := lock.acquire(ctx, e)
	var raced *lockNotAcquiredError
	if errors.As(err, &raced) {
		// Every holder released before we could read it: still contention.
		err = &BackupConflictError{Service: service, OperationID: "unknown"}
	}
	if err != nil {
		return 0, err
	}
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

func (e *Engine) nextBackupEpoch(ctx context.Context, service string) (int, error) {
	return e.nextEpoch(ctx, e.backupEpochPath(service))
}

func (e *Engine) writeBackupFence(ctx context.Context, service, operationID string, epoch int, lockValue string) error {
	fenceValue := operationID + " " + strconv.Itoa(epoch)
	write := atomicEpochWriteCmd(e.backupEpochPath(service), epoch) + "; echo " + q(fenceValue) + " > " + q(e.backupFencePath(service))
	result, err := e.T.Run(ctx, guardedCmd(fileHolds(e.backupLockPath(service), lockValue), write, "onebox-backup-lock-lost", lockLostExitCode))
	if err != nil {
		return err
	}
	if refusedBy(result, lockLostExitCode, "onebox-backup-lock-lost") {
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
	if expected := e.backupLockVals[service]; expected != "" && e.releaseRemoteLock(e.backupLockPath(service), expected, "backup lock") {
		delete(e.backupLockVals, service)
		delete(e.backupFenceVals, service)
	}
}

// backupOwned is the shell condition that both the exact service lock value
// and its fence still belong to this runner.
func (e *Engine) backupOwned(service string) string {
	return fileHolds(e.backupLockPath(service), e.backupLockVals[service]) + " && " + fileHolds(e.backupFencePath(service), e.backupFenceVals[service])
}

// StartBackupHeartbeat keeps a service lock fresh only while both its
// exact lock value and fence still belong to this runner.
func (e *Engine) StartBackupHeartbeat(ctx context.Context, service string) (func(), error) {
	if e.backupLockVals[service] == "" || e.backupFenceVals[service] == "" {
		return nil, errors.New("backup heartbeat requires service lock ownership")
	}
	command := "if " + e.backupOwned(service) + "; then touch -c " + q(e.backupLockPath(service)) + "; else exit " + strconv.Itoa(lockNotOwnedExitCode) + "; fi"
	return e.startLockRefresher(ctx, e.lockTTL()/10, "backup heartbeat for "+service+" failed", func(ctx context.Context) (transport.Result, error) {
		return e.T.Run(ctx, command)
	}), nil
}

// BackupMutate nests the exact service lock/fence guard inside the app
// fence guard. A runner that loses either authority cannot mutate service data.
func (e *Engine) BackupMutate(ctx context.Context, service, command string) (transport.Result, error) {
	if e.lockVal == "" || e.fenceVal == "" || e.backupLockVals[service] == "" || e.backupFenceVals[service] == "" {
		return transport.Result{}, errors.New("backup mutation requires application and service lock ownership")
	}
	result, err := e.mutate(ctx, guardedCmd(e.backupOwned(service), command, "onebox-backup-fenced", backupFencedExitCode))
	if err == nil && refusedBy(result, backupFencedExitCode, "onebox-backup-fenced") {
		err = ErrBackupFenced
	}
	return result, err
}

func safeBackupHolder(value string) string {
	if backupIdentity.MatchString(value) {
		return value
	}
	return "unknown"
}
