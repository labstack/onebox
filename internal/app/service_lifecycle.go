package app

import (
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
