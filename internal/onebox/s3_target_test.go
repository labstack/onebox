package onebox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

func testS3Target() app.BackupTarget {
	return app.BackupTarget{
		Kind: "s3-compatible", Endpoint: "https://objects.example.net", Bucket: "onebox-backups",
		Prefix: "production/shop", Region: "us-east-1", TLS: "required",
		FailureDomain: app.FailureDomain{Identity: "provider-a/us-east-1/account-42", Host: "objects.example.net"},
		Credentials: app.CredentialReference{
			File: "secrets/backup.env", Provider: "sops", AccessKeyEntry: "BACKUP_ACCESS_KEY_ID",
			SecretKeyEntry: "BACKUP_SECRET_ACCESS_KEY", SessionTokenEntry: "BACKUP_SESSION_TOKEN",
		},
		Encryption: app.TargetEncryption{PITR: "archive-password", Snapshot: "client-side"},
	}
}

func testS3CredentialFile(t *testing.T, mode os.FileMode) (string, string) {
	t.Helper()
	const secret = "storage-secret-canary"
	path := filepath.Join(t.TempDir(), "credentials.env")
	content := "BACKUP_ACCESS_KEY_ID=test\nBACKUP_SECRET_ACCESS_KEY=" + secret + "\nBACKUP_SESSION_TOKEN=session\n"
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path, secret
}

func successfulS3Observation() S3TargetProbeObservation {
	return S3TargetProbeObservation{
		EndpointHost: "objects.example.net", EndpointAddresses: []string{"198.51.100.24"},
		FailureDomainIdentity: "provider-a/us-east-1/account-42",
		Reachable:             true, Authorized: true, BucketPresent: true, TLSVerified: true, OffHost: true,
		EncryptionMode: "archive-password", EncryptionEvidenceID: "pgbackrest-repo-cipher-aes-256-cbc",
		ProbeEvidenceID: "s3-probe-20260807T120000Z",
	}
}

func lifecycleFailureCode(t *testing.T, err error) string {
	t.Helper()
	var failure LifecycleFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a lifecycle failure", err)
	}
	return failure.Code
}

func TestS3TargetAdapterProjectsClosedSecretFreeContractAndProbeEvidence(t *testing.T) {
	credentialFile, secret := testS3CredentialFile(t, 0o600)
	target, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, testS3Target())
	if err != nil {
		t.Fatal(err)
	}
	if target.Endpoint != "https://objects.example.net" || target.Bucket != "onebox-backups" || target.Prefix != "production/shop" ||
		target.Region != "us-east-1" || target.TLS != "required" || target.EncryptionMode != "archive-password" {
		t.Fatalf("target adapter = %#v", target)
	}

	probeCalls := 0
	evidence, err := target.Probe(context.Background(), ProtectedFailureDomain{
		Identity: "onebox-host/shop", Host: "app.example.net", Addresses: []string{"203.0.113.10"},
	}, S3TargetProbeFunc(func(_ context.Context, got S3TargetAdapter) (S3TargetProbeObservation, error) {
		probeCalls++
		if got.Credentials.File != credentialFile || got.Credentials.SecretKeyEntry != "BACKUP_SECRET_ACCESS_KEY" {
			t.Fatalf("probe target = %#v", got)
		}
		return successfulS3Observation(), nil
	}))
	if err != nil {
		t.Fatalf("probe S3 target: %v", err)
	}
	if probeCalls != 1 || !evidence.OffHost || evidence.CredentialFileMode != 0o600 || evidence.EncryptionEvidenceID == "" {
		t.Fatalf("probe evidence = %#v, calls = %d", evidence, probeCalls)
	}
	encoded, _ := json.Marshal(evidence)
	if strings.Contains(string(encoded), credentialFile) || strings.Contains(string(encoded), secret) {
		t.Fatalf("public target evidence leaked credential material: %s", encoded)
	}
}

func TestS3TargetAdapterRejectsInvalidAndUnprovenConfiguration(t *testing.T) {
	credentialFile, _ := testS3CredentialFile(t, 0o600)

	invalidRegion := testS3Target()
	invalidRegion.Region = "US East 1"
	if _, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, invalidRegion); err == nil {
		t.Fatal("invalid S3 region was accepted")
	}

	unproven := testS3Target()
	unproven.Encryption.PITR = ""
	if _, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, unproven); lifecycleFailureCode(t, err) != "backup_encryption_unverified" {
		t.Fatalf("unproven encryption error = %v", err)
	}

	if _, err := NewS3TargetAdapter("offsite", "pitr", "relative/credentials.env", testS3Target()); err == nil {
		t.Fatal("relative target-side credential path was accepted")
	}

	adapter, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, testS3Target())
	if err != nil {
		t.Fatal(err)
	}
	adapter.Credentials.SecretKeyEntry = "bad/name"
	if err := adapter.Validate(); err == nil {
		t.Fatal("mutated invalid credential entry was accepted")
	}

	wrongKind := testS3Target()
	wrongKind.Kind = "minio-replication"
	if _, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, wrongKind); err == nil {
		t.Fatal("removed replication target was accepted by S3 adapter")
	}
}

