package onebox

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
)

func protectionStateProjection() app.ProtectionEffectiveProjection {
	return app.ProtectionEffectiveProjection{
		Policy: app.ProtectionPolicy{
			Target: "offsite", RecoveryKind: "pitr", MaximumDataLoss: "5m",
			Schedule:  app.Schedule{Cron: "17 */6 * * *", Timezone: "UTC"},
			Retention: app.ProtectionRetention{MinimumGenerations: 7, RecoveryWindow: "7d"},
			RestoreDrill: app.RestoreDrillPolicy{
				Schedule: app.Schedule{Cron: "23 4 * * 1,4", Timezone: "UTC"}, ProofMaximumAge: "7d",
			},
		},
		Target: app.BackupTarget{
			Kind: "s3-compatible", Endpoint: "https://objects.example.test", Bucket: "onebox-backups",
			TLS: "required", FailureDomain: app.FailureDomain{Identity: "provider-a/us-east-1/account-42"},
			Credentials: app.CredentialReference{
				File: "secrets/backup.env", Provider: "sops", AccessKeyEntry: "BACKUP_ACCESS_KEY_ID", SecretKeyEntry: "BACKUP_SECRET_ACCESS_KEY",
			},
			Encryption: app.TargetEncryption{PITR: "archive-password"},
		},
	}
}

func pendingProtectionState(t *testing.T) (ProtectionLifecycleState, ProtectionDisablePlan, ProtectionDisableAuthorization, time.Time) {
	t.Helper()
	initial, err := NewProtectionLifecycleState("example", "production", "database", 1)
	if err != nil {
		t.Fatal(err)
	}
	image := "postgres@sha256:" + strings.Repeat("a", 64)
	enabled, err := EnableProtection(initial, protectionStateProjection(), image, "enable-op", true, 2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	pending, err := RequestProtectionDisable(enabled, "disable-op", now, 3)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewProtectionDisablePlan(pending)
	if err != nil {
		t.Fatal(err)
	}
	authorization := ProtectionDisableAuthorization{
		OperationID: plan.OperationID, PlanDigest: plan.PlanDigest, StateDigest: plan.StateDigest, Strong: true,
	}
	return pending, plan, authorization, now
}

func TestProtectionDisableRequiresStrongApprovalAndKeepsStorageSchedules(t *testing.T) {
	pending, plan, _, _ := pendingProtectionState(t)
	_, err := AdvanceProtectionDisable(pending, plan, ProtectionDisableAuthorization{}, ProtectionPhasePrerequisiteReversed, 4)
	var failure LifecycleFailure
	if !errors.As(err, &failure) || failure.Code != "protection_disablement_not_authorized" {
		t.Fatalf("missing approval error = %v", err)
	}
	want := []string{"backup-create", "backup-prune", "replay-archive"}
	if got := pending.activeScheduleKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pending schedules = %#v, want %#v", got, want)
	}
	for _, schedule := range pending.Schedules {
		if schedule.Kind == "restore-drill" && schedule.Active {
			t.Fatal("restore drill remained active during disable-pending")
		}
		if schedule.Kind == "replay-archive" && schedule.Schedule.Cron != "*/5 * * * *" {
			t.Fatalf("replay archive schedule = %q, want cadence bounded by 5m RPO", schedule.Schedule.Cron)
		}
	}
}

func TestProtectionDisableOperationGatesAndSafeImageRetention(t *testing.T) {
	pending, _, _, _ := pendingProtectionState(t)
	if err := pending.AllowOperation(KindDeploy, false); err != nil {
		t.Fatalf("unrelated apply was refused: %v", err)
	}
	if err := pending.AllowOperation(KindBackupCreate, true); err != nil {
		t.Fatalf("retained backup operation was refused: %v", err)
	}
	for _, test := range []struct {
		kind OperationKind
		code string
	}{
		{KindServiceImagePatch, "service_image_patch_disable_pending"},
		{KindRestoreTest, "protection_disable_pending"},
	} {
		err := pending.AllowOperation(test.kind, true)
		var failure LifecycleFailure
		if !errors.As(err, &failure) || failure.Code != test.code {
			t.Fatalf("operation %s error = %v, want %s", test.kind, err, test.code)
		}
	}
	if err := pending.ValidateRuntimeImage("postgres:17"); err == nil || !strings.Contains(err.Error(), "protection_image_revert_unsafe") {
		t.Fatalf("unsafe image reversion error = %v", err)
	}
}

