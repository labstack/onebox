package onebox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"

	"github.com/labstack/onebox/internal/app"
)

const (
	ProtectionStateSchemaVersion       = "onebox.run/protection-state/v1alpha1"
	ProtectionDisablePlanSchemaVersion = "onebox.run/protection-disable-plan/v1alpha1"
	ProtectionDisableActionWindow      = 24 * time.Hour
)

type ProtectionState string

const (
	ProtectionNeverEnabled   ProtectionState = "never-enabled"
	ProtectionEnabled        ProtectionState = "enabled"
	ProtectionDisablePending ProtectionState = "disable-pending"
	ProtectionDisabled       ProtectionState = "disabled"
)

type ProtectionDisablePhase string

const (
	ProtectionPhaseIdle                 ProtectionDisablePhase = "idle"
	ProtectionPhaseRequested            ProtectionDisablePhase = "requested"
	ProtectionPhasePrerequisiteReversed ProtectionDisablePhase = "prerequisite-reversed"
	ProtectionPhasePrerequisiteAbsent   ProtectionDisablePhase = "prerequisite-absent"
	ProtectionPhaseRuntimeReverted      ProtectionDisablePhase = "runtime-reverted"
	ProtectionPhaseLocalSupportRemoved  ProtectionDisablePhase = "local-support-removed"
	ProtectionPhaseComplete             ProtectionDisablePhase = "complete"
)

var protectedRuntimeImage = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

type ProtectionScheduleState struct {
	Kind     string       `json:"kind"`
	Schedule app.Schedule `json:"schedule"`
	Active   bool         `json:"active"`
}

type ProtectionLifecycleState struct {
	SchemaVersion                   string                             `json:"schema_version"`
	Application                     string                             `json:"application"`
	Environment                     string                             `json:"environment"`
	Service                         string                             `json:"service"`
	State                           ProtectionState                    `json:"state"`
	Phase                           ProtectionDisablePhase             `json:"phase"`
	Epoch                           int                                `json:"epoch"`
	OperationID                     string                             `json:"operation_id,omitempty"`
	DisablePlanDigest               string                             `json:"disable_plan_digest,omitempty"`
	RequestedAt                     string                             `json:"requested_at,omitempty"`
	ActionDeadline                  string                             `json:"action_deadline,omitempty"`
	ServiceImage                    string                             `json:"service_image,omitempty"`
	ServiceImagePublicationVerified bool                               `json:"service_image_publication_verified,omitempty"`
	PrerequisiteEffective           bool                               `json:"prerequisite_effective"`
	LocalSupportInstalled           bool                               `json:"local_support_installed"`
	LastEffective                   *app.ProtectionEffectiveProjection `json:"last_effective,omitempty"`
	Schedules                       []ProtectionScheduleState          `json:"schedules,omitempty"`
	StateDigest                     string                             `json:"state_digest"`
}

type ProtectionLifecycleStatus struct {
	State            ProtectionState           `json:"state"`
	Phase            ProtectionDisablePhase    `json:"phase"`
	RequestedAt      string                    `json:"requested_at,omitempty"`
	ActionDeadline   string                    `json:"action_deadline,omitempty"`
	Elapsed          string                    `json:"elapsed,omitempty"`
	Schedules        []ProtectionScheduleState `json:"schedules,omitempty"`
	StorageContinues bool                      `json:"storage_continues"`
	ResolvingCommand string                    `json:"resolving_command,omitempty"`
	Failure          *LifecycleFailure         `json:"failure,omitempty"`
}

