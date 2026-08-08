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
	ErrProtectionConflict = errors.New("backup_conflict")
	ErrProtectionFenced   = errors.New("protection operation fenced by a newer owner")
	protectionIdentity    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
)

const protectionLockPollInterval = 100 * time.Millisecond

type protectionLockMeta struct {
	Owner       string `json:"owner"`
	OperationID string `json:"operation_id"`
	Service     string `json:"service"`
	Epoch       int    `json:"epoch"`
	TTLSeconds  int    `json:"ttl_s"`
	AcquiredAt  string `json:"acquired_at"`
}

// ProtectionConflictError is safe to serialize as a lifecycle failure. The
// code is stable and the holder identity is operational metadata, never a
// command, credential, or database value.
type ProtectionConflictError struct {
	Service     string
	OperationID string
	AgeSeconds  int
}

func (err *ProtectionConflictError) Error() string {
	return fmt.Sprintf("backup_conflict: service %s is held by operation %s (age %ds)", err.Service, err.OperationID, err.AgeSeconds)
}

func (err *ProtectionConflictError) Unwrap() error { return ErrProtectionConflict }
func (err *ProtectionConflictError) Code() string  { return "backup_conflict" }
func (err *ProtectionConflictError) Retryable() bool {
	return true
}

func (e *Engine) protectionLockDir() string { return e.base() + "/protection/locks" }
func (e *Engine) protectionLockPath(service string) string {
	return e.protectionLockDir() + "/" + service + ".lock"
}
func (e *Engine) protectionEpochPath(service string) string {
	return e.protectionLockDir() + "/" + service + ".epoch"
}
func (e *Engine) protectionFencePath(service string) string {
	return e.protectionLockDir() + "/" + service + ".fence"
}

// AcquireProtectionLock acquires the per-service lock beneath the application
// lock. wait is a bounded contention budget; expiry returns backup_conflict.
// An expired lock or the same operation identity is reclaimed with a new epoch,
// fencing the former runner.
func (e *Engine) AcquireProtectionLock(ctx context.Context, service, operationID string, wait time.Duration) (int, error) {
	if e.lockVal == "" {
		return 0, errors.New("protection lock requires the application lock")
	}
	if !protectionIdentity.MatchString(service) || !protectionIdentity.MatchString(operationID) {
		return 0, errors.New("protection lock service and operation identity are invalid")
	}
	if wait < 0 {
		return 0, errors.New("protection lock wait must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if res, err := e.T.Run(ctx, "mkdir -p "+q(e.protectionLockDir())); err != nil {
		return 0, err
	} else if res.ExitCode != 0 {
		return 0, fmt.Errorf("create protection lock directory: %s", strings.TrimSpace(res.Stderr))
	}

	maxAttempts := 1
	if wait > 0 {
		maxAttempts += int((wait + protectionLockPollInterval - 1) / protectionLockPollInterval)
	}
	var conflict *ProtectionConflictError
	staleReclaims := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		epoch, err := e.nextProtectionEpoch(ctx, service)
		if err != nil {
			return 0, err
		}
		meta := protectionLockMeta{
			Owner: journal.DefaultOperator(), OperationID: operationID, Service: service,
			Epoch: epoch, TTLSeconds: int(e.lockTTL().Seconds()), AcquiredAt: e.Opts.Now().UTC().Format(time.RFC3339),
		}
		encoded, _ := json.Marshal(meta)
		lockValue := string(encoded)
		create := "set -C; echo " + q(lockValue) + " > " + q(e.protectionLockPath(service)) + " 2>/dev/null"
		res, err := e.T.Run(ctx, create)
		if err != nil {
			return 0, err
		}
		if res.ExitCode == 0 {
			if err := e.writeProtectionFence(ctx, service, operationID, epoch, lockValue); err != nil {
				return 0, err
			}
			return epoch, nil
		}

		observedResult, err := e.T.Run(ctx, "cat "+q(e.protectionLockPath(service))+" 2>/dev/null || true")
		if err != nil {
			return 0, err
		}
		observed := strings.TrimSpace(observedResult.Stdout)
		if observed == "" {
			continue
		}
		var holder protectionLockMeta
		_ = json.Unmarshal([]byte(observed), &holder)
		ageResult, err := e.T.Run(ctx, lockAgeCmd(q(e.protectionLockPath(service))))
		if err != nil {
			return 0, err
		}
		age, _ := strconv.Atoi(strings.TrimSpace(ageResult.Stdout))
		if age > int(e.lockTTL().Seconds()) || holder.OperationID == operationID {
			if staleReclaims >= 4 {
				return 0, errors.New("could not reclaim stale protection lock")
			}
			staleReclaims++
			removeObserved := `if [ "$(cat ` + q(e.protectionLockPath(service)) + ` 2>/dev/null)" = ` + q(observed) + ` ]; then rm -f ` + q(e.protectionLockPath(service)) + `; else exit 75; fi`
			removed, err := e.T.Run(ctx, removeObserved)
			if err != nil {
				return 0, err
			}
			if removed.ExitCode == 0 || removed.ExitCode == 75 {
				attempt-- // a stale-holder race does not consume contention budget
				continue
			}
			return 0, fmt.Errorf("reclaim protection lock: %s", strings.TrimSpace(removed.Stderr))
		}
		conflict = &ProtectionConflictError{Service: service, OperationID: safeProtectionHolder(holder.OperationID), AgeSeconds: age}
		if attempt+1 < maxAttempts {
			e.Opts.Sleep(protectionLockPollInterval)
		}
	}
	if conflict == nil {
		conflict = &ProtectionConflictError{Service: service, OperationID: "unknown", AgeSeconds: 0}
	}
	return 0, conflict
}

