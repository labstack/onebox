package onebox

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

const (
	S3TargetAdapterSchemaVersion = "onebox.run/s3-target/v1alpha1"
	S3TargetProbeSchemaVersion   = "onebox.run/s3-target-probe/v1alpha1"
)

// S3CredentialBinding identifies entries in a target-local mode-0600 file.
// It intentionally cannot carry credential values.
type S3CredentialBinding struct {
	File              string `json:"file"`
	AccessKeyEntry    string `json:"access_key_entry"`
	SecretKeyEntry    string `json:"secret_key_entry"`
	SessionTokenEntry string `json:"session_token_entry,omitempty"`
}

// S3TargetAdapter is the closed, secret-free input shared by native drivers
// that use an S3-compatible destination. Drivers still own consistency and
// repository semantics; this type owns destination identity and evidence.
type S3TargetAdapter struct {
	SchemaVersion  string              `json:"schema_version"`
	Name           string              `json:"name"`
	Endpoint       string              `json:"endpoint"`
	Bucket         string              `json:"bucket"`
	Prefix         string              `json:"prefix,omitempty"`
	Region         string              `json:"region,omitempty"`
	TLS            string              `json:"tls"`
	RecoveryKind   string              `json:"recovery_kind"`
	EncryptionMode string              `json:"encryption_mode"`
	FailureDomain  app.FailureDomain   `json:"failure_domain"`
	Credentials    S3CredentialBinding `json:"credentials"`
}

type ProtectedFailureDomain struct {
	Identity  string
	Host      string
	Addresses []string
}

// S3TargetProbeObservation is returned by the native target-side probe. It is
// evidence only: the adapter validates it before publishing a passing result.
type S3TargetProbeObservation struct {
	EndpointHost          string
	EndpointAddresses     []string
	FailureDomainIdentity string
	Reachable             bool
	Authorized            bool
	BucketPresent         bool
	TLSVerified           bool
	OffHost               bool
	EncryptionMode        string
	EncryptionEvidenceID  string
	ProbeEvidenceID       string
}

type S3TargetProber interface {
	ProbeS3(context.Context, S3TargetAdapter) (S3TargetProbeObservation, error)
}

type S3TargetProbeFunc func(context.Context, S3TargetAdapter) (S3TargetProbeObservation, error)

func (probe S3TargetProbeFunc) ProbeS3(ctx context.Context, target S3TargetAdapter) (S3TargetProbeObservation, error) {
	return probe(ctx, target)
}

// S3TargetProbeEvidence is safe for plans, journals, status, and model-visible
// output. Credential paths and values are deliberately absent.
type S3TargetProbeEvidence struct {
	SchemaVersion         string   `json:"schema_version"`
	Target                string   `json:"target"`
	EndpointHost          string   `json:"endpoint_host"`
	EndpointAddresses     []string `json:"endpoint_addresses"`
	Bucket                string   `json:"bucket"`
	Prefix                string   `json:"prefix,omitempty"`
	Region                string   `json:"region,omitempty"`
	TLS                   string   `json:"tls"`
	FailureDomainIdentity string   `json:"failure_domain_identity"`
	OffHost               bool     `json:"off_host"`
	CredentialFileMode    uint32   `json:"credential_file_mode"`
	EncryptionMode        string   `json:"encryption_mode"`
	EncryptionEvidenceID  string   `json:"encryption_evidence_id"`
	ProbeEvidenceID       string   `json:"probe_evidence_id"`
}

type S3CredentialFileEvidence struct {
	Mode    uint32
	Regular bool
	Symlink bool
}

