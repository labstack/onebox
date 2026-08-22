// Package onebox exposes the typed product service shared by agent-facing
// adapters. It deliberately contains no protocol or presentation code.
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
	BackupStateSchemaVersion       = "onebox.run/backup-state/v1alpha1"
	BackupDisablePlanSchemaVersion = "onebox.run/backup-disable-plan/v1alpha1"
	BackupDisableActionWindow      = 24 * time.Hour
)

type BackupState string

const (
	BackupNeverEnabled   BackupState = "never-enabled"
	BackupEnabled        BackupState = "enabled"
	BackupDisablePending BackupState = "disable-pending"
	BackupDisabled       BackupState = "disabled"
)

type BackupDisablePhase string

const (
	BackupPhaseIdle                 BackupDisablePhase = "idle"
	BackupPhaseRequested            BackupDisablePhase = "requested"
	BackupPhasePrerequisiteReversed BackupDisablePhase = "prerequisite-reversed"
	BackupPhasePrerequisiteAbsent   BackupDisablePhase = "prerequisite-absent"
	BackupPhaseRuntimeReverted      BackupDisablePhase = "runtime-reverted"
	BackupPhaseLocalSupportRemoved  BackupDisablePhase = "local-support-removed"
	BackupPhaseComplete             BackupDisablePhase = "complete"
)

var backedUpRuntimeImage = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

type BackupScheduleState struct {
	Kind     string       `json:"kind"`
	Schedule app.Schedule `json:"schedule"`
	Active   bool         `json:"active"`
}

type BackupLifecycleState struct {
	SchemaVersion                   string                         `json:"schema_version"`
	Application                     string                         `json:"application"`
	Environment                     string                         `json:"environment"`
	Service                         string                         `json:"service"`
	State                           BackupState                    `json:"state"`
	Phase                           BackupDisablePhase             `json:"phase"`
	Epoch                           int                            `json:"epoch"`
	OperationID                     string                         `json:"operation_id,omitempty"`
	DisablePlanDigest               string                         `json:"disable_plan_digest,omitempty"`
	RequestedAt                     string                         `json:"requested_at,omitempty"`
	ActionDeadline                  string                         `json:"action_deadline,omitempty"`
	ServiceImage                    string                         `json:"service_image,omitempty"`
	ServiceImageReference           string                         `json:"service_image_reference,omitempty"`
	ServiceImagePublicationVerified bool                           `json:"service_image_publication_verified,omitempty"`
	PrerequisiteEffective           bool                           `json:"prerequisite_effective"`
	LocalSupportInstalled           bool                           `json:"local_support_installed"`
	LastEffective                   *app.BackupEffectiveProjection `json:"last_effective,omitempty"`
	DatabaseSystemIdentifier        string                         `json:"database_system_identifier,omitempty"`
	BackupRepositoryGeneration      string                         `json:"backup_repository_generation,omitempty"`
	Schedules                       []BackupScheduleState          `json:"schedules,omitempty"`
	StateDigest                     string                         `json:"state_digest"`
}

type BackupLifecycleStatus struct {
	State                    BackupState           `json:"state"`
	Phase                    BackupDisablePhase    `json:"phase"`
	RequestedAt              string                `json:"requested_at,omitempty"`
	ActionDeadline           string                `json:"action_deadline,omitempty"`
	Elapsed                  string                `json:"elapsed,omitempty"`
	Schedules                []BackupScheduleState `json:"schedules,omitempty"`
	StorageContinues         bool                  `json:"storage_continues"`
	ResolvingCommand         string                `json:"resolving_command,omitempty"`
	Repository               string                `json:"repository,omitempty"`
	DatabaseSystemIdentifier string                `json:"database_system_identifier,omitempty"`
	Failure                  *LifecycleFailure     `json:"failure,omitempty"`
}

func NewBackupLifecycleState(application, environment, service string, epoch int) (BackupLifecycleState, error) {
	state := BackupLifecycleState{
		SchemaVersion: BackupStateSchemaVersion, Application: application, Environment: environment,
		Service: service, State: BackupNeverEnabled, Phase: BackupPhaseIdle, Epoch: epoch,
	}
	if err := state.Seal(); err != nil {
		return BackupLifecycleState{}, err
	}
	return state, nil
}

