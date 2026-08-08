package onebox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/labstack/onebox/internal/app"
)

const ScheduledInstallPlanSchemaVersion = "onebox.run/scheduled-install-plan/v1alpha1"

type ScheduledPublicationProof struct {
	ArtifactDigest     string `json:"artifact_digest"`
	SBOMDigest         string `json:"sbom_digest"`
	ProvenanceID       string `json:"provenance_id"`
	Publisher          string `json:"publisher"`
	VerificationMethod string `json:"verification_method"`
	EvidenceDigest     string `json:"evidence_digest"`
	VerifiedAt         string `json:"verified_at"`
	Verified           bool   `json:"verified"`
}

type ScheduledInstallArtifact struct {
	Class   string `json:"class"`
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Owner   string `json:"owner"`
	Group   string `json:"group"`
	Digest  string `json:"digest"`
	Content []byte `json:"-"`
}

type ScheduledInstallPlan struct {
	SchemaVersion    string                           `json:"schema_version"`
	Application      string                           `json:"application"`
	Environment      string                           `json:"environment"`
	Service          string                           `json:"service"`
	Runner           ScheduledRunnerArtifactReference `json:"runner"`
	EnvelopePath     string                           `json:"envelope_path"`
	EnvelopeDigest   string                           `json:"envelope_digest"`
	PublicationProof ScheduledPublicationProof        `json:"publication_proof"`
	Artifacts        []ScheduledInstallArtifact       `json:"artifacts"`
	PlanDigest       string                           `json:"plan_digest"`
}

type ScheduledInstalledMetadata struct {
	Path   string
	Mode   uint32
	Owner  string
	Group  string
	Digest string
}

type ScheduledArtifactTarget interface {
	InstallAtomic(context.Context, ScheduledInstallArtifact) error
	Inspect(context.Context, string) (ScheduledInstalledMetadata, error)
	Remove(context.Context, string) error
}

func NewScheduledInstallPlan(names app.Names, runnerBytes []byte, envelope ScheduledOperationEnvelope, proof ScheduledPublicationProof) (ScheduledInstallPlan, error) {
	if err := envelope.Validate(); err != nil {
		return ScheduledInstallPlan{}, err
	}
	if err := proof.Validate(); err != nil {
		return ScheduledInstallPlan{}, err
	}
	runnerDigest := digestScheduledBytes(runnerBytes)
	if runnerDigest != envelope.Runner.Digest || proof.ArtifactDigest != runnerDigest || proof.SBOMDigest != envelope.Runner.SBOMDigest || proof.ProvenanceID != envelope.Runner.ProvenanceID {
		return ScheduledInstallPlan{}, errors.New("scheduled runner bytes, envelope reference, and publication proof do not match")
	}
	wantRunnerPath := names.ProtectionRunnerPath(runnerDigest)
	if envelope.Runner.Path != wantRunnerPath {
		return ScheduledInstallPlan{}, errors.New("scheduled runner path is outside the digest-derived Onebox layout")
	}
	envelopePath := names.ProtectionEnvelopePath(envelope.Service, string(envelope.Operation))
	envelopeBytes, err := EncodeScheduledOperationEnvelope(envelope)
	if err != nil {
		return ScheduledInstallPlan{}, err
	}
	plan := ScheduledInstallPlan{
		SchemaVersion: ScheduledInstallPlanSchemaVersion, Application: envelope.Application,
		Environment: envelope.Environment, Service: envelope.Service, Runner: envelope.Runner,
		EnvelopePath: envelopePath, EnvelopeDigest: envelope.EnvelopeDigest, PublicationProof: proof,
		Artifacts: []ScheduledInstallArtifact{
			{Class: "envelope", Path: envelopePath, Mode: 0o400, Owner: "onebox", Group: "onebox", Digest: digestScheduledBytes(envelopeBytes), Content: envelopeBytes},
			{Class: "runner", Path: wantRunnerPath, Mode: 0o500, Owner: "onebox", Group: "onebox", Digest: runnerDigest, Content: append([]byte(nil), runnerBytes...)},
		},
	}
	if err := plan.Seal(); err != nil {
		return ScheduledInstallPlan{}, err
	}
	return plan, nil
}