func NewProtectionLifecycleState(application, environment, service string, epoch int) (ProtectionLifecycleState, error) {
	state := ProtectionLifecycleState{
		SchemaVersion: ProtectionStateSchemaVersion, Application: application, Environment: environment,
		Service: service, State: ProtectionNeverEnabled, Phase: ProtectionPhaseIdle, Epoch: epoch,
	}
	if err := state.Seal(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	return state, nil
}

func EnableProtection(current ProtectionLifecycleState, projection app.ProtectionEffectiveProjection, serviceImage, operationID string, publicationVerified bool, nextEpoch int) (ProtectionLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	// disable-pending is enablable: it means a disablement was requested and did
	// not finish, so the service is very likely still archiving. Re-enabling is
	// how an operator changes their mind, and refusing would leave the only way
	// out as completing a disable they no longer want.
	if current.State != ProtectionNeverEnabled && current.State != ProtectionDisabled &&
		current.State != ProtectionDisablePending {
		return ProtectionLifecycleState{}, fmt.Errorf("cannot enable protection from %q", current.State)
	}
	if !safeLifecycleMetadata(operationID) || !protectedRuntimeImage.MatchString(serviceImage) || !publicationVerified || nextEpoch <= current.Epoch {
		return ProtectionLifecycleState{}, errors.New("protection enablement operation, image, or fencing epoch is invalid")
	}
	next := current
	next.State, next.Phase, next.Epoch = ProtectionEnabled, ProtectionPhaseIdle, nextEpoch
	next.OperationID, next.DisablePlanDigest, next.RequestedAt, next.ActionDeadline = "", "", "", ""
	next.ServiceImage, next.PrerequisiteEffective, next.LocalSupportInstalled = serviceImage, true, true
	next.ServiceImagePublicationVerified = publicationVerified
	next.LastEffective = cloneProtectionProjection(&projection)
	next.Schedules = effectiveProtectionSchedules(projection, true)
	if err := next.Seal(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	return next, nil
}

func (state ProtectionLifecycleState) AllowOperation(kind OperationKind, touchesProtectedService bool) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if state.State != ProtectionDisablePending {
		return nil
	}
	if kind == KindServiceImagePatch {
		failure, _ := NewLifecycleFailure("service_image_patch_disable_pending")
		return failure
	}
	if kind == KindRestoreTest || ((kind == KindDeploy || kind == KindServiceApply) && touchesProtectedService) {
		failure, _ := NewLifecycleFailure("protection_disable_pending")
		return failure
	}
	return nil
}

func (state ProtectionLifecycleState) ValidateRuntimeImage(candidate string) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if state.State == ProtectionDisablePending && candidate != state.ServiceImage && state.Phase != ProtectionPhaseRuntimeReverted && state.Phase != ProtectionPhaseLocalSupportRemoved {
		failure, _ := NewLifecycleFailure("protection_image_revert_unsafe")
		return failure
	}
	return nil
}

func (state ProtectionLifecycleState) RuntimeState() app.ServiceRuntimeState {
	return app.ServiceRuntimeState{
		ProtectionState: string(state.State), ServiceImage: state.ServiceImage,
		PublicationVerified: state.ServiceImagePublicationVerified,
		LastEffective:       cloneProtectionProjection(state.LastEffective),
	}
}

func (state ProtectionLifecycleState) Status(now time.Time) (ProtectionLifecycleStatus, error) {
	if err := state.Validate(); err != nil {
		return ProtectionLifecycleStatus{}, err
	}
	status := ProtectionLifecycleStatus{State: state.State, Phase: state.Phase, Schedules: append([]ProtectionScheduleState(nil), state.Schedules...)}
	for _, schedule := range state.Schedules {
		if schedule.Active && schedule.Kind != "restore-drill" {
			status.StorageContinues = true
		}
	}
	if state.State != ProtectionDisablePending {
		return status, nil
	}
	requested, _ := time.Parse(time.RFC3339Nano, state.RequestedAt)
	deadline, _ := time.Parse(time.RFC3339Nano, state.ActionDeadline)
	now = now.UTC()
	elapsed := now.Sub(requested)
	if elapsed < 0 {
		elapsed = 0
	}
	status.RequestedAt, status.ActionDeadline = state.RequestedAt, state.ActionDeadline
	status.Elapsed = elapsed.Round(time.Second).String()
	status.ResolvingCommand = "ob protection disable --output ndjson"
	if !now.Before(deadline) {
		failure, _ := NewLifecycleFailure("protection_disablement_overdue")
		status.Failure = &failure
	}
	return status, nil
}

func (state *ProtectionLifecycleState) Seal() error {
	if state == nil {
		return errors.New("protection lifecycle state is nil")
	}
	if err := state.validateContent(); err != nil {
		return err
	}
	digest, err := state.computeDigest()
	if err != nil {
		return err
	}
	state.StateDigest = digest
	return nil
}

func (state ProtectionLifecycleState) Validate() error {
	if err := state.validateContent(); err != nil {
		return err
	}
	if !lifecycleGraphDigest.MatchString(state.StateDigest) {
		return errors.New("protection lifecycle state digest is missing or invalid")
	}
	expected, err := state.computeDigest()
	if err != nil {
		return err
	}
	if state.StateDigest != expected {
		return errors.New("protection lifecycle state digest mismatch")
	}
	return nil
}