func EnableBackup(current BackupLifecycleState, projection app.BackupEffectiveProjection, serviceImage, serviceImageReference, operationID string, publicationVerified bool, nextEpoch int) (BackupLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return BackupLifecycleState{}, err
	}
	// disable-pending is enablable: it means a disablement was requested and did
	// not finish, so the service is very likely still archiving. Re-enabling is
	// how an operator changes their mind, and refusing would leave the only way
	// out as completing a disable they no longer want.
	if current.State != BackupNeverEnabled && current.State != BackupDisabled &&
		current.State != BackupDisablePending {
		return BackupLifecycleState{}, fmt.Errorf("cannot enable backup from %q", current.State)
	}
	if !safeLifecycleMetadata(operationID) || !backedUpRuntimeImage.MatchString(serviceImage) || !publicationVerified || nextEpoch <= current.Epoch {
		return BackupLifecycleState{}, errors.New("backup enablement operation, image, or fencing epoch is invalid")
	}
	next := current
	next.State, next.Phase, next.Epoch = BackupEnabled, BackupPhaseIdle, nextEpoch
	next.OperationID, next.DisablePlanDigest, next.RequestedAt, next.ActionDeadline = "", "", "", ""
	next.ServiceImage, next.PrerequisiteEffective, next.LocalSupportInstalled = serviceImage, true, true
	// The reference that produced the pin, so a later enable can tell "the same
	// image, already held" from "the project now declares a different one".
	next.ServiceImageReference = serviceImageReference
	next.ServiceImagePublicationVerified = publicationVerified
	next.LastEffective = cloneBackupProjection(&projection)
	next.Schedules = effectiveBackupSchedules(projection)
	if err := next.Seal(); err != nil {
		return BackupLifecycleState{}, err
	}
	return next, nil
}

func (state BackupLifecycleState) RuntimeState() app.ServiceRuntimeState {
	return app.ServiceRuntimeState{
		BackupState: string(state.State), ServiceImage: state.ServiceImage,
		PublicationVerified:        state.ServiceImagePublicationVerified,
		LastEffective:              cloneBackupProjection(state.LastEffective),
		DatabaseSystemIdentifier:   state.DatabaseSystemIdentifier,
		BackupRepositoryGeneration: state.BackupRepositoryGeneration,
	}
}

