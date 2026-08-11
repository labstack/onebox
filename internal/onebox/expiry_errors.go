package onebox

import (
	"fmt"
	"time"
)

// PlanExpiredError and ApprovalExpiredError are the two most common recoverable
// states in the plan/approve/apply loop, and both were previously untyped. An
// untyped failure reaches the caller as the generic operation_failed envelope,
// which cannot distinguish "re-plan and try again" — always safe, always the
// right move — from a failure that changed something on the host. Typing them
// is what lets the envelope carry the command that resolves them.
// PlanKind distinguishes the two sealed plans, because they are re-planned by
// different commands. One shared guidance command sent an operator whose job
// plan expired to `ob plan`, which produces a deployment plan instead.
type PlanKind string

const (
	PlanKindDeployment PlanKind = "deployment plan"
	PlanKindJob        PlanKind = "job plan"
)

type PlanExpiredError struct {
	Kind      PlanKind
	Job       string
	ExpiresAt time.Time
}

func (err *PlanExpiredError) Error() string {
	return fmt.Sprintf("%s expired at %s — re-plan", err.Kind, err.ExpiresAt.UTC().Format(time.RFC3339))
}

func (err *PlanExpiredError) Code() string { return "plan_expired" }

// GuidanceCommand overrides the code's published default with the command that
// re-plans this kind of artifact.
func (err *PlanExpiredError) GuidanceCommand() string {
	if err.Kind != PlanKindJob {
		return "ob plan --output json"
	}
	if err.Job == "" {
		return "ob job plan <job> --output json"
	}
	return "ob job plan " + err.Job + " --output json"
}

type ApprovalExpiredError struct{ ExpiresAt time.Time }

func (err *ApprovalExpiredError) Error() string {
	return fmt.Sprintf("approval expired at %s — approve the current plan again", err.ExpiresAt.UTC().Format(time.RFC3339))
}

func (err *ApprovalExpiredError) Code() string { return "approval_expired" }