func (state ProtectionLifecycleState) validateContent() error {
	if state.SchemaVersion != ProtectionStateSchemaVersion {
		return fmt.Errorf("unsupported protection state schema %q", state.SchemaVersion)
	}
	for _, value := range []string{state.Application, state.Environment, state.Service} {
		if !safeLifecycleMetadata(value) {
			return errors.New("protection state ownership metadata is invalid")
		}
	}
	if state.Epoch <= 0 {
		return errors.New("protection lifecycle epoch must be positive")
	}
	if !validProtectionStatePhase(state.State, state.Phase) {
		return fmt.Errorf("invalid protection state/phase %q/%q", state.State, state.Phase)
	}
	if state.ServiceImage != "" && !protectedRuntimeImage.MatchString(state.ServiceImage) {
		return errors.New("protected service image must be digest-pinned")
	}
	if state.State == ProtectionEnabled || state.State == ProtectionDisablePending {
		if state.LastEffective == nil || state.ServiceImage == "" || !state.ServiceImagePublicationVerified {
			return errors.New("active protection state requires last-effective intent and a provenance-verified service image")
		}
	}
	if state.State == ProtectionDisablePending {
		if !safeLifecycleMetadata(state.OperationID) {
			return errors.New("disable-pending state requires an operation identity")
		}
		requested, err := time.Parse(time.RFC3339Nano, state.RequestedAt)
		if err != nil {
			return errors.New("disable-pending requested_at is invalid")
		}
		deadline, err := time.Parse(time.RFC3339Nano, state.ActionDeadline)
		if err != nil || deadline.Sub(requested) != ProtectionDisableActionWindow {
			return errors.New("disable-pending action deadline must be exactly 24 hours")
		}
	}
	previous := ""
	for _, schedule := range state.Schedules {
		if !safeLifecycleMetadata(schedule.Kind) || (previous != "" && schedule.Kind <= previous) {
			return errors.New("protection schedules must have unique sorted safe kinds")
		}
		if schedule.Kind == "restore-drill" && state.State == ProtectionDisablePending && schedule.Active {
			return errors.New("restore drills must stop during disable-pending")
		}
		previous = schedule.Kind
	}
	return nil
}