func TestS3TargetAdapterRejectsDeclaredSelfTargetBeforeProbe(t *testing.T) {
	credentialFile, _ := testS3CredentialFile(t, 0o600)
	target, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, testS3Target())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = target.Probe(context.Background(), ProtectedFailureDomain{
		Identity: target.FailureDomain.Identity, Host: "app.example.net",
	}, S3TargetProbeFunc(func(context.Context, S3TargetAdapter) (S3TargetProbeObservation, error) {
		called = true
		return successfulS3Observation(), nil
	}))
	if lifecycleFailureCode(t, err) != "backup_target_not_independent" || called {
		t.Fatalf("self-target error/called = %v/%v", err, called)
	}
}

func TestS3TargetAdapterRejectsResolvedHostAlias(t *testing.T) {
	credentialFile, _ := testS3CredentialFile(t, 0o600)
	target, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, testS3Target())
	if err != nil {
		t.Fatal(err)
	}
	observation := successfulS3Observation()
	observation.EndpointAddresses = []string{"203.0.113.10", "2001:db8::20"}
	_, err = target.Probe(context.Background(), ProtectedFailureDomain{
		Identity: "onebox-host/shop", Host: "app.example.net", Addresses: []string{"203.0.113.10"},
	}, S3TargetProbeFunc(func(context.Context, S3TargetAdapter) (S3TargetProbeObservation, error) {
		return observation, nil
	}))
	if lifecycleFailureCode(t, err) != "backup_target_not_independent" {
		t.Fatalf("alias error = %v", err)
	}
}

func TestS3TargetAdapterRequiresPrivateCredentialFile(t *testing.T) {
	credentialFile, _ := testS3CredentialFile(t, 0o644)
	target, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, testS3Target())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = target.Probe(context.Background(), ProtectedFailureDomain{Identity: "onebox-host/shop", Host: "app.example.net"},
		S3TargetProbeFunc(func(context.Context, S3TargetAdapter) (S3TargetProbeObservation, error) {
			called = true
			return successfulS3Observation(), nil
		}))
	if lifecycleFailureCode(t, err) != "backup_target_unauthorized" || called {
		t.Fatalf("credential mode error/called = %v/%v", err, called)
	}
}

func TestS3TargetAdapterRedactsProbeFailure(t *testing.T) {
	credentialFile, secret := testS3CredentialFile(t, 0o600)
	target, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, testS3Target())
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.Probe(context.Background(), ProtectedFailureDomain{Identity: "onebox-host/shop", Host: "app.example.net"},
		S3TargetProbeFunc(func(context.Context, S3TargetAdapter) (S3TargetProbeObservation, error) {
			return S3TargetProbeObservation{}, errors.New("provider response included " + secret)
		}))
	if lifecycleFailureCode(t, err) != "backup_target_unreachable" || strings.Contains(err.Error(), secret) {
		t.Fatalf("redacted probe error = %v", err)
	}
}

// localTestRepository is intentionally test-only. It exercises repository
// contracts without ever being mistaken for off-host protection.
type localTestRepository struct{}

func (localTestRepository) ProbeS3(context.Context, S3TargetAdapter) (S3TargetProbeObservation, error) {
	observation := successfulS3Observation()
	observation.OffHost = false
	observation.ProbeEvidenceID = "local-test-repository"
	return observation, nil
}

func TestLocalTestRepositoryNeverCountsAsOffHostProtection(t *testing.T) {
	credentialFile, _ := testS3CredentialFile(t, 0o600)
	target, err := NewS3TargetAdapter("offsite", "pitr", credentialFile, testS3Target())
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.Probe(context.Background(), ProtectedFailureDomain{Identity: "onebox-host/shop", Host: "app.example.net"}, localTestRepository{})
	if lifecycleFailureCode(t, err) != "backup_target_not_independent" {
		t.Fatalf("local repository error = %v", err)
	}
}