func (state BackupLifecycleState) Status(now time.Time) (BackupLifecycleStatus, error) {
	if err := state.Validate(); err != nil {
		return BackupLifecycleStatus{}, err
	}
	status := BackupLifecycleStatus{
		State: state.State, Phase: state.Phase, Schedules: append([]BackupScheduleState(nil), state.Schedules...),
		DatabaseSystemIdentifier: state.DatabaseSystemIdentifier,
	}
	if state.LastEffective != nil {
		status.Repository = app.WalgPrefix(state.LastEffective.Target, state.Application, state.Service, state.BackupRepositoryGeneration)
	}
	for _, schedule := range state.Schedules {
		if schedule.Active {
			status.StorageContinues = true
		}
	}
	if state.State != BackupDisablePending {
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
	status.ResolvingCommand = "ob backup disable --output ndjson"
	if !now.Before(deadline) {
		failure, _ := NewLifecycleFailure("backup_disablement_overdue")
		status.Failure = &failure
	}
	return status, nil
}

func (state *BackupLifecycleState) Seal() error {
	if state == nil {
		return errors.New("backup lifecycle state is nil")
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

func (state BackupLifecycleState) Validate() error {
	if err := state.validateContent(); err != nil {
		return err
	}
	if !lifecycleGraphDigest.MatchString(state.StateDigest) {
		return errors.New("backup lifecycle state digest is missing or invalid")
	}
	expected, err := state.computeDigest()
	if err != nil {
		return err
	}
	if state.StateDigest != expected {
		return errors.New("backup lifecycle state digest mismatch")
	}
	return nil
}

func (state BackupLifecycleState) validateContent() error {
	if state.SchemaVersion != BackupStateSchemaVersion {
		return fmt.Errorf("unsupported backup state schema %q", state.SchemaVersion)
	}
	for _, value := range []string{state.Application, state.Environment, state.Service} {
		if !safeLifecycleMetadata(value) {
			return errors.New("backup state ownership metadata is invalid")
		}
	}
	if state.Epoch <= 0 {
		return errors.New("backup lifecycle epoch must be positive")
	}
	if !validBackupStatePhase(state.State, state.Phase) {
		return fmt.Errorf("invalid backup state/phase %q/%q", state.State, state.Phase)
	}
	if state.ServiceImage != "" && !backedUpRuntimeImage.MatchString(state.ServiceImage) {
		return errors.New("protected service image must be digest-pinned")
	}
	if state.DatabaseSystemIdentifier != "" && !databaseSystemIdentifier.MatchString(state.DatabaseSystemIdentifier) {
		return errors.New("database system identifier is invalid")
	}
	if state.BackupRepositoryGeneration != "" && state.BackupRepositoryGeneration != state.DatabaseSystemIdentifier {
		return errors.New("backup repository generation must match the database system identifier")
	}
	if state.State == BackupEnabled || state.State == BackupDisablePending {
		if state.LastEffective == nil || state.ServiceImage == "" || !state.ServiceImagePublicationVerified {
			return errors.New("active backup state requires last-effective intent and a provenance-verified service image")
		}
	}
	if state.State == BackupDisablePending {
		if !safeLifecycleMetadata(state.OperationID) {
			return errors.New("disable-pending state requires an operation identity")
		}
		requested, err := time.Parse(time.RFC3339Nano, state.RequestedAt)
		if err != nil {
			return errors.New("disable-pending requested_at is invalid")
		}
		deadline, err := time.Parse(time.RFC3339Nano, state.ActionDeadline)
		if err != nil || deadline.Sub(requested) != BackupDisableActionWindow {
			return errors.New("disable-pending action deadline must be exactly 24 hours")
		}
	}
	previous := ""
	for _, schedule := range state.Schedules {
		if !safeLifecycleMetadata(schedule.Kind) || (previous != "" && schedule.Kind <= previous) {
			return errors.New("backup schedules must have unique sorted safe kinds")
		}
		previous = schedule.Kind
	}
	return nil
}

var databaseSystemIdentifier = regexp.MustCompile(`^[0-9]{1,20}$`)

func (state BackupLifecycleState) computeDigest() (string, error) {
	copy := state
	copy.StateDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DecodeBackupLifecycleState validates target-observed state without
// accepting unknown fields or trailing JSON that were not covered by its seal.
func DecodeBackupLifecycleState(encoded []byte) (BackupLifecycleState, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state BackupLifecycleState
	if err := decoder.Decode(&state); err != nil {
		return BackupLifecycleState{}, fmt.Errorf("decode backup lifecycle state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return BackupLifecycleState{}, errors.New("decode backup lifecycle state: multiple JSON values")
		}
		return BackupLifecycleState{}, fmt.Errorf("decode backup lifecycle state: %w", err)
	}
	if err := state.Validate(); err != nil {
		return BackupLifecycleState{}, fmt.Errorf("validate backup lifecycle state: %w", err)
	}
	return state, nil
}

func validBackupStatePhase(state BackupState, phase BackupDisablePhase) bool {
	switch state {
	case BackupNeverEnabled, BackupEnabled:
		return phase == BackupPhaseIdle
	case BackupDisablePending:
		// One phase, because disablement is one step now. The intermediate
		// phases belonged to the multi-phase apparatus this replaced.
		return phase == BackupPhaseRequested
	case BackupDisabled:
		return phase == BackupPhaseComplete || phase == BackupPhaseIdle
	default:
		return false
	}
}

// effectiveBackupSchedules lists what the host itself runs, which is what an
// operator reading `ob backup status` is entitled to assume it means.
//
// It used to also list a "restore-drill" as active on the drill cadence. No such
// timer is ever installed: onebox is agentless, a real drill is orchestrated by
// `ob`, and the unattended half the host can honestly run is the archive
// verification (see the note in engine/backup_schedule.go). The entry was a
// claim of protection that nothing performed — the one thing this product says
// it does not do. `ob backup drill` remains the whole proof, run on the declared
// cadence from CI or a workstation.
func effectiveBackupSchedules(projection app.BackupEffectiveProjection) []BackupScheduleState {
	schedules := []BackupScheduleState{
		{Kind: "backup-create", Schedule: projection.Policy.Schedule, Active: true},
		{Kind: "backup-prune", Schedule: projection.Policy.Schedule, Active: true},
		{Kind: "backup-verify", Schedule: projection.Policy.Drill.Schedule, Active: true},
	}
	if projection.Policy.RecoveryKind == "pitr" {
		schedules = append(schedules, BackupScheduleState{Kind: "replay-archive", Schedule: replayArchiveSchedule(projection.Policy), Active: true})
	}
	sort.Slice(schedules, func(i, j int) bool { return schedules[i].Kind < schedules[j].Kind })
	return schedules
}

func replayArchiveSchedule(policy app.BackupPolicy) app.Schedule {
	duration, ok := app.ParseDuration(policy.MaxDataLoss)
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

func cloneBackupProjection(projection *app.BackupEffectiveProjection) *app.BackupEffectiveProjection {
	if projection == nil {
		return nil
	}
	copy := *projection
	return &copy
}

// BeginBackupDisable records the intent before any of the work happens.
//
// Disablement stops archiving, restarts the service unprotected and removes the
// destination credentials, and those steps take time and can fail. Writing
// "disabled" first would claim the work was done before it was: a failure
// halfway leaves a record saying the service is not archiving while it still is.
// disable-pending is the state that says the decision is made and the work is
// not finished, which is what a resumed or retried run needs to read.
func BeginBackupDisable(current BackupLifecycleState, operationID string, now time.Time, nextEpoch int) (BackupLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return BackupLifecycleState{}, err
	}
	if current.State == BackupDisablePending {
		return current, nil
	}
	if current.State != BackupEnabled {
		return BackupLifecycleState{}, fmt.Errorf("cannot begin disablement from %q", current.State)
	}
	if !safeLifecycleMetadata(operationID) || nextEpoch <= current.Epoch {
		return BackupLifecycleState{}, errors.New("backup disablement operation or fencing epoch is invalid")
	}
	next := current
	next.State, next.Phase, next.Epoch = BackupDisablePending, BackupPhaseRequested, nextEpoch
	next.OperationID = operationID
	// The deadline is what makes a stalled disablement visible: `ob backup
	// status` reports a pending state past it as overdue rather than as a
	// service quietly still archiving after somebody asked it to stop.
	next.RequestedAt = now.UTC().Format(time.RFC3339Nano)
	next.ActionDeadline = now.UTC().Add(BackupDisableActionWindow).Format(time.RFC3339Nano)
	// Drills stop the moment disablement is requested. A drill materialises a
	// whole recovered cluster; running one for a service somebody has just asked
	// to stop protecting is work nobody wants and capacity nobody budgeted.
	// Backups keep running until the work completes, so the recovery window has
	// no hole in it while the disablement is in flight.
	if current.LastEffective != nil {
		next.Schedules = effectiveBackupSchedules(*current.LastEffective)
	}
	// Still archiving, still holding its runtime — that is the point of the
	// pending state, and rendering keeps producing the protected server until
	// the work actually completes.
	if err := next.Seal(); err != nil {
		return BackupLifecycleState{}, err
	}
	return next, nil
}

// DisableBackup records that the work is done.
//
// The multi-phase apparatus this replaced — request, plan, authorize, advance,
// roll back — was never reachable and is gone. Stopping a backup is not a data
// migration, and every phase of it was another place to leave a service half
// disabled. Two states carry what is actually needed: pending while the work
// runs, disabled once it has.
//
// It says nothing about the repository, and deliberately: disabling backup
// must never delete backups. The history that already exists is the reason
// anyone took it, and someone turning archiving off today may still need to
// recover from last week.
func DisableBackup(current BackupLifecycleState, operationID string, nextEpoch int) (BackupLifecycleState, error) {
	if err := current.Validate(); err != nil {
		return BackupLifecycleState{}, err
	}
	if current.State == BackupDisabled || current.State == BackupNeverEnabled {
		return current, nil
	}
	if !safeLifecycleMetadata(operationID) || nextEpoch <= current.Epoch {
		return BackupLifecycleState{}, errors.New("backup disablement operation or fencing epoch is invalid")
	}
	next := current
	next.State, next.Phase, next.Epoch = BackupDisabled, BackupPhaseIdle, nextEpoch
	next.OperationID, next.DisablePlanDigest, next.RequestedAt, next.ActionDeadline = "", "", "", ""
	next.PrerequisiteEffective, next.LocalSupportInstalled = false, false
	// Every schedule stops. A timer left active would keep pushing to a
	// repository the project no longer describes.
	next.Schedules = nil
	if err := next.Seal(); err != nil {
		return BackupLifecycleState{}, err
	}
	return next, nil
}