func (state ProtectionLifecycleState) computeDigest() (string, error) {
	copy := state
	copy.StateDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func SaveProtectionLifecycleState(path string, state ProtectionLifecycleState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	return saveBackupArtifact(path, ".backup-state-*", state)
}

func LoadProtectionLifecycleState(path string) (ProtectionLifecycleState, error) {
	var state ProtectionLifecycleState
	if err := loadBackupArtifact(path, &state); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if err := state.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	return state, nil
}

// DecodeProtectionLifecycleState validates target-observed state without
// accepting unknown fields or trailing JSON that were not covered by its seal.
func DecodeProtectionLifecycleState(encoded []byte) (ProtectionLifecycleState, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state ProtectionLifecycleState
	if err := decoder.Decode(&state); err != nil {
		return ProtectionLifecycleState{}, fmt.Errorf("decode protection lifecycle state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ProtectionLifecycleState{}, errors.New("decode protection lifecycle state: multiple JSON values")
		}
		return ProtectionLifecycleState{}, fmt.Errorf("decode protection lifecycle state: %w", err)
	}
	if err := state.Validate(); err != nil {
		return ProtectionLifecycleState{}, fmt.Errorf("validate protection lifecycle state: %w", err)
	}
	return state, nil
}

func validProtectionStatePhase(state ProtectionState, phase ProtectionDisablePhase) bool {
	switch state {
	case ProtectionNeverEnabled, ProtectionEnabled:
		return phase == ProtectionPhaseIdle
	case ProtectionDisablePending:
		// One phase, because disablement is one step now. The intermediate
		// phases belonged to the multi-phase apparatus this replaced.
		return phase == ProtectionPhaseRequested
	case ProtectionDisabled:
		return phase == ProtectionPhaseComplete || phase == ProtectionPhaseIdle
	default:
		return false
	}
}

func effectiveProtectionSchedules(projection app.ProtectionEffectiveProjection, drillsActive bool) []ProtectionScheduleState {
	schedules := []ProtectionScheduleState{
		{Kind: "backup-create", Schedule: projection.Policy.Schedule, Active: true},
		{Kind: "backup-prune", Schedule: projection.Policy.Schedule, Active: true},
		{Kind: "restore-drill", Schedule: projection.Policy.RestoreDrill.Schedule, Active: drillsActive},
	}
	if projection.Policy.RecoveryKind == "pitr" {
		schedules = append(schedules, ProtectionScheduleState{Kind: "replay-archive", Schedule: replayArchiveSchedule(projection.Policy), Active: true})
	}
	sort.Slice(schedules, func(i, j int) bool { return schedules[i].Kind < schedules[j].Kind })
	return schedules
}

func replayArchiveSchedule(policy app.BackupPolicy) app.Schedule {
	duration, ok := app.ParseDuration(policy.MaximumDataLoss)
	if !ok || duration <= 0 {
		return policy.Schedule
	}
	minutes := int(duration / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	var cron string
	switch {
	case minutes < 60:
		cron = fmt.Sprintf("*/%d * * * *", minutes)
	case minutes < 24*60:
		hours := minutes / 60
		cron = fmt.Sprintf("0 */%d * * *", hours)
	default:
		cron = "0 0 * * *"
	}
	return app.Schedule{Cron: cron, Timezone: policy.Schedule.Timezone}
}

func cloneProtectionProjection(projection *app.ProtectionEffectiveProjection) *app.ProtectionEffectiveProjection {
	if projection == nil {
		return nil
	}
	copy := *projection
	return &copy
}

// BeginProtectionDisable records the intent before any of the work happens.
//
// Disablement stops archiving, restarts the service unprotected and removes the
// destination credentials, and those steps take time and can fail. Writing
// "disabled" first would claim the work was done before it was: a failure
// halfway leaves a record saying the service is not archiving while it still is.
// disable-pending is the state that says the decision is made and the work is
// not finished, which is what a resumed or retried run needs to read.
func BeginProtectionDisable(current ProtectionLifecycleState, operationID string, now time.Time, nextEpoch int) (ProtectionLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if current.State == ProtectionDisablePending {
		return current, nil
	}
	if current.State != ProtectionEnabled {
		return ProtectionLifecycleState{}, fmt.Errorf("cannot begin disablement from %q", current.State)
	}
	if !safeLifecycleMetadata(operationID) || nextEpoch <= current.Epoch {
		return ProtectionLifecycleState{}, errors.New("protection disablement operation or fencing epoch is invalid")
	}
	next := current
	next.State, next.Phase, next.Epoch = ProtectionDisablePending, ProtectionPhaseRequested, nextEpoch
	next.OperationID = operationID
	// The deadline is what makes a stalled disablement visible: `ob backup
	// status` reports a pending state past it as overdue rather than as a
	// service quietly still archiving after somebody asked it to stop.
	next.RequestedAt = now.UTC().Format(time.RFC3339Nano)
	next.ActionDeadline = now.UTC().Add(ProtectionDisableActionWindow).Format(time.RFC3339Nano)
	// Drills stop the moment disablement is requested. A drill materialises a
	// whole recovered cluster; running one for a service somebody has just asked
	// to stop protecting is work nobody wants and capacity nobody budgeted.
	// Backups keep running until the work completes, so the recovery window has
	// no hole in it while the disablement is in flight.
	if current.LastEffective != nil {
		next.Schedules = effectiveProtectionSchedules(*current.LastEffective, false)
	}
	// Still archiving, still holding its runtime — that is the point of the
	// pending state, and rendering keeps producing the protected server until
	// the work actually completes.
	if err := next.Seal(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	return next, nil
}

// DisableProtection records that the work is done.
//
// The multi-phase apparatus this replaced — request, plan, authorize, advance,
// roll back — was never reachable and is gone. Stopping a backup is not a data
// migration, and every phase of it was another place to leave a service half
// disabled. Two states carry what is actually needed: pending while the work
// runs, disabled once it has.
//
// It says nothing about the repository, and deliberately: disabling protection
// must never delete backups. The history that already exists is the reason
// anyone took it, and someone turning archiving off today may still need to
// recover from last week.
func DisableProtection(current ProtectionLifecycleState, operationID string, nextEpoch int) (ProtectionLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if current.State == ProtectionDisabled || current.State == ProtectionNeverEnabled {
		return current, nil
	}
	if !safeLifecycleMetadata(operationID) || nextEpoch <= current.Epoch {
		return ProtectionLifecycleState{}, errors.New("protection disablement operation or fencing epoch is invalid")
	}
	next := current
	next.State, next.Phase, next.Epoch = ProtectionDisabled, ProtectionPhaseIdle, nextEpoch
	next.OperationID, next.DisablePlanDigest, next.RequestedAt, next.ActionDeadline = "", "", "", ""
	next.PrerequisiteEffective, next.LocalSupportInstalled = false, false
	// Every schedule stops. A timer left active would keep pushing to a
	// repository the project no longer describes.
	next.Schedules = nil
	if err := next.Seal(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	return next, nil
}
