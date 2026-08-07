package onebox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const ProtectionRemovalPlanSchemaVersion = "onebox.run/protection-removal-plan/v1alpha1"

type ProtectionResourceKind string

const (
	ProtectionResourceUnit     ProtectionResourceKind = "unit"
	ProtectionResourceHook     ProtectionResourceKind = "hook"
	ProtectionResourceConfig   ProtectionResourceKind = "config"
	ProtectionResourceEnvelope ProtectionResourceKind = "envelope"
	ProtectionResourceRunner   ProtectionResourceKind = "runner"
	ProtectionRemoteBackup     ProtectionResourceKind = "remote-backup"
	ProtectionRemoteReplica    ProtectionResourceKind = "remote-replica"
	ProtectionManifest         ProtectionResourceKind = "manifest"
	ProtectionManifestImage    ProtectionResourceKind = "manifest-image"
	ProtectionServiceVolume    ProtectionResourceKind = "service-volume"
	ProtectionPreviousVolume   ProtectionResourceKind = "previous-volume"
)

type ProtectionResource struct {
	Identity               string                 `json:"identity"`
	Kind                   ProtectionResourceKind `json:"kind"`
	OwnerApplication       string                 `json:"owner_application,omitempty"`
	OwnerEnvironment       string                 `json:"owner_environment,omitempty"`
	Service                string                 `json:"service,omitempty"`
	Referenced             bool                   `json:"referenced,omitempty"`
	RequiredByPrerequisite bool                   `json:"required_by_prerequisite,omitempty"`
}

type ProtectionResourceInspection struct {
	Application string               `json:"application"`
	Environment string               `json:"environment"`
	Owned       []ProtectionResource `json:"owned,omitempty"`
	Preserved   []ProtectionResource `json:"preserved,omitempty"`
	Foreign     []ProtectionResource `json:"foreign,omitempty"`
}

type ProtectionRemovalRequest struct {
	Mode                        OperationKind `json:"mode"`
	Application                 string        `json:"application"`
	Environment                 string        `json:"environment"`
	Service                     string        `json:"service,omitempty"`
	ProtectionState             string        `json:"protection_state"`
	StateDigest                 string        `json:"state_digest"`
	PrerequisitesVerifiedAbsent bool          `json:"prerequisites_verified_absent"`
}

type ProtectionRemovalPlan struct {
	SchemaVersion string               `json:"schema_version"`
	Mode          OperationKind        `json:"mode"`
	Application   string               `json:"application"`
	Environment   string               `json:"environment"`
	Service       string               `json:"service,omitempty"`
	StateDigest   string               `json:"state_digest"`
	Remove        []ProtectionResource `json:"remove,omitempty"`
	Preserve      []ProtectionResource `json:"preserve,omitempty"`
	PlanDigest    string               `json:"plan_digest"`
}

// ProtectionRemovalAuthorization is issued only after the approval boundary
// validates the sealed plan and live state. It carries no generic force flag.
type ProtectionRemovalAuthorization struct {
	Operation   OperationKind `json:"operation"`
	PlanDigest  string        `json:"plan_digest"`
	StateDigest string        `json:"state_digest"`
}

func InspectProtectionResources(application, environment string, resources []ProtectionResource) (ProtectionResourceInspection, error) {
	if !safeLifecycleMetadata(application) || !safeLifecycleMetadata(environment) {
		return ProtectionResourceInspection{}, errors.New("application and environment must be safe ownership metadata")
	}
	inspection := ProtectionResourceInspection{Application: application, Environment: environment}
	seen := make(map[string]struct{}, len(resources))
	for index, resource := range resources {
		if err := resource.validate(); err != nil {
			return ProtectionResourceInspection{}, fmt.Errorf("resources[%d]: %w", index, err)
		}
		key := string(resource.Kind) + "\x00" + resource.Identity
		if _, exists := seen[key]; exists {
			return ProtectionResourceInspection{}, fmt.Errorf("duplicate protection resource %q", resource.Identity)
		}
		seen[key] = struct{}{}
		switch {
		case resource.OwnerApplication != application || resource.OwnerEnvironment != environment:
			inspection.Foreign = append(inspection.Foreign, resource)
		case protectionResourceAlwaysPreserved(resource.Kind):
			inspection.Preserved = append(inspection.Preserved, resource)
		default:
			inspection.Owned = append(inspection.Owned, resource)
		}
	}
	sortProtectionResources(inspection.Owned)
	sortProtectionResources(inspection.Preserved)
	sortProtectionResources(inspection.Foreign)
	return inspection, nil
}

