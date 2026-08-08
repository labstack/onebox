package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/labstack/onebox/internal/journal"
)

var (
	ErrScheduledRunnerCrash = errors.New("scheduled runner stopped before terminal journal commit")
	scheduledEvidenceID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

type ScheduledProtectionRequest struct {
	OperationID      string
	OperationKind    string
	Service          string
	RetryIdentity    string
	LockWait         time.Duration
	HelperProvenance *journal.HelperProvenance
}

type ScheduledProtectionAction func(context.Context, *Engine) (string, error)

type scheduledCodedError interface {
	Code() string
}

type scheduledRetryableError interface {
	Retryable() bool
}

// ExecuteScheduledProtection applies the same app lock, service lock, fences,
// heartbeat, deterministic journal identity, retry classification, provenance,
// and redaction boundary used by interactive lifecycle execution.
func (e *Engine) ExecuteScheduledProtection(ctx context.Context, request ScheduledProtectionRequest, action ScheduledProtectionAction) error {
	if action == nil {
		return errors.New("scheduled protection action is nil")
	}
	if request.LockWait < 0 {
		return errors.New("scheduled protection lock wait must not be negative")
	}
	for name, value := range map[string]string{
		"operation_id": request.OperationID, "operation_kind": request.OperationKind,
		"service": request.Service, "retry_identity": request.RetryIdentity,
	} {
		if !scheduledEvidenceID.MatchString(value) {
			return fmt.Errorf("scheduled protection %s is invalid", name)
		}
	}
	stepID, err := journal.ProtectionStepID(request.OperationKind, request.Service, request.RetryIdentity)
	if err != nil {
		return err
	}
	epoch, err := e.AcquireLock(ctx, request.OperationID, false)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, request.OperationID, epoch); err != nil {
		return err
	}
	stopAppHeartbeat := e.StartHeartbeat(ctx)
	defer stopAppHeartbeat()

	writer := &journal.Writer{
		T: e.T, Names: e.names(), DeployID: request.OperationID, Epoch: epoch,
		Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner,
	}
	if terminal, ok, err := journal.LookupProtectionTerminalResult(ctx, e.T, e.names(), request.OperationID); err != nil {
		return err
	} else if ok {
		switch terminal.State {
		case "succeeded":
			return nil
		case "failed", "cancelled":
			return &scheduledTerminalError{state: terminal.State, code: terminal.ErrorCode}
		}
	}
	attempt := 1
	if previous, ok, err := journal.LookupProtectionStep(ctx, e.T, e.names(), request.OperationID, stepID); err != nil {
		return err
	} else if ok && previous.ProtectionAttempt >= attempt {
		attempt = previous.ProtectionAttempt + 1
	}
	baseRecord := journal.Record{
		Phase: "scheduled", SubStep: request.RetryIdentity, Event: "start", Status: "ok",
		OperationKind: request.OperationKind, Service: request.Service,
		ProtectionStepID: stepID, ProtectionAttempt: attempt, HelperProvenance: cloneScheduledHelper(request.HelperProvenance),
	}
	if err := writer.AppendProtection(ctx, baseRecord); err != nil {
		return err
	}
	if _, err := e.AcquireProtectionLock(ctx, request.Service, request.OperationID, request.LockWait); err != nil {
		return appendScheduledProtectionResult(ctx, writer, baseRecord, "", err)
	}
	defer e.ReleaseProtectionLock(request.Service)
	stopProtectionHeartbeat, err := e.StartProtectionHeartbeat(ctx, request.Service)
	if err != nil {
		return appendScheduledProtectionResult(ctx, writer, baseRecord, "", err)
	}
	defer stopProtectionHeartbeat()

	evidenceID, actionErr := action(ctx, e)
	if errors.Is(actionErr, ErrScheduledRunnerCrash) {
		return actionErr
	}
	if actionErr == nil && !scheduledEvidenceID.MatchString(evidenceID) {
		actionErr = errors.New("scheduled execution returned an invalid evidence identity")
	}
	return appendScheduledProtectionResult(ctx, writer, baseRecord, evidenceID, actionErr)
}

type scheduledTerminalError struct {
	state string
	code  string
}

func (err *scheduledTerminalError) Error() string {
	return fmt.Sprintf("scheduled operation already %s with code %s", err.state, err.code)
}

func (err *scheduledTerminalError) Code() string { return err.code }

func appendScheduledProtectionResult(
	ctx context.Context,
	writer *journal.Writer,
	baseRecord journal.Record,
	evidenceID string,
	actionErr error,
) error {
	resultRecord := baseRecord
	resultRecord.Event = "result"
	resultRecord.TerminalResult = &journal.ProtectionTerminalResult{State: "succeeded", EvidenceID: evidenceID}
	if actionErr != nil {
		code, retryable := classifyScheduledProtectionError(actionErr)
		state, retryClass := "failed", "terminal"
		if errors.Is(actionErr, context.Canceled) || errors.Is(actionErr, context.DeadlineExceeded) {
			state, code = "cancelled", "operation_cancelled"
		} else if retryable {
			state, retryClass = "incomplete", "retryable"
		}
		resultRecord.Status, resultRecord.ErrorCode = "fail", code
		resultRecord.Retry = &journal.RetryClassification{Class: retryClass, ReasonCode: code}
		resultRecord.TerminalResult = &journal.ProtectionTerminalResult{State: state, ErrorCode: code}
	}
	journalContext := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil || errors.Is(actionErr, context.Canceled) || errors.Is(actionErr, context.DeadlineExceeded) {
		journalContext, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err := writer.AppendProtection(journalContext, resultRecord); err != nil {
		return errors.Join(actionErr, err)
	}
	return actionErr
}

func classifyScheduledProtectionError(err error) (string, bool) {
	code := "scheduled_execution_failed"
	var coded scheduledCodedError
	if errors.As(err, &coded) && scheduledEvidenceID.MatchString(coded.Code()) {
		code = coded.Code()
	}
	var retryable scheduledRetryableError
	return code, errors.As(err, &retryable) && retryable.Retryable()
}

func cloneScheduledHelper(helper *journal.HelperProvenance) *journal.HelperProvenance {
	if helper == nil {
		return nil
	}
	copy := *helper
	return &copy
}
