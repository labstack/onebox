package app

import (
	"fmt"
	"regexp"
	"sort"
)

type serviceDeliveryClass string

const (
	deliveryUpstreamDigest serviceDeliveryClass = "upstream-digest"
	deliveryDerivedImage   serviceDeliveryClass = "derived-image"
	deliveryExternalHelper serviceDeliveryClass = "external-helper"
)

type repositoryOwnership string

const (
	repositoryNativeDirect repositoryOwnership = "native-direct"
	repositoryArtifact     repositoryOwnership = "artifact"
)

// lifecycleCapability is deliberately separate from the runtime driver. A
// service can be runnable without an executable backup contract, and an absent
// record never inherits volume-copy or generic-helper behavior.
type lifecycleCapability interface {
	DriverName() string
	ProtectionQualified(version string) bool
	SupportsRecoveryKind(version, recoveryKind string) bool
	Record() lifecycleCapabilityRecord
}

type lifecycleArtifactProvenance struct {
	Repository     string `json:"repository"`
	Digest         string `json:"digest"`
	UpstreamDigest string `json:"upstream_digest"`
	SBOMDigest     string `json:"sbom_digest"`
	ProvenanceID   string `json:"provenance_id"`
}

type lifecycleVersionRange struct {
	Pattern string
}

type protectedPatchTransition struct {
	CurrentServiceDigest   string
	CandidateServiceDigest string
	CurrentHelperDigest    string
	CandidateHelperDigest  string
	MaintenanceRange       string
	CompatibilityProbes    []string
	ContinuityProbes       []string
	RollbackLimit          string
}

type lifecyclePrecondition struct {
	Code            string `json:"code"`
	Consistency     string `json:"consistency"`
	Topology        string `json:"topology"`
	RestartRequired bool   `json:"restart_required"`
}

type lifecycleOperations struct {
	Backup  string
	Restore string
	Verify  string
}

// lifecycleCapabilityRecord models every generic seam. policyQualified means
// the project schema may accept the contract; graduated is separately gated by
// live backup/restore evidence and is never inferred from this record.
type lifecycleCapabilityRecord struct {
	driver             string
	policyQualified    bool
	graduated          bool
	recoveryKinds      map[string]bool
	delivery           serviceDeliveryClass
	serviceArtifact    lifecycleArtifactProvenance
	helperArtifact     *lifecycleArtifactProvenance
	supportedVersions  []lifecycleVersionRange
	patchTransitions   []protectedPatchTransition
	repository         repositoryOwnership
	encryptionByKind   map[string]string
	retentionMapping   string
	preconditions      []lifecyclePrecondition
	achievableRPO      string
	credentialSlots    []string
	protectedResources []string
	operations         lifecycleOperations
	graduationEvidence []string
}

func (record lifecycleCapabilityRecord) DriverName() string { return record.driver }

func (record lifecycleCapabilityRecord) Record() lifecycleCapabilityRecord { return record }

func (record lifecycleCapabilityRecord) ProtectionQualified(version string) bool {
	return record.policyQualified && record.supportsVersion(version)
}

func (record lifecycleCapabilityRecord) SupportsRecoveryKind(version, recoveryKind string) bool {
	return record.ProtectionQualified(version) && record.recoveryKinds[recoveryKind]
}

func (record lifecycleCapabilityRecord) supportsVersion(version string) bool {
	for _, supported := range record.supportedVersions {
		matched, err := regexp.MatchString(supported.Pattern, version)
		if err == nil && matched {
			return true
		}
	}
	return false
}

