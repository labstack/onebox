package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"sort"
)

type ProtectionEffectiveProjection struct {
	Policy ProtectionPolicy `json:"policy"`
	Target BackupTarget     `json:"target"`
}

type GeneratedProtectionArtifact struct {
	Class   string `json:"class"`
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Digest  string `json:"digest"`
	Content []byte `json:"-"`
}

type ProtectionArtifactSet struct {
	Service   string                        `json:"service"`
	Driver    string                        `json:"driver"`
	Source    string                        `json:"source"`
	Artifacts []GeneratedProtectionArtifact `json:"artifacts"`
}

type ProtectionArtifactDrift struct {
	Class          string `json:"class"`
	ExpectedDigest string `json:"expected_digest"`
	ObservedDigest string `json:"observed_digest,omitempty"`
}

func (resolved *Resolved) GenerateProtectionArtifacts(serviceName string) (ProtectionArtifactSet, error) {
	if resolved == nil || resolved.Spec == nil {
		return ProtectionArtifactSet{}, errors.New("resolved project is nil")
	}
	service, ok := resolved.Services[serviceName]
	if !ok {
		return ProtectionArtifactSet{}, errf("project_invalid", "services."+serviceName, "ob validate", "service is not declared")
	}
	driverName := service.Driver
	if driverName == "" {
		driverName = serviceName
	}
	capability, ok := lifecycleCapabilityFor(driverName)
	if !ok || !capability.ProtectionQualified(resolved.DeclaredVersion(serviceName)) {
		return ProtectionArtifactSet{}, errf("backup_driver_unsupported", "services."+serviceName+".protection", "ob validate", "service has no qualified protection artifact contract")
	}
	record := capability.Record()
	projection, source, err := resolved.effectiveProtectionProjection(serviceName, service)
	if err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := validateProtectionPolicy(projection.Policy, "services."+serviceName+".protection"); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := validateBackupTarget(projection.Target, "backup_targets."+projection.Policy.Target); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if !capability.SupportsRecoveryKind(resolved.DeclaredVersion(serviceName), projection.Policy.RecoveryKind) {
		return ProtectionArtifactSet{}, errf("backup_driver_unsupported", "services."+serviceName+".protection.recovery_kind", "ob validate", "driver does not support the retained recovery kind")
	}
	names := resolved.NamesFor(resolved.Env)
	base := path.Join(names.AppDir(), "protection", "artifacts", serviceName)
	credentialPath := names.ProtectionCredentialFile(serviceName, projection.Policy.Target)
	credentialEntries := []string{projection.Target.Credentials.AccessKeyEntry, projection.Target.Credentials.SecretKeyEntry}
	if projection.Target.Credentials.SessionTokenEntry != "" {
		credentialEntries = append(credentialEntries, projection.Target.Credentials.SessionTokenEntry)
	}
	inputs := map[string]any{
		"service": serviceName, "driver": driverName, "recovery_kind": projection.Policy.RecoveryKind,
		"maximum_data_loss": projection.Policy.MaximumDataLoss, "target": projection.Policy.Target,
		"endpoint": projection.Target.Endpoint, "bucket": projection.Target.Bucket, "prefix": projection.Target.Prefix,
		"region": projection.Target.Region, "tls": projection.Target.TLS, "failure_domain": projection.Target.FailureDomain,
		"credential_file":    credentialPath,
		"credential_entries": credentialEntries,
	}
	artifacts := make([]GeneratedProtectionArtifact, 0, 12)
	appendArtifact := func(class string, mode uint32, value any) error {
		content, err := json.Marshal(value)
		if err != nil {
			return err
		}
		content = append(content, '\n')
		sum := sha256.Sum256(content)
		artifacts = append(artifacts, GeneratedProtectionArtifact{
			Class: class, Path: path.Join(base, class+".json"), Mode: mode,
			Digest: "sha256:" + hex.EncodeToString(sum[:]), Content: content,
		})
		return nil
	}
	if err := appendArtifact("inputs", 0o600, inputs); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := appendArtifact("backup-schedule", 0o644, projection.Policy.Schedule); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := appendArtifact("drill-schedule", 0o644, projection.Policy.RestoreDrill.Schedule); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := appendArtifact("enablement", 0o600, map[string]any{
		"operation": "protection_enable", "preconditions": record.preconditions,
	}); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := appendArtifact("disablement", 0o600, map[string]any{"operation": "protection_disable", "service": serviceName}); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if requiresReplayArtifact(projection.Policy.RecoveryKind) {
		if err := appendArtifact("archive-hook", 0o600, map[string]any{
			"operation": "replay_archive", "service": serviceName, "credential_file": credentialPath,
		}); err != nil {
			return ProtectionArtifactSet{}, err
		}
	}
	if err := appendArtifact("service-image", 0o644, record.serviceArtifact); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if record.helperArtifact != nil {
		if err := appendArtifact("helper", 0o644, record.helperArtifact); err != nil {
			return ProtectionArtifactSet{}, err
		}
	}
	if err := appendArtifact("lifecycle-state", 0o600, map[string]any{
		"path": names.ActiveVolumeFile(serviceName), "credential_file": credentialPath,
	}); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := appendArtifact("retention", 0o644, map[string]any{
		"minimum_generations": projection.Policy.Retention.MinimumGenerations,
		"recovery_window":     projection.Policy.Retention.RecoveryWindow, "native_mapping": record.retentionMapping,
	}); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := appendArtifact("restore-template", 0o600, map[string]any{
		"service": serviceName, "operation": record.operations.Restore, "verify": record.operations.Verify,
		"staging_filesystem": projection.Policy.RestoreDrill.StagingFilesystem,
	}); err != nil {
		return ProtectionArtifactSet{}, err
	}
	if err := appendArtifact("provenance-sbom", 0o644, map[string]any{
		"service": record.serviceArtifact, "helper": record.helperArtifact,
	}); err != nil {
		return ProtectionArtifactSet{}, err
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Class < artifacts[j].Class })
	return ProtectionArtifactSet{Service: serviceName, Driver: driverName, Source: source, Artifacts: artifacts}, nil
}

