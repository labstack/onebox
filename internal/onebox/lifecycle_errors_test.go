package onebox

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEveryDeltaSpecFailureHasASecretFreeGuidanceContract(t *testing.T) {
	expected := []string{
		"backup_conflict",
		"backup_disable_pending",
		"backup_disablement_overdue",
		"backup_driver_unsupported",
		"backup_encryption_unverified",
		"backup_image_revert_unsafe",
		"backup_interruption_not_authorized",
		"backup_retention_unsupported",
		"backup_service_image_unpublished",
		"backup_target_not_independent",
		"backup_target_unknown",
		"drill_schedule_too_sparse",
		"recovery_objective_unsupported",
		"service_image_digest_unavailable",
		"service_image_patch_disable_pending",
		"service_patch_unsupported",
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
			if !safeGuidanceCommand(failure.GuidanceCommand()) || failure.GuidanceRole() == "" {
				t.Fatalf("unsafe command guidance: %+v", failure)
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

		})
	}
}

func TestLifecycleFailureValidationDoesNotReflectUnsafeReplacement(t *testing.T) {
	failure, err := NewLifecycleFailure("backup_target_unknown")
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

func TestGuidanceRoleForCommandDoesNotCallReadsResolving(t *testing.T) {
	tests := map[string]string{
		"ob status --output json":                       "diagnostic",
		"ob backup target inspect --output json":        "diagnostic",
		"ob secrets list --output json":                 "diagnostic",
		"ob plan --output json":                         "next",
		"ob approve --plan <plan> --out <confirmation>": "next",
		"ob service apply --output ndjson":              "resolving",
	}
	for command, want := range tests {
		if got := GuidanceRoleForCommand(command); got != want {
			t.Errorf("GuidanceRoleForCommand(%q) = %q, want %q", command, got, want)
		}
	}
}