func (record lifecycleCapabilityRecord) validate() error {
	if _, ok := drivers[record.driver]; !ok && record.driver != "_test_lifecycle" {
		return fmt.Errorf("lifecycle driver %q is not a runtime driver", record.driver)
	}
	if len(record.recoveryKinds) == 0 {
		return fmt.Errorf("lifecycle driver %q has no recovery kinds", record.driver)
	}
	if record.delivery != deliveryUpstreamDigest && record.delivery != deliveryDerivedImage && record.delivery != deliveryExternalHelper {
		return fmt.Errorf("lifecycle driver %q has invalid service delivery class", record.driver)
	}
	if err := record.serviceArtifact.validate("service artifact"); err != nil {
		return fmt.Errorf("lifecycle driver %q: %w", record.driver, err)
	}
	if record.delivery == deliveryExternalHelper && record.helperArtifact == nil {
		return fmt.Errorf("lifecycle driver %q external-helper delivery has no helper provenance", record.driver)
	}
	if record.helperArtifact != nil {
		if err := record.helperArtifact.validate("helper artifact"); err != nil {
			return fmt.Errorf("lifecycle driver %q: %w", record.driver, err)
		}
	}
	if len(record.supportedVersions) == 0 {
		return fmt.Errorf("lifecycle driver %q has no supported version range", record.driver)
	}
	for _, supported := range record.supportedVersions {
		if _, err := regexp.Compile(supported.Pattern); err != nil {
			return fmt.Errorf("lifecycle driver %q has invalid version range", record.driver)
		}
	}
	if record.repository != repositoryNativeDirect && record.repository != repositoryArtifact {
		return fmt.Errorf("lifecycle driver %q has invalid repository ownership", record.driver)
	}
	for kind := range record.recoveryKinds {
		if !contains(eRecoveryKind, kind) {
			return fmt.Errorf("lifecycle driver %q has invalid recovery kind %q", record.driver, kind)
		}
		if !contains(eEncryptionMode, record.encryptionByKind[kind]) {
			return fmt.Errorf("lifecycle driver %q has no valid encryption mode for %q", record.driver, kind)
		}
	}
	if !contains([]string{"pgbackrest", "pbm", "clickhouse-chain", "artifact", "snapshot"}, record.retentionMapping) {
		return fmt.Errorf("lifecycle driver %q has invalid native retention mapping", record.driver)
	}
	if _, err := PositiveDuration(record.achievableRPO); err != nil {
		return fmt.Errorf("lifecycle driver %q has invalid achievable RPO", record.driver)
	}
	if len(record.preconditions) == 0 {
		return fmt.Errorf("lifecycle driver %q has no consistency/topology preconditions", record.driver)
	}
	for _, precondition := range record.preconditions {
		if !gIdent.pattern.MatchString(precondition.Code) || precondition.Consistency == "" || precondition.Topology == "" {
			return fmt.Errorf("lifecycle driver %q has incomplete precondition metadata", record.driver)
		}
	}
	if len(record.credentialSlots) == 0 || len(record.protectedResources) == 0 {
		return fmt.Errorf("lifecycle driver %q has no credentials or protected resources", record.driver)
	}
	for _, slot := range record.credentialSlots {
		if !gEnvName.pattern.MatchString(slot) {
			return fmt.Errorf("lifecycle driver %q has unsafe credential slot metadata", record.driver)
		}
	}
	for _, resource := range record.protectedResources {
		if !gFailureDomain.pattern.MatchString(resource) {
			return fmt.Errorf("lifecycle driver %q has unsafe protected resource metadata", record.driver)
		}
	}
	for name, operation := range map[string]string{"backup": record.operations.Backup, "restore": record.operations.Restore, "verify": record.operations.Verify} {
		if !gIdent.pattern.MatchString(operation) {
			return fmt.Errorf("lifecycle driver %q has invalid %s operation", record.driver, name)
		}
	}
	if len(record.graduationEvidence) == 0 {
		return fmt.Errorf("lifecycle driver %q has no graduation evidence contract", record.driver)
	}
	for _, evidence := range record.graduationEvidence {
		if !gIdent.pattern.MatchString(evidence) {
			return fmt.Errorf("lifecycle driver %q has unsafe graduation evidence metadata", record.driver)
		}
	}
	if record.graduated && !record.policyQualified {
		return fmt.Errorf("lifecycle driver %q cannot graduate without a qualified policy", record.driver)
	}
	for i, transition := range record.patchTransitions {
		if err := transition.validate(record.delivery, record.helperArtifact != nil); err != nil {
			return fmt.Errorf("lifecycle driver %q transition %d: %w", record.driver, i, err)
		}
	}
	return nil
}

func (artifact lifecycleArtifactProvenance) validate(name string) error {
	if artifact.Repository == "" || !lifecycleDigest.MatchString(artifact.Digest) ||
		!lifecycleDigest.MatchString(artifact.UpstreamDigest) || !lifecycleDigest.MatchString(artifact.SBOMDigest) ||
		!gFailureDomain.pattern.MatchString(artifact.ProvenanceID) {
		return fmt.Errorf("%s provenance is incomplete or unpinned", name)
	}
	return nil
}

func (transition protectedPatchTransition) validate(delivery serviceDeliveryClass, helper bool) error {
	if !lifecycleDigest.MatchString(transition.CurrentServiceDigest) || !lifecycleDigest.MatchString(transition.CandidateServiceDigest) ||
		transition.CurrentServiceDigest == transition.CandidateServiceDigest {
		return fmt.Errorf("service digests do not identify an exact transition")
	}
	if helper && (!lifecycleDigest.MatchString(transition.CurrentHelperDigest) || !lifecycleDigest.MatchString(transition.CandidateHelperDigest)) {
		return fmt.Errorf("helper digests do not identify an exact transition")
	}
	if !helper && (transition.CurrentHelperDigest != "" || transition.CandidateHelperDigest != "") {
		return fmt.Errorf("transition invents helper digests for %s delivery", delivery)
	}
	if transition.MaintenanceRange == "" || len(transition.CompatibilityProbes) == 0 || len(transition.ContinuityProbes) == 0 || transition.RollbackLimit == "" {
		return fmt.Errorf("transition lacks range, probes, or rollback limit")
	}
	return nil
}

var lifecycleDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

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
// trusted target-side credential file. Values never cross this catalogue API.
func LifecycleCredentialSlots(driverName, version string) ([]string, bool) {
	capability, ok := lifecycleCapabilityFor(driverName)
	if !ok || !capability.ProtectionQualified(version) {
		return nil, false
	}
	slots := append([]string(nil), capability.Record().credentialSlots...)
	sort.Strings(slots)
	return slots, true
}

func validateLifecycleCatalogue() error {
	if len(lifecycleCapabilities) != len(drivers) {
		return fmt.Errorf("lifecycle catalogue has %d records for %d runtime drivers", len(lifecycleCapabilities), len(drivers))
	}
	for _, driverName := range sortedKeys(lifecycleCapabilities) {
		capability := lifecycleCapabilities[driverName]
		if capability.DriverName() != driverName {
			return fmt.Errorf("lifecycle record %q identifies driver %q", driverName, capability.DriverName())
		}
		if err := capability.Record().validate(); err != nil {
			return err
		}
	}
	return nil
}