func ApplyScheduledInstall(ctx context.Context, target ScheduledArtifactTarget, plan ScheduledInstallPlan) error {
	if target == nil {
		return errors.New("scheduled artifact target is nil")
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	for _, artifact := range plan.Artifacts {
		if digestScheduledBytes(artifact.Content) != artifact.Digest {
			return fmt.Errorf("scheduled install artifact %q content changed after planning", artifact.Class)
		}
		if err := target.InstallAtomic(ctx, artifact); err != nil {
			return fmt.Errorf("install scheduled %s: %w", artifact.Class, err)
		}
		observed, err := target.Inspect(ctx, artifact.Path)
		if err != nil {
			return fmt.Errorf("inspect installed scheduled %s: %w", artifact.Class, err)
		}
		if observed.Path != artifact.Path || observed.Mode != artifact.Mode || observed.Owner != artifact.Owner || observed.Group != artifact.Group || observed.Digest != artifact.Digest {
			return fmt.Errorf("installed scheduled %s ownership, mode, or digest does not match the sealed plan", artifact.Class)
		}
	}
	return nil
}

func ScheduledArtifactsAsProtectionResources(plan ScheduledInstallPlan, referencedRunner bool) ([]ProtectionResource, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	resources := make([]ProtectionResource, 0, len(plan.Artifacts))
	for _, artifact := range plan.Artifacts {
		kind := ProtectionResourceEnvelope
		referenced := false
		if artifact.Class == "runner" {
			kind = ProtectionResourceRunner
			referenced = referencedRunner
		}
		resources = append(resources, ProtectionResource{
			Identity: artifact.Path, Kind: kind, OwnerApplication: plan.Application,
			OwnerEnvironment: plan.Environment, Service: plan.Service, Referenced: referenced,
		})
	}
	return resources, nil
}

func ApplyAuthorizedScheduledRemoval(ctx context.Context, target ScheduledArtifactTarget, plan ProtectionRemovalPlan, authorization ProtectionRemovalAuthorization) error {
	if target == nil {
		return errors.New("scheduled artifact target is nil")
	}
	return ApplyProtectionRemoval(plan, authorization, func(resource ProtectionResource) error {
		if resource.Kind != ProtectionResourceRunner && resource.Kind != ProtectionResourceEnvelope {
			return fmt.Errorf("resource %q is not a scheduled executable artifact", resource.Identity)
		}
		return target.Remove(ctx, resource.Identity)
	})
}

func (proof ScheduledPublicationProof) Validate() error {
	if !proof.Verified || !lifecycleGraphDigest.MatchString(proof.ArtifactDigest) || !lifecycleGraphDigest.MatchString(proof.SBOMDigest) ||
		!lifecycleGraphDigest.MatchString(proof.EvidenceDigest) || !safeLifecycleMetadata(proof.ProvenanceID) || !safeLifecycleMetadata(proof.Publisher) {
		return errors.New("scheduled runner publication proof is incomplete or unverified")
	}
	if proof.VerificationMethod != "sigstore-bundle" && proof.VerificationMethod != "transparency-log" {
		return errors.New("scheduled runner publication verification method is unsupported")
	}
	if _, err := time.Parse(time.RFC3339Nano, proof.VerifiedAt); err != nil {
		return errors.New("scheduled runner publication verification time is invalid")
	}
	return nil
}

func (plan *ScheduledInstallPlan) Seal() error {
	if plan == nil {
		return errors.New("scheduled install plan is nil")
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

func (plan ScheduledInstallPlan) Validate() error {
	if err := plan.validateContent(); err != nil {
		return err
	}
	expected, err := plan.computeDigest()
	if err != nil {
		return err
	}
	if plan.PlanDigest != expected {
		return errors.New("scheduled install plan digest mismatch")
	}
	return nil
}

func (plan ScheduledInstallPlan) validateContent() error {
	if plan.SchemaVersion != ScheduledInstallPlanSchemaVersion {
		return fmt.Errorf("unsupported scheduled install schema %q", plan.SchemaVersion)
	}
	for _, value := range []string{plan.Application, plan.Environment, plan.Service} {
		if !safeLifecycleMetadata(value) {
			return errors.New("scheduled install ownership metadata is invalid")
		}
	}
	if err := plan.PublicationProof.Validate(); err != nil {
		return err
	}
	if !lifecycleGraphDigest.MatchString(plan.EnvelopeDigest) || len(plan.Artifacts) != 2 {
		return errors.New("scheduled install plan has an invalid envelope binding or artifact set")
	}
	previous := ""
	classes := map[string]bool{}
	for _, artifact := range plan.Artifacts {
		if artifact.Class != "envelope" && artifact.Class != "runner" {
			return fmt.Errorf("unsupported scheduled install artifact class %q", artifact.Class)
		}
		if previous != "" && artifact.Class <= previous {
			return errors.New("scheduled install artifacts must be unique and sorted")
		}
		if artifact.Path == "" || !lifecycleGraphDigest.MatchString(artifact.Digest) || artifact.Owner != "onebox" || artifact.Group != "onebox" {
			return errors.New("scheduled install artifact binding is invalid")
		}
		if (artifact.Class == "runner" && artifact.Mode != 0o500) || (artifact.Class == "envelope" && artifact.Mode != 0o400) {
			return errors.New("scheduled install artifact mode is not least-privilege")
		}
		classes[artifact.Class] = true
		previous = artifact.Class
	}
	if !classes["runner"] || !classes["envelope"] || plan.Runner.Digest != plan.PublicationProof.ArtifactDigest {
		return errors.New("scheduled install runner or envelope is missing")
	}
	return nil
}

func (plan ScheduledInstallPlan) computeDigest() (string, error) {
	copy := plan
	copy.PlanDigest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	return digestScheduledBytes(encoded), nil
}

func digestScheduledBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