func NewS3TargetAdapter(name, recoveryKind, credentialFile string, target app.BackupTarget) (S3TargetAdapter, error) {
	if err := app.ValidateBackupTarget(name, target); err != nil {
		return S3TargetAdapter{}, err
	}
	if target.Kind != "s3-compatible" {
		return S3TargetAdapter{}, errors.New("S3 target adapter requires an s3-compatible target")
	}
	mode := app.BackupTargetEncryptionMode(target, recoveryKind)
	if !oneOf(mode, "client-side", "archive-password", "server-side-sse") {
		return S3TargetAdapter{}, lifecycleFailure("backup_encryption_unverified")
	}
	adapter := S3TargetAdapter{
		SchemaVersion:  S3TargetAdapterSchemaVersion,
		Name:           name,
		Endpoint:       target.Endpoint,
		Bucket:         target.Bucket,
		Prefix:         target.Prefix,
		Region:         target.Region,
		TLS:            target.TLS,
		RecoveryKind:   recoveryKind,
		EncryptionMode: mode,
		FailureDomain:  target.FailureDomain,
		Credentials: S3CredentialBinding{
			File: credentialFile, AccessKeyEntry: target.Credentials.AccessKeyEntry,
			SecretKeyEntry: target.Credentials.SecretKeyEntry, SessionTokenEntry: target.Credentials.SessionTokenEntry,
		},
	}
	if err := adapter.Validate(); err != nil {
		return S3TargetAdapter{}, err
	}
	return adapter, nil
}

func (target S3TargetAdapter) Validate() error {
	if target.SchemaVersion != S3TargetAdapterSchemaVersion {
		return fmt.Errorf("unsupported S3 target adapter schema %q", target.SchemaVersion)
	}
	if target.RecoveryKind != "snapshot" && target.RecoveryKind != "pitr" && target.RecoveryKind != "cold" {
		return errors.New("S3 target recovery kind must be snapshot, pitr, or cold")
	}
	if !oneOf(target.EncryptionMode, "client-side", "archive-password", "server-side-sse") {
		return lifecycleFailure("backup_encryption_unverified")
	}
	encryption := app.TargetEncryption{}
	switch target.RecoveryKind {
	case "snapshot":
		encryption.Snapshot = target.EncryptionMode
	case "pitr":
		encryption.PITR = target.EncryptionMode
	case "cold":
		encryption.Cold = target.EncryptionMode
	}
	declared := app.BackupTarget{
		Kind: "s3-compatible", Endpoint: target.Endpoint, Bucket: target.Bucket,
		Prefix: target.Prefix, Region: target.Region, TLS: target.TLS,
		FailureDomain: target.FailureDomain, Encryption: encryption,
		Credentials: app.CredentialReference{
			File: "bound/credentials.env", Provider: "sops",
			AccessKeyEntry:    target.Credentials.AccessKeyEntry,
			SecretKeyEntry:    target.Credentials.SecretKeyEntry,
			SessionTokenEntry: target.Credentials.SessionTokenEntry,
		},
	}
	if err := app.ValidateBackupTarget(target.Name, declared); err != nil {
		return err
	}
	if !filepath.IsAbs(target.Credentials.File) || filepath.Clean(target.Credentials.File) != target.Credentials.File {
		return errors.New("S3 credential file must be a clean absolute target path")
	}
	entries := []string{target.Credentials.AccessKeyEntry, target.Credentials.SecretKeyEntry}
	if target.Credentials.SessionTokenEntry != "" {
		entries = append(entries, target.Credentials.SessionTokenEntry)
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, exists := seen[entry]; exists {
			return errors.New("S3 credential entries must be distinct")
		}
		seen[entry] = struct{}{}
	}
	return nil
}

// InspectS3CredentialFile verifies the target-local file without reading its
// contents. The native probe opens only the named entries after this check.
func InspectS3CredentialFile(path string) (S3CredentialFileEvidence, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return S3CredentialFileEvidence{}, errors.New("inspect S3 credential file: path is not a clean absolute target path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return S3CredentialFileEvidence{}, errors.New("inspect S3 credential file")
	}
	return S3CredentialFileEvidence{
		Mode: uint32(info.Mode().Perm()), Regular: info.Mode().IsRegular(), Symlink: info.Mode()&os.ModeSymlink != 0,
	}, nil
}