func NewProtectionRemovalPlan(inspection ProtectionResourceInspection, request ProtectionRemovalRequest) (ProtectionRemovalPlan, error) {
	if request.Application != inspection.Application || request.Environment != inspection.Environment {
		return ProtectionRemovalPlan{}, errors.New("removal request does not match inspected ownership")
	}
	if request.Mode != KindProtectionDisable && request.Mode != KindDestroy {
		return ProtectionRemovalPlan{}, fmt.Errorf("unsupported protection removal mode %q", request.Mode)
	}
	if !lifecycleGraphDigest.MatchString(request.StateDigest) {
		return ProtectionRemovalPlan{}, errors.New("removal state_digest must be sha256:<64 lowercase hex>")
	}
	if !request.PrerequisitesVerifiedAbsent {
		failure, _ := NewLifecycleFailure("protection_image_revert_unsafe")
		return ProtectionRemovalPlan{}, failure
	}
	if request.Mode == KindProtectionDisable {
		if request.ProtectionState != "disabled" || !safeLifecycleMetadata(request.Service) {
			failure, _ := NewLifecycleFailure("protection_disable_pending")
			return ProtectionRemovalPlan{}, failure
		}
	} else if request.ProtectionState != "disabled" && request.ProtectionState != "never-enabled" {
		failure, _ := NewLifecycleFailure("protection_disable_pending")
		return ProtectionRemovalPlan{}, failure
	}
	plan := ProtectionRemovalPlan{
		SchemaVersion: ProtectionRemovalPlanSchemaVersion,
		Mode:          request.Mode, Application: request.Application, Environment: request.Environment,
		Service: request.Service, StateDigest: request.StateDigest,
		Preserve: append([]ProtectionResource(nil), inspection.Preserved...),
	}
	plan.Preserve = append(plan.Preserve, inspection.Foreign...)
	for _, resource := range inspection.Owned {
		remove := !resource.RequiredByPrerequisite && !resource.Referenced
		if request.Mode == KindProtectionDisable {
			remove = remove && resource.Service == request.Service
		}
		if remove {
			plan.Remove = append(plan.Remove, resource)
		} else {
			plan.Preserve = append(plan.Preserve, resource)
		}
	}
	sortProtectionResources(plan.Remove)
	sortProtectionResources(plan.Preserve)
	if err := plan.Seal(); err != nil {
		return ProtectionRemovalPlan{}, err
	}
	return plan, nil
}

func ApplyProtectionRemoval(plan ProtectionRemovalPlan, authorization ProtectionRemovalAuthorization, remove func(ProtectionResource) error) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if authorization.Operation != plan.Mode || authorization.PlanDigest != plan.PlanDigest || authorization.StateDigest != plan.StateDigest {
		failure, _ := NewLifecycleFailure("protection_disablement_not_authorized")
		return failure
	}
	if remove == nil {
		return errors.New("protection resource remover is nil")
	}
	for _, resource := range plan.Remove {
		if protectionResourceAlwaysPreserved(resource.Kind) {
			return fmt.Errorf("refusing to remove preserved protection resource %q", resource.Identity)
		}
		if err := remove(resource); err != nil {
			return fmt.Errorf("remove owned protection resource %q: %w", resource.Identity, err)
		}
	}
	return nil
}

func (plan *ProtectionRemovalPlan) Seal() error {
	if plan == nil {
		return errors.New("protection removal plan is nil")
	}
	if err := plan.validateContent(); err != nil {
		return err
	}
	digest, err := plan.computeDigest()
	if err != nil {
		return err
	}
	plan.PlanDigest = digest
	return nil
}

