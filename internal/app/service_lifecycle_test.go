package app

import "testing"

func TestProductionLifecycleCatalogueIsComplete(t *testing.T) {
	if err := validateLifecycleCatalogue(); err != nil {
		t.Fatalf("validate lifecycle catalogue: %v", err)
	}
	if got, want := len(lifecycleCapabilities), 11; got != want {
		t.Fatalf("lifecycle capability count = %d, want %d", got, want)
	}

	for driverName := range drivers {
		capability, ok := lifecycleCapabilityFor(driverName)
		if !ok {
			t.Errorf("runtime driver %q has no lifecycle record", driverName)
			continue
		}
		record := capability.Record()
		if record.graduated {
			t.Errorf("driver %q graduated without runtime evidence", driverName)
		}
		if len(record.patchTransitions) != 0 {
			t.Errorf("driver %q has a default protected patch transition", driverName)
		}
		if len(record.encryptionByKind) != len(record.recoveryKinds) {
			t.Errorf("driver %q does not model encryption for every recovery kind", driverName)
		}
	}
}

func TestInternalLifecycleDriverExercisesEverySeamWithoutGraduating(t *testing.T) {
	record := nonGraduatingTestLifecycleCapability()
	if err := record.validate(); err != nil {
		t.Fatalf("validate internal lifecycle driver: %v", err)
	}
	if !record.policyQualified || record.graduated {
		t.Fatalf("internal driver qualification/graduation = %v/%v, want true/false", record.policyQualified, record.graduated)
	}
	if !record.SupportsRecoveryKind("1", "pitr") {
		t.Fatal("internal driver does not exercise versioned recovery-kind selection")
	}
	if record.delivery != deliveryExternalHelper || record.helperArtifact == nil {
		t.Fatal("internal driver does not exercise helper delivery and provenance")
	}
	if len(record.patchTransitions) != 1 {
		t.Fatalf("internal driver patch transitions = %d, want 1", len(record.patchTransitions))
	}
	if len(record.preconditions) == 0 || !record.preconditions[0].RestartRequired {
		t.Fatal("internal driver does not exercise restart-gated enablement")
	}
	if record.repository == "" || record.retentionMapping == "" || record.achievableRPO == "" ||
		len(record.credentialSlots) == 0 || len(record.protectedResources) == 0 || len(record.graduationEvidence) == 0 ||
		record.operations.Backup == "" || record.operations.Restore == "" || record.operations.Verify == "" {
		t.Fatal("internal driver does not exercise every lifecycle seam")
	}
	if _, ok := drivers[record.driver]; ok {
		t.Fatal("internal driver leaked into the runtime/schema catalogue")
	}
	if _, ok := lifecycleCapabilities[record.driver]; ok {
		t.Fatal("internal driver leaked into the lifecycle/status catalogue")
	}
}

func TestProtectedPatchTransitionsAreExact(t *testing.T) {
	record := nonGraduatingTestLifecycleCapability()
	transition := record.patchTransitions[0]
	transition.CandidateServiceDigest = transition.CurrentServiceDigest
	if err := transition.validate(record.delivery, true); err == nil {
		t.Fatal("same-digest protected patch transition was accepted")
	}

	transition = record.patchTransitions[0]
	transition.ContinuityProbes = nil
	if err := transition.validate(record.delivery, true); err == nil {
		t.Fatal("protected patch transition without continuity probes was accepted")
	}
}
