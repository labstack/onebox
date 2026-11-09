package app

import (
	"fmt"
	"regexp"
)

// Helpers only tests call. They wrap or compose production code, and live
// here so the shipped binary carries no function nothing reaches.

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
