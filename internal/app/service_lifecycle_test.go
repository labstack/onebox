package app

import "testing"

func TestEveryRuntimeDriverHasALifecycleRecord(t *testing.T) {
	if err := validateLifecycleCatalogue(); err != nil {
		t.Fatalf("validate lifecycle catalogue: %v", err)
	}
	if got, want := len(lifecycleCapabilities), 11; got != want {
		t.Fatalf("lifecycle capability count = %d, want %d", got, want)
	}
	for driverName := range drivers {
		if _, ok := lifecycleCapabilityFor(driverName); !ok {
			t.Errorf("runtime driver %q has no lifecycle record", driverName)
		}
	}
}

// Qualification is per version, not per driver: a project declaring a version
// outside the qualified range must be refused rather than accepted on the
// driver's name alone.
func TestQualificationIsScopedToTheSupportedVersions(t *testing.T) {
	postgres, ok := lifecycleCapabilityFor("postgres")
	if !ok {
		t.Fatal("postgres has no lifecycle record")
	}
	for _, tc := range []struct {
		version   string
		qualified bool
	}{
		{"18", true}, {"17.4", true}, {"16", false}, {"19", false}, {"latest", false},
	} {
		if got := postgres.BackupQualified(tc.version); got != tc.qualified {
			t.Errorf("BackupQualified(%q) = %v, want %v", tc.version, got, tc.qualified)
		}
	}
	if postgres.SupportsRecoveryKind("18", "snapshot") {
		t.Error("postgres accepted a recovery kind its contract does not execute")
	}
	if !postgres.SupportsRecoveryKind("18", "pitr") {
		t.Error("postgres refused the recovery kind its contract executes")
	}
}

// An unqualified driver hands out no credential slots, so nothing downstream can
// treat it as having a backup contract.
func TestAnUnqualifiedDriverExposesNoCredentialContract(t *testing.T) {
	if _, ok := LifecycleCredentialSlots("redis", "8"); ok {
		t.Fatal("an unqualified driver exposed credential slots")
	}
	slots, ok := LifecycleCredentialSlots("postgres", "18")
	if !ok || len(slots) == 0 {
		t.Fatalf("postgres credential slots = %v, ok = %v", slots, ok)
	}
}

// The catalogue is fixed data, so its own consistency rules are worth checking
// against a record that breaks each one.
func TestCatalogueValidationRejectsIncompleteRecords(t *testing.T) {
	base := lifecycleCapabilities["postgres"]
	for name, mutate := range map[string]func(*lifecycleCapability){
		"unknown driver":    func(c *lifecycleCapability) { c.driver = "not-a-driver" },
		"no recovery kind":  func(c *lifecycleCapability) { c.recoveryKinds = nil },
		"bad recovery kind": func(c *lifecycleCapability) { c.recoveryKinds = map[string]bool{"eventually": true} },
		"no versions":       func(c *lifecycleCapability) { c.supportedVersions = nil },
		"bad version":       func(c *lifecycleCapability) { c.supportedVersions = []string{"^1[7"} },
		"no credentials":    func(c *lifecycleCapability) { c.credentialSlots = nil },
		"unsafe credential": func(c *lifecycleCapability) { c.credentialSlots = []string{"NOT AN ENV NAME"} },
	} {
		t.Run(name, func(t *testing.T) {
			record := base
			mutate(&record)
			if err := record.validate(); err == nil {
				t.Fatal("an incomplete lifecycle record validated")
			}
		})
	}
}
