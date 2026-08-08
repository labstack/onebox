package onebox

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEveryDeltaSpecFailureHasASecretFreeResolvingContract(t *testing.T) {
	expected := []string{
		"assurance_stale",
		"backup_conflict",
		"backup_driver_unsupported",
		"backup_encryption_unverified",
		"backup_interruption_not_authorized",
		"backup_retention_unsupported",
		"backup_stale",
		"backup_target_not_independent",
		"backup_target_unauthorized",
		"backup_target_unknown",
		"backup_target_unreachable",
		"disk_pressure_critical",
		"drill_deferred_capacity",
		"external_service_not_owned",
		"external_service_state_stale",
		"protected_service_identity_changed",
		"protected_service_patch_incompatible",
		"protected_service_patch_unsupported",
		"protection_disable_pending",
		"protection_disablement_not_authorized",
		"protection_disablement_overdue",
		"protection_enablement_restart_not_authorized",
		"protection_image_revert_unsafe",
		"protection_image_update_overdue",
		"protection_prerequisite_drifted",
		"protection_service_image_unpublished",
		"protection_service_patch_available",
		"protection_service_patch_required",
		"recovery_objective_unsupported",
		"replay_continuity_broken",
		"restore_drill_schedule_too_sparse",
		"restore_state_stale",
		"service_image_digest_unavailable",
		"service_image_patch_disable_pending",
		"service_major_upgrade_unsupported",
	}
	if got := LifecycleFailureCodes(); !reflect.DeepEqual(got, expected) {
		t.Fatalf("lifecycle failure registry =\n%q\nwant\n%q", got, expected)
	}

	for _, code := range expected {
		t.Run(code, func(t *testing.T) {
			failure, err := NewLifecycleFailure(code)
			if err != nil {
				t.Fatal(err)
			}
			if err := failure.Validate(); err != nil {
				t.Fatal(err)
			}
			if !safeLifecycleMetadata(failure.Code) {
				t.Fatalf("code is not stable metadata: %q", failure.Code)
			}
			if !safeResolvingCommand(failure.Next) {
				t.Fatalf("unsafe resolving command: %q", failure.Next)
			}
			encoded, err := json.Marshal(failure)
			if err != nil {
				t.Fatal(err)
			}
			lower := strings.ToLower(string(encoded))
			for _, secretShape := range []string{"userinfo@", "password=", "secret=", "token=", "credential="} {
				if strings.Contains(lower, secretShape) {
					t.Fatalf("public failure contains secret-shaped data %q: %s", secretShape, encoded)
				}
			}

			record := validLifecycleResultRecord(LifecycleBackupCreate, "postgres")
			record.Result.TerminalState = "failed"
			record.Result.ErrorCode = failure.Code
			record.Result.ResolvingCommands = []string{failure.Next}
			if err := record.Validate(); err != nil {
				t.Fatalf("failure does not fit lifecycle result contract: %v", err)
			}
		})
	}
}

func TestLifecycleFailureValidationDoesNotReflectUnsafeReplacement(t *testing.T) {
	failure, err := NewLifecycleFailure("backup_target_unreachable")
	if err != nil {
		t.Fatal(err)
	}
	canary := "password=mcp-must-not-see-this"
	failure.Message = canary
	err = failure.Validate()
	if err == nil {
		t.Fatal("modified public failure was accepted")
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "mcp-must-not-see-this") {
		t.Fatalf("validation reflected unsafe replacement: %v", err)
	}
}