func (e *Engine) nextProtectionEpoch(ctx context.Context, service string) (int, error) {
	result, err := e.T.Run(ctx, "cat "+q(e.protectionEpochPath(service))+" 2>/dev/null || echo 0")
	if err != nil {
		return 0, err
	}
	previous, _ := strconv.Atoi(strings.TrimSpace(result.Stdout))
	return previous + 1, nil
}

func (e *Engine) writeProtectionFence(ctx context.Context, service, operationID string, epoch int, lockValue string) error {
	fenceValue := operationID + " " + strconv.Itoa(epoch)
	command := `if [ "$(cat ` + q(e.protectionLockPath(service)) + ` 2>/dev/null)" = ` + q(lockValue) + ` ]; then echo ` + strconv.Itoa(epoch) + ` > ` + q(e.protectionEpochPath(service)) + ` && echo ` + q(fenceValue) + ` > ` + q(e.protectionFencePath(service)) + `; else echo ob-protection-lock-lost >&2; exit 96; fi`
	result, err := e.T.Run(ctx, command)
	if err != nil {
		return err
	}
	if result.ExitCode == 96 && strings.Contains(result.Stderr, "ob-protection-lock-lost") {
		return ErrProtectionFenced
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write protection fence: %s", strings.TrimSpace(result.Stderr))
	}
	if e.protectionLockVals == nil {
		e.protectionLockVals = make(map[string]string)
		e.protectionFenceVals = make(map[string]string)
	}
	e.protectionLockVals[service] = lockValue
	e.protectionFenceVals[service] = fenceValue
	return nil
}

func (e *Engine) ReleaseProtectionLock(service string) {
	expected := e.protectionLockVals[service]
	if expected == "" {
		return
	}
	cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := e.T.Run(cleanupContext, `if [ "$(cat `+q(e.protectionLockPath(service))+` 2>/dev/null)" = `+q(expected)+` ]; then rm -f `+q(e.protectionLockPath(service))+`; fi`)
	if err != nil || result.ExitCode != 0 {
		e.warnf("release protection lock failed: %v %s", err, strings.TrimSpace(result.Stderr))
		return
	}
	delete(e.protectionLockVals, service)
	delete(e.protectionFenceVals, service)
}

// StartProtectionHeartbeat keeps a service lock fresh only while both its
// exact lock value and fence still belong to this runner.
func (e *Engine) StartProtectionHeartbeat(ctx context.Context, service string) (func(), error) {
	lockValue := e.protectionLockVals[service]
	fenceValue := e.protectionFenceVals[service]
	if lockValue == "" || fenceValue == "" {
		return nil, errors.New("protection heartbeat requires service lock ownership")
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
				command := `if [ "$(cat ` + q(e.protectionLockPath(service)) + ` 2>/dev/null)" = ` + q(lockValue) + ` ] && [ "$(cat ` + q(e.protectionFencePath(service)) + ` 2>/dev/null)" = ` + q(fenceValue) + ` ]; then touch -c ` + q(e.protectionLockPath(service)) + `; else exit 3; fi`
				if result, err := e.T.Run(heartbeatContext, command); err == nil && result.ExitCode != 0 && result.ExitCode != 3 {
					e.warnf("protection heartbeat for %s failed (exit %d): %s", service, result.ExitCode, strings.TrimSpace(result.Stderr))
				}
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}

// ProtectionMutate nests the exact service lock/fence guard inside the app
// fence guard. A runner that loses either authority cannot mutate service data.
func (e *Engine) ProtectionMutate(ctx context.Context, service, command string) (transport.Result, error) {
	lockValue := e.protectionLockVals[service]
	fenceValue := e.protectionFenceVals[service]
	if e.lockVal == "" || e.fenceVal == "" || lockValue == "" || fenceValue == "" {
		return transport.Result{}, errors.New("protection mutation requires application and service lock ownership")
	}
	guarded := `if [ "$(cat ` + q(e.protectionLockPath(service)) + ` 2>/dev/null)" = ` + q(lockValue) + ` ] && [ "$(cat ` + q(e.protectionFencePath(service)) + ` 2>/dev/null)" = ` + q(fenceValue) + ` ]; then ` + command + `; else echo ob-protection-fenced >&2; exit 98; fi`
	result, err := e.mutate(ctx, guarded)
	if err != nil {
		return result, err
	}
	if result.ExitCode == 98 && strings.Contains(result.Stderr, "ob-protection-fenced") {
		return result, ErrProtectionFenced
	}
	return result, nil
}

func safeProtectionHolder(value string) string {
	if protectionIdentity.MatchString(value) {
		return value
	}
	return "unknown"
}
