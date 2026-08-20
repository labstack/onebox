package app

import (
	"fmt"
	"regexp"
	"sort"
)

// lifecycleCapability records what a driver's backup contract can actually do.
//
// It is deliberately separate from the runtime driver: a service can be
// runnable without an executable backup contract, and a driver with no record
// never inherits one.
//
// Only what gates behaviour lives here. An earlier version of this catalogue
// also carried delivery classes, repository ownership, artifact and SBOM
// provenance, patch transitions with continuity probes, consistency and
// topology preconditions, achievable RPO, protected resource names, native
// retention mappings, per-kind encryption modes, operation identifiers, and a
// graduation-evidence contract — around 450 lines describing a system that did
// not exist. Nothing read any of it except the function that validated it
// against itself, and the provenance digests were placeholders (sixty-four
// repetitions of one hex character) presented in the same shape as the one real
// checksum in the repository. Metadata that describes nothing and proves
// nothing is worse than absent: it reads as a guarantee.
type lifecycleCapability struct {
	driver          string
	policyQualified bool
	recoveryKinds   map[string]bool
	// Version patterns the contract is qualified for, as regular expressions.
	supportedVersions []string
	// Names the driver expects in its target-side credential file. Values never
	// cross this catalogue.
	credentialSlots []string
}

func (capability lifecycleCapability) BackupQualified(version string) bool {
	return capability.policyQualified && capability.supportsVersion(version)
}

func (capability lifecycleCapability) SupportsRecoveryKind(version, recoveryKind string) bool {
	return capability.BackupQualified(version) && capability.recoveryKinds[recoveryKind]
}

func (capability lifecycleCapability) supportsVersion(version string) bool {
	for _, pattern := range capability.supportedVersions {
		matched, err := regexp.MatchString(pattern, version)
		if err == nil && matched {
			return true
		}
	}
	return false
}

func (capability lifecycleCapability) validate() error {
	if _, ok := drivers[capability.driver]; !ok {
		return fmt.Errorf("lifecycle driver %q is not a runtime driver", capability.driver)
	}
	if len(capability.recoveryKinds) == 0 {
		return fmt.Errorf("lifecycle driver %q has no recovery kinds", capability.driver)
	}
	for kind := range capability.recoveryKinds {
		if !contains(eRecoveryKind, kind) {
			return fmt.Errorf("lifecycle driver %q has invalid recovery kind %q", capability.driver, kind)
		}
	}
	if len(capability.supportedVersions) == 0 {
		return fmt.Errorf("lifecycle driver %q has no supported version range", capability.driver)
	}
	for _, pattern := range capability.supportedVersions {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("lifecycle driver %q has invalid version range", capability.driver)
		}
	}
	if len(capability.credentialSlots) == 0 {
		return fmt.Errorf("lifecycle driver %q has no credential slots", capability.driver)
	}
	for _, slot := range capability.credentialSlots {
		if !gEnvName.pattern.MatchString(slot) {
			return fmt.Errorf("lifecycle driver %q has unsafe credential slot metadata", capability.driver)
		}
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func lifecycleCapabilityFor(driverName string) (lifecycleCapability, bool) {
	capability, ok := lifecycleCapabilities[driverName]
	return capability, ok
}

// LifecycleCredentialSlots returns the names a qualified driver expects in its
// trusted target-side credential file.
func LifecycleCredentialSlots(driverName, version string) ([]string, bool) {
	capability, ok := lifecycleCapabilityFor(driverName)
	if !ok || !capability.BackupQualified(version) {
		return nil, false
	}
	slots := append([]string(nil), capability.credentialSlots...)
	sort.Strings(slots)
	return slots, true
}

func validateLifecycleCatalogue() error {
	if len(lifecycleCapabilities) != len(drivers) {
		return fmt.Errorf("lifecycle catalogue has %d records for %d runtime drivers", len(lifecycleCapabilities), len(drivers))
	}
	for _, driverName := range sortedKeys(lifecycleCapabilities) {
		capability := lifecycleCapabilities[driverName]
		if capability.driver != driverName {
			return fmt.Errorf("lifecycle record %q identifies driver %q", driverName, capability.driver)
		}
		if err := capability.validate(); err != nil {
			return err
		}
	}
	return nil
}