func (target S3TargetAdapter) Probe(ctx context.Context, protected ProtectedFailureDomain, prober S3TargetProber) (S3TargetProbeEvidence, error) {
	if err := target.Validate(); err != nil {
		return S3TargetProbeEvidence{}, err
	}
	if prober == nil {
		return S3TargetProbeEvidence{}, errors.New("S3 target probe is unavailable")
	}
	if target.declaredSelfTarget(protected) {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_not_independent")
	}
	credential, err := InspectS3CredentialFile(target.Credentials.File)
	if err != nil || credential.Mode != 0o600 || !credential.Regular || credential.Symlink {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_unauthorized")
	}
	observation, err := prober.ProbeS3(ctx, target)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return S3TargetProbeEvidence{}, err
		}
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_unreachable")
	}
	evidence, err := target.validateObservation(protected, credential, observation)
	if err != nil {
		return S3TargetProbeEvidence{}, err
	}
	return evidence, nil
}

func (target S3TargetAdapter) declaredSelfTarget(protected ProtectedFailureDomain) bool {
	endpoint, _ := url.Parse(target.Endpoint)
	return sameProbeHost(target.FailureDomain.Identity, protected.Identity) ||
		sameProbeHost(target.FailureDomain.Host, protected.Host) ||
		sameProbeHost(endpoint.Hostname(), protected.Host)
}

func (target S3TargetAdapter) validateObservation(
	protected ProtectedFailureDomain,
	credential S3CredentialFileEvidence,
	observation S3TargetProbeObservation,
) (S3TargetProbeEvidence, error) {
	endpoint, _ := url.Parse(target.Endpoint)
	if !sameProbeHost(observation.EndpointHost, endpoint.Hostname()) {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_not_independent")
	}
	addresses, parsedAddresses, err := canonicalProbeAddresses(observation.EndpointAddresses)
	if err != nil || len(addresses) == 0 {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_not_independent")
	}
	_, protectedAddresses, err := canonicalProbeAddresses(protected.Addresses)
	if err != nil || addressesOverlap(parsedAddresses, protectedAddresses) ||
		observation.FailureDomainIdentity != target.FailureDomain.Identity || !observation.OffHost {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_not_independent")
	}
	if !observation.Reachable || !observation.BucketPresent {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_unreachable")
	}
	if !observation.Authorized {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_unauthorized")
	}
	if target.TLS == "required" && !observation.TLSVerified {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_target_unreachable")
	}
	if observation.EncryptionMode != target.EncryptionMode || !safeLifecycleMetadata(observation.EncryptionEvidenceID) {
		return S3TargetProbeEvidence{}, lifecycleFailure("backup_encryption_unverified")
	}
	if !safeLifecycleMetadata(observation.ProbeEvidenceID) {
		return S3TargetProbeEvidence{}, errors.New("S3 target probe returned an invalid evidence identity")
	}
	return S3TargetProbeEvidence{
		SchemaVersion: S3TargetProbeSchemaVersion, Target: target.Name,
		EndpointHost: endpoint.Hostname(), EndpointAddresses: addresses,
		Bucket: target.Bucket, Prefix: target.Prefix, Region: target.Region, TLS: target.TLS,
		FailureDomainIdentity: target.FailureDomain.Identity, OffHost: true, CredentialFileMode: credential.Mode,
		EncryptionMode: target.EncryptionMode, EncryptionEvidenceID: observation.EncryptionEvidenceID,
		ProbeEvidenceID: observation.ProbeEvidenceID,
	}, nil
}

func canonicalProbeAddresses(values []string) ([]string, map[netip.Addr]struct{}, error) {
	addresses := make(map[netip.Addr]struct{}, len(values))
	for _, value := range values {
		address, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			return nil, nil, err
		}
		addresses[address.Unmap()] = struct{}{}
	}
	encoded := make([]string, 0, len(addresses))
	for address := range addresses {
		encoded = append(encoded, address.String())
	}
	sort.Strings(encoded)
	return encoded, addresses, nil
}

func addressesOverlap(left, right map[netip.Addr]struct{}) bool {
	for address := range left {
		if _, exists := right[address]; exists {
			return true
		}
	}
	return false
}

func sameProbeHost(left, right string) bool {
	left = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(left)), ".")
	right = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(right)), ".")
	return left != "" && left == right
}

func lifecycleFailure(code string) error {
	failure, err := NewLifecycleFailure(code)
	if err != nil {
		return err
	}
	return failure
}
