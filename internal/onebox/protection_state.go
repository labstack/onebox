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
	"strings"
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

type ProtectionDisableStep struct {
	Phase        ProtectionDisablePhase `json:"phase"`
	Mutation     bool                   `json:"mutation"`
	Rollbackable bool                   `json:"rollbackable"`
}

type ProtectionDisablePlan struct {
	SchemaVersion    string                  `json:"schema_version"`
	OperationID      string                  `json:"operation_id"`
	Application      string                  `json:"application"`
	Environment      string                  `json:"environment"`
	Service          string                  `json:"service"`
	StateDigest      string                  `json:"state_digest"`
	Epoch            int                     `json:"epoch"`
	Approval         ApprovalClass           `json:"approval"`
	Interruption     bool                    `json:"interruption"`
	RemoteDataAction string                  `json:"remote_data_action"`
	Steps            []ProtectionDisableStep `json:"steps"`
	PlanDigest       string                  `json:"plan_digest"`
}

type ProtectionDisableAuthorization struct {
	OperationID string `json:"operation_id"`
	PlanDigest  string `json:"plan_digest"`
	StateDigest string `json:"state_digest"`
	Strong      bool   `json:"strong"`
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
	if current.State != ProtectionNeverEnabled && current.State != ProtectionDisabled {
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

func RequestProtectionDisable(current ProtectionLifecycleState, operationID string, now time.Time, nextEpoch int) (ProtectionLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if current.State == ProtectionDisablePending {
		if current.OperationID == operationID {
			return current, nil
		}
		failure, _ := NewLifecycleFailure("backup_conflict")
		return ProtectionLifecycleState{}, failure
	}
	if current.State != ProtectionEnabled || current.LastEffective == nil {
		return ProtectionLifecycleState{}, fmt.Errorf("cannot request protection disablement from %q", current.State)
	}
	if !safeLifecycleMetadata(operationID) || nextEpoch <= current.Epoch {
		return ProtectionLifecycleState{}, errors.New("protection disablement operation or fencing epoch is invalid")
	}
	now = now.UTC()
	next := current
	next.State, next.Phase, next.Epoch = ProtectionDisablePending, ProtectionPhaseRequested, nextEpoch
	next.OperationID, next.DisablePlanDigest = operationID, ""
	next.RequestedAt = now.Format(time.RFC3339Nano)
	next.ActionDeadline = now.Add(ProtectionDisableActionWindow).Format(time.RFC3339Nano)
	next.Schedules = effectiveProtectionSchedules(*current.LastEffective, false)
	if err := next.Seal(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	return next, nil
}

func NewProtectionDisablePlan(state ProtectionLifecycleState) (ProtectionDisablePlan, error) {
	if err := state.Validate(); err != nil {
		return ProtectionDisablePlan{}, err
	}
	if state.State != ProtectionDisablePending || state.Phase != ProtectionPhaseRequested {
		return ProtectionDisablePlan{}, errors.New("protection disable plan requires newly requested disable-pending state")
	}
	plan := ProtectionDisablePlan{
		SchemaVersion: ProtectionDisablePlanSchemaVersion, OperationID: state.OperationID,
		Application: state.Application, Environment: state.Environment, Service: state.Service,
		StateDigest: state.StateDigest, Epoch: state.Epoch, Approval: ApprovalStrong, Interruption: true,
		RemoteDataAction: "handback-preserve",
		Steps: []ProtectionDisableStep{
			{Phase: ProtectionPhasePrerequisiteReversed, Mutation: true, Rollbackable: true},
			{Phase: ProtectionPhasePrerequisiteAbsent, Rollbackable: true},
			{Phase: ProtectionPhaseRuntimeReverted, Mutation: true},
			{Phase: ProtectionPhaseLocalSupportRemoved, Mutation: true},
			{Phase: ProtectionPhaseComplete, Mutation: true},
		},
	}
	if err := plan.Seal(); err != nil {
		return ProtectionDisablePlan{}, err
	}
	return plan, nil
}

func AdvanceProtectionDisable(current ProtectionLifecycleState, plan ProtectionDisablePlan, authorization ProtectionDisableAuthorization, completed ProtectionDisablePhase, nextEpoch int) (ProtectionLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if err := plan.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if err := validateProtectionDisableAuthority(current, plan, authorization); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if current.State != ProtectionDisablePending {
		return ProtectionLifecycleState{}, fmt.Errorf("cannot advance disablement from %q", current.State)
	}
	// A disconnected caller may retry the phase whose commit it did not see.
	if current.Phase == completed {
		return current, nil
	}
	expected, ok := nextProtectionDisablePhase(current.Phase)
	if !ok || completed != expected {
		return ProtectionLifecycleState{}, fmt.Errorf("disablement phase %q cannot follow %q", completed, current.Phase)
	}
	if nextEpoch <= current.Epoch {
		return ProtectionLifecycleState{}, errors.New("protection disablement update has a stale fencing epoch")
	}
	next := current
	next.Phase, next.Epoch, next.DisablePlanDigest = completed, nextEpoch, plan.PlanDigest
	switch completed {
	case ProtectionPhasePrerequisiteAbsent:
		next.PrerequisiteEffective = false
	case ProtectionPhaseRuntimeReverted:
		if next.PrerequisiteEffective {
			failure, _ := NewLifecycleFailure("protection_image_revert_unsafe")
			return ProtectionLifecycleState{}, failure
		}
	case ProtectionPhaseLocalSupportRemoved:
		if next.PrerequisiteEffective {
			failure, _ := NewLifecycleFailure("protection_image_revert_unsafe")
			return ProtectionLifecycleState{}, failure
		}
		next.LocalSupportInstalled = false
	case ProtectionPhaseComplete:
		if next.PrerequisiteEffective || next.LocalSupportInstalled {
			return ProtectionLifecycleState{}, errors.New("disablement cannot complete before prerequisite and local support removal")
		}
		next.State = ProtectionDisabled
		next.ServiceImage = ""
		next.Schedules = inactiveProtectionSchedules(next.Schedules)
	}
	if err := next.Seal(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	return next, nil
}

func RollbackProtectionDisable(current ProtectionLifecycleState, plan ProtectionDisablePlan, authorization ProtectionDisableAuthorization, nextEpoch int) (ProtectionLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if err := plan.Validate(); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if err := validateProtectionDisableAuthority(current, plan, authorization); err != nil {
		return ProtectionLifecycleState{}, err
	}
	if current.State != ProtectionDisablePending || (current.Phase != ProtectionPhaseRequested && current.Phase != ProtectionPhasePrerequisiteReversed) {
		return ProtectionLifecycleState{}, errors.New("disablement rollback is no longer safe after prerequisite absence was verified")
	}
	if nextEpoch <= current.Epoch {
		return ProtectionLifecycleState{}, errors.New("protection rollback has a stale fencing epoch")
	}
	next := current
	next.State, next.Phase, next.Epoch = ProtectionEnabled, ProtectionPhaseIdle, nextEpoch
	next.OperationID, next.DisablePlanDigest, next.RequestedAt, next.ActionDeadline = "", "", "", ""
	next.PrerequisiteEffective, next.LocalSupportInstalled = true, true
	next.Schedules = effectiveProtectionSchedules(*next.LastEffective, true)
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

func (state ProtectionLifecycleState) RemovalRequest(mode OperationKind) (ProtectionRemovalRequest, error) {
	if err := state.Validate(); err != nil {
		return ProtectionRemovalRequest{}, err
	}
	if state.State != ProtectionDisabled || state.PrerequisiteEffective || state.LocalSupportInstalled {
		failure, _ := NewLifecycleFailure("protection_disable_pending")
		return ProtectionRemovalRequest{}, failure
	}
	return ProtectionRemovalRequest{
		Mode: mode, Application: state.Application, Environment: state.Environment, Service: state.Service,
		ProtectionState: string(state.State), StateDigest: state.StateDigest, PrerequisitesVerifiedAbsent: true,
	}, nil
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
		if state.Phase != ProtectionPhaseRequested && !lifecycleGraphDigest.MatchString(state.DisablePlanDigest) {
			return errors.New("advanced disable-pending state requires a sealed plan digest")
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

func (plan *ProtectionDisablePlan) Seal() error {
	if plan == nil {
		return errors.New("protection disable plan is nil")
	}
	if err := plan.validateContent(); err != nil {
		return err
	}
	digest, err := plan.computeDigest()
	if err != nil {
		return err
	}
	plan.PlanDigest = digest
	return nil
}

func (plan ProtectionDisablePlan) Validate() error {
	if err := plan.validateContent(); err != nil {
		return err
	}
	expected, err := plan.computeDigest()
	if err != nil {
		return err
	}
	if plan.PlanDigest != expected {
		return errors.New("protection disable plan digest mismatch")
	}
	return nil
}

func (plan ProtectionDisablePlan) validateContent() error {
	if plan.SchemaVersion != ProtectionDisablePlanSchemaVersion || plan.Approval != ApprovalStrong || !plan.Interruption || plan.RemoteDataAction != "handback-preserve" {
		return errors.New("protection disable plan safety contract is invalid")
	}
	for _, value := range []string{plan.OperationID, plan.Application, plan.Environment, plan.Service} {
		if !safeLifecycleMetadata(value) {
			return errors.New("protection disable plan metadata is invalid")
		}
	}
	if !lifecycleGraphDigest.MatchString(plan.StateDigest) || plan.Epoch <= 0 {
		return errors.New("protection disable plan state binding is invalid")
	}
	want := []ProtectionDisablePhase{
		ProtectionPhasePrerequisiteReversed, ProtectionPhasePrerequisiteAbsent, ProtectionPhaseRuntimeReverted,
		ProtectionPhaseLocalSupportRemoved, ProtectionPhaseComplete,
	}
	if len(plan.Steps) != len(want) {
		return errors.New("protection disable plan has an incomplete phase graph")
	}
	for index, phase := range want {
		if plan.Steps[index].Phase != phase {
			return errors.New("protection disable plan phase graph is not canonical")
		}
	}
	return nil
}

func (plan ProtectionDisablePlan) computeDigest() (string, error) {
	copy := plan
	copy.PlanDigest = ""
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
	return saveBackupArtifact(path, ".protection-state-*", state)
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

func validateProtectionDisableAuthority(state ProtectionLifecycleState, plan ProtectionDisablePlan, authorization ProtectionDisableAuthorization) error {
	if !authorization.Strong || authorization.OperationID != plan.OperationID || authorization.PlanDigest != plan.PlanDigest || authorization.StateDigest != plan.StateDigest {
		failure, _ := NewLifecycleFailure("protection_disablement_not_authorized")
		return failure
	}
	if state.OperationID != plan.OperationID || state.Application != plan.Application || state.Environment != plan.Environment || state.Service != plan.Service {
		return errors.New("protection disable plan does not match lifecycle state")
	}
	if state.Phase == ProtectionPhaseRequested {
		if state.StateDigest != plan.StateDigest {
			return errors.New("protection disable plan state binding is stale")
		}
	} else if state.DisablePlanDigest != plan.PlanDigest {
		return errors.New("protection disable plan does not own the in-flight state")
	}
	return nil
}

func nextProtectionDisablePhase(current ProtectionDisablePhase) (ProtectionDisablePhase, bool) {
	switch current {
	case ProtectionPhaseRequested:
		return ProtectionPhasePrerequisiteReversed, true
	case ProtectionPhasePrerequisiteReversed:
		return ProtectionPhasePrerequisiteAbsent, true
	case ProtectionPhasePrerequisiteAbsent:
		return ProtectionPhaseRuntimeReverted, true
	case ProtectionPhaseRuntimeReverted:
		return ProtectionPhaseLocalSupportRemoved, true
	case ProtectionPhaseLocalSupportRemoved:
		return ProtectionPhaseComplete, true
	default:
		return "", false
	}
}

func validProtectionStatePhase(state ProtectionState, phase ProtectionDisablePhase) bool {
	switch state {
	case ProtectionNeverEnabled, ProtectionEnabled:
		return phase == ProtectionPhaseIdle
	case ProtectionDisablePending:
		return phase == ProtectionPhaseRequested || phase == ProtectionPhasePrerequisiteReversed ||
			phase == ProtectionPhasePrerequisiteAbsent || phase == ProtectionPhaseRuntimeReverted || phase == ProtectionPhaseLocalSupportRemoved
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

func replayArchiveSchedule(policy app.ProtectionPolicy) app.Schedule {
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

func inactiveProtectionSchedules(schedules []ProtectionScheduleState) []ProtectionScheduleState {
	copy := append([]ProtectionScheduleState(nil), schedules...)
	for index := range copy {
		copy[index].Active = false
	}
	return copy
}

func cloneProtectionProjection(projection *app.ProtectionEffectiveProjection) *app.ProtectionEffectiveProjection {
	if projection == nil {
		return nil
	}
	copy := *projection
	return &copy
}

func (state ProtectionLifecycleState) activeScheduleKinds() []string {
	var kinds []string
	for _, schedule := range state.Schedules {
		if schedule.Active {
			kinds = append(kinds, schedule.Kind)
		}
	}
	sort.Strings(kinds)
	return kinds
}

func protectionStateContainsRemoteDeletion(value any) bool {
	encoded, _ := json.Marshal(value)
	text := strings.ToLower(string(encoded))
	return strings.Contains(text, "delete-remote") || strings.Contains(text, "purge-remote")
}