func requiresReplayArtifact(recoveryKind string) bool {
	return recoveryKind == "pitr"
}

func (resolved *Resolved) effectiveProtectionProjection(serviceName string, service Service) (ProtectionEffectiveProjection, string, error) {
	if state, ok := resolved.serviceRuntime[serviceName]; ok && state.ProtectionState == "disable-pending" {
		if state.LastEffective == nil {
			return ProtectionEffectiveProjection{}, "", errf("protection_image_revert_unsafe", "services."+serviceName, "ob protection disable --output ndjson", "disable-pending state has no durable last-effective protection projection")
		}
		return *state.LastEffective, "last-effective", nil
	}
	if service.Protection == nil {
		return ProtectionEffectiveProjection{}, "", errf("project_invalid", "services."+serviceName+".protection", "ob validate", "service has no protection intent or retained projection")
	}
	target, ok := resolved.BackupTargets[service.Protection.Target]
	if !ok {
		return ProtectionEffectiveProjection{}, "", errf("backup_target_unknown", "services."+serviceName+".protection.target", "ob validate", "protection target is not declared")
	}
	return ProtectionEffectiveProjection{Policy: *service.Protection, Target: target}, "project-intent", nil
}

func CompareProtectionArtifacts(desired ProtectionArtifactSet, observed map[string]string) []ProtectionArtifactDrift {
	drift := make([]ProtectionArtifactDrift, 0)
	for _, artifact := range desired.Artifacts {
		if observed[artifact.Class] != artifact.Digest {
			drift = append(drift, ProtectionArtifactDrift{
				Class: artifact.Class, ExpectedDigest: artifact.Digest, ObservedDigest: observed[artifact.Class],
			})
		}
	}
	return drift
}