func (plan ProtectionRemovalPlan) Validate() error {
	if err := plan.validateContent(); err != nil {
		return err
	}
	if !lifecycleGraphDigest.MatchString(plan.PlanDigest) {
		return errors.New("protection removal plan_digest is required")
	}
	expected, err := plan.computeDigest()
	if err != nil {
		return err
	}
	if plan.PlanDigest != expected {
		return errors.New("protection removal plan digest mismatch")
	}
	return nil
}

func (plan ProtectionRemovalPlan) validateContent() error {
	if plan.SchemaVersion != ProtectionRemovalPlanSchemaVersion {
		return fmt.Errorf("unsupported protection removal schema %q", plan.SchemaVersion)
	}
	if plan.Mode != KindProtectionDisable && plan.Mode != KindDestroy {
		return fmt.Errorf("unsupported protection removal mode %q", plan.Mode)
	}
	if !safeLifecycleMetadata(plan.Application) || !safeLifecycleMetadata(plan.Environment) {
		return errors.New("protection removal ownership is invalid")
	}
	if plan.Mode == KindProtectionDisable && !safeLifecycleMetadata(plan.Service) {
		return errors.New("protection disable removal requires a service")
	}
	if !lifecycleGraphDigest.MatchString(plan.StateDigest) {
		return errors.New("protection removal state_digest is invalid")
	}
	seen := make(map[string]struct{}, len(plan.Remove)+len(plan.Preserve))
	for _, group := range [][]ProtectionResource{plan.Remove, plan.Preserve} {
		previous := ""
		for _, resource := range group {
			if err := resource.validate(); err != nil {
				return err
			}
			key := string(resource.Kind) + "\x00" + resource.Identity
			if previous != "" && key <= previous {
				return errors.New("protection removal resources must be unique and sorted")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("protection resource %q appears more than once", resource.Identity)
			}
			seen[key] = struct{}{}
			previous = key
		}
	}
	for _, resource := range plan.Remove {
		if resource.OwnerApplication != plan.Application || resource.OwnerEnvironment != plan.Environment || protectionResourceAlwaysPreserved(resource.Kind) {
			return fmt.Errorf("protection removal plan contains non-removable resource %q", resource.Identity)
		}
	}
	return nil
}

func (plan ProtectionRemovalPlan) computeDigest() (string, error) {
	copy := plan
	copy.PlanDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (resource ProtectionResource) validate() error {
	if strings.TrimSpace(resource.Identity) == "" || strings.ContainsAny(resource.Identity, "\r\n\x00") {
		return errors.New("resource identity must be non-empty single-line metadata")
	}
	if resource.OwnerApplication != "" && !safeLifecycleMetadata(resource.OwnerApplication) {
		return errors.New("resource owner application is invalid")
	}
	if resource.OwnerEnvironment != "" && !safeLifecycleMetadata(resource.OwnerEnvironment) {
		return errors.New("resource owner environment is invalid")
	}
	if resource.Service != "" && !safeLifecycleMetadata(resource.Service) {
		return errors.New("resource service is invalid")
	}
	switch resource.Kind {
	case ProtectionResourceUnit, ProtectionResourceHook, ProtectionResourceConfig, ProtectionResourceEnvelope,
		ProtectionResourceRunner, ProtectionRemoteBackup, ProtectionRemoteReplica, ProtectionManifest,
		ProtectionManifestImage, ProtectionServiceVolume, ProtectionPreviousVolume:
		return nil
	default:
		return fmt.Errorf("unknown protection resource kind %q", resource.Kind)
	}
}

func protectionResourceAlwaysPreserved(kind ProtectionResourceKind) bool {
	switch kind {
	case ProtectionRemoteBackup, ProtectionRemoteReplica, ProtectionManifest, ProtectionManifestImage,
		ProtectionServiceVolume, ProtectionPreviousVolume:
		return true
	default:
		return false
	}
}

func sortProtectionResources(resources []ProtectionResource) {
	sort.Slice(resources, func(i, j int) bool {
		left := string(resources[i].Kind) + "\x00" + resources[i].Identity
		right := string(resources[j].Kind) + "\x00" + resources[j].Identity
		return left < right
	})
}