func TestProtectionDisableStatusBecomesOverdueWithoutMutation(t *testing.T) {
	pending, _, _, requestedAt := pendingProtectionState(t)
	before := pending.StateDigest
	status, err := pending.Status(requestedAt.Add(24*time.Hour + time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status.Failure == nil || status.Failure.Code != "protection_disablement_overdue" || !status.StorageContinues {
		t.Fatalf("overdue status = %#v", status)
	}
	if pending.StateDigest != before || status.ResolvingCommand != "ob protection disable --output ndjson" {
		t.Fatal("status mutated state or omitted the exact resolving command")
	}
}

func TestProtectionDisableCrashResumeRetryAndSafeCompletion(t *testing.T) {
	state, plan, authorization, _ := pendingProtectionState(t)
	var err error
	state, err = AdvanceProtectionDisable(state, plan, authorization, ProtectionPhasePrerequisiteReversed, 4)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "protection-state.json")
	if err := SaveProtectionLifecycleState(statePath, state); err != nil {
		t.Fatal(err)
	}
	resumed, err := LoadProtectionLifecycleState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// Output loss after commit: retrying the same phase is an idempotent lookup.
	retried, err := AdvanceProtectionDisable(resumed, plan, authorization, ProtectionPhasePrerequisiteReversed, 5)
	if err != nil {
		t.Fatal(err)
	}
	if retried.StateDigest != resumed.StateDigest || retried.Epoch != resumed.Epoch {
		t.Fatal("same-phase retry created a second transition")
	}
	for index, phase := range []ProtectionDisablePhase{
		ProtectionPhasePrerequisiteAbsent, ProtectionPhaseRuntimeReverted,
		ProtectionPhaseLocalSupportRemoved, ProtectionPhaseComplete,
	} {
		resumed, err = AdvanceProtectionDisable(resumed, plan, authorization, phase, 5+index)
		if err != nil {
			t.Fatalf("advance %s: %v", phase, err)
		}
	}
	if resumed.State != ProtectionDisabled || resumed.PrerequisiteEffective || resumed.LocalSupportInstalled || len(resumed.activeScheduleKinds()) != 0 {
		t.Fatalf("completed disablement = %#v", resumed)
	}
	request, err := resumed.RemovalRequest(KindProtectionDisable)
	if err != nil || !request.PrerequisitesVerifiedAbsent {
		t.Fatalf("safe removal request = %#v, %v", request, err)
	}
	if protectionStateContainsRemoteDeletion(plan) || protectionStateContainsRemoteDeletion(resumed) {
		t.Fatal("disablement invented a remote deletion path")
	}
}

func TestProtectionDisableRollbackIsBounded(t *testing.T) {
	pending, plan, authorization, _ := pendingProtectionState(t)
	rolledBack, err := RollbackProtectionDisable(pending, plan, authorization, 4)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.State != ProtectionEnabled || !rolledBack.PrerequisiteEffective || !rolledBack.LocalSupportInstalled {
		t.Fatalf("rollback state = %#v", rolledBack)
	}
	if !containsString(rolledBack.activeScheduleKinds(), "restore-drill") {
		t.Fatal("rollback did not restore the drill schedule")
	}
	advanced, err := AdvanceProtectionDisable(pending, plan, authorization, ProtectionPhasePrerequisiteReversed, 4)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err = AdvanceProtectionDisable(advanced, plan, authorization, ProtectionPhasePrerequisiteAbsent, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackProtectionDisable(advanced, plan, authorization, 6); err == nil {
		t.Fatal("rollback was accepted after prerequisite absence was verified")
	}
}

func TestProtectionDisableRejectsCompetingInFlightOperation(t *testing.T) {
	pending, _, _, requestedAt := pendingProtectionState(t)
	retried, err := RequestProtectionDisable(pending, "disable-op", requestedAt.Add(time.Minute), 4)
	if err != nil || retried.StateDigest != pending.StateDigest {
		t.Fatalf("same-operation retry = %#v, %v", retried, err)
	}
	_, err = RequestProtectionDisable(pending, "other-op", requestedAt.Add(time.Minute), 4)
	var failure LifecycleFailure
	if !errors.As(err, &failure) || failure.Code != "backup_conflict" {
		t.Fatalf("competing operation error = %v", err)
	}
}

func TestProtectionDisableRuntimeProjectionTreatsRetainedArtifactsAsDesired(t *testing.T) {
	pending, _, _, _ := pendingProtectionState(t)
	projection := protectionStateProjection()
	withIntent := &app.Resolved{
		Spec: &app.Spec{
			Name: "example", BasePath: "/var/lib/onebox",
			Services:      map[string]app.Service{"database": {Driver: "postgres", Version: 17, Protection: &projection.Policy}},
			BackupTargets: map[string]app.BackupTarget{"offsite": projection.Target},
		},
		Env: "production",
	}
	original, err := withIntent.GenerateProtectionArtifacts("database")
	if err != nil {
		t.Fatal(err)
	}
	withoutIntent := &app.Resolved{
		Spec: &app.Spec{Name: "example", BasePath: "/var/lib/onebox", Services: map[string]app.Service{
			"database": {Driver: "postgres", Version: 17},
		}},
		Env: "production",
	}
	retained, err := withoutIntent.WithServiceRuntimeStates(map[string]app.ServiceRuntimeState{"database": pending.RuntimeState()})
	if err != nil {
		t.Fatal(err)
	}
	desired, err := retained.GenerateProtectionArtifacts("database")
	if err != nil {
		t.Fatal(err)
	}
	observed := make(map[string]string, len(original.Artifacts))
	for _, artifact := range original.Artifacts {
		observed[artifact.Class] = artifact.Digest
	}
	if drift := app.CompareProtectionArtifacts(desired, observed); len(drift) != 0 {
		t.Fatalf("retained pending projection reported drift: %#v", drift)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRuntimeStateDoesNotInferImageEvidenceFromReference(t *testing.T) {
	state, err := NewProtectionLifecycleState("example", "production", "database", 1)
	if err != nil {
		t.Fatal(err)
	}
	state.State = ProtectionDisabled
	state.Phase = ProtectionPhaseIdle
	state.ServiceImage = "postgres@sha256:" + strings.Repeat("a", 64)
	if err := state.Seal(); err != nil {
		t.Fatal(err)
	}
	runtime := state.RuntimeState()
	if runtime.PublicationVerified || runtime.DigestAvailable || runtime.CacheVerified {
		t.Fatalf("runtime inferred evidence from an image string: %#v", runtime)
	}
}
