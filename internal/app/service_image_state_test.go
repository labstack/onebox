package app

import (
	"errors"
	"strings"
	"testing"
)

func serviceImageTestResolved(withPolicy bool) *Resolved {
	service := Service{Driver: "postgres", Version: 17}
	if withPolicy {
		service.Backup = backupTestPolicy()
	}
	return &Resolved{
		Spec: &Spec{
			Name: "example", BasePath: "/var/lib/ob",
			Services:      map[string]Service{"database": service},
			BackupTargets: map[string]BackupTarget{"offsite": backupTestTarget()},
		},
		Env: "production",
	}
}

func backupTestPolicy() *BackupPolicy {
	return &BackupPolicy{
		Target: "offsite", RecoveryKind: "pitr", MaxDataLoss: "15m",
		Retention: BackupRetention{Keep: 7, Window: "7d"},
	}
}

func backupTestTarget() BackupTarget {
	return BackupTarget{
		Kind: "s3-compatible", Endpoint: "https://objects.example.net",
		Bucket: "onebox-backups", Prefix: "production/example", Region: "us-east-1", TLS: "verify",
		Credentials: CredentialReference{
			File: "secrets/backup.env", Provider: "sops",
			AccessKeyEntry: "BACKUP_ACCESS_KEY_ID", SecretKeyEntry: "BACKUP_SECRET_ACCESS_KEY",
		},
		Encryption: TargetEncryption{PITR: "client-side"},
	}
}

// backupTestProjection is what enablement records. A runtime state that
// says "enabled" without one describes a service that is archiving somewhere
// nothing can name, which is not a state enablement can produce.
func backupTestProjection() *BackupEffectiveProjection {
	return &BackupEffectiveProjection{Policy: *backupTestPolicy(), Target: backupTestTarget()}
}

func pinnedServiceImage(repository string, digit byte) string {
	return repository + "@sha256:" + strings.Repeat(string(digit), 64)
}

func renderServiceImage(t *testing.T, resolved *Resolved) string {
	t.Helper()
	documents, err := resolved.RenderServices("production")
	if err != nil {
		t.Fatalf("render services: %v", err)
	}
	body := string(documents["database"])
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "image:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "image:"))
		}
	}
	t.Fatalf("service document has no image: %s", body)
	return ""
}

func TestUnprotectedServiceKeepsTagRenderingOffline(t *testing.T) {
	resolved := serviceImageTestResolved(false)
	if got := renderServiceImage(t, resolved); got != "postgres:17" {
		t.Fatalf("unprotected service image = %q", got)
	}
	withState, err := resolved.WithServiceRuntimeStates(map[string]ServiceRuntimeState{
		"database": {BackupState: "never-enabled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := renderServiceImage(t, withState); got != "postgres:17" {
		t.Fatalf("never-enabled service image = %q", got)
	}
}

func TestProtectedServiceRenderingUsesDurableDigestNotPolicyOrMovedTag(t *testing.T) {
	image := pinnedServiceImage("postgres", 'a')
	state := ServiceRuntimeState{
		BackupState: "enabled", ServiceImage: image, LastEffective: backupTestProjection(),
		PublicationVerified: true, DigestAvailable: true,
		TagObservedDigest: "sha256:" + strings.Repeat("b", 64),
	}
	for _, withPolicy := range []bool{true, false} {
		resolved, err := serviceImageTestResolved(withPolicy).WithServiceRuntimeStates(map[string]ServiceRuntimeState{"database": state})
		if err != nil {
			t.Fatal(err)
		}
		if got := renderServiceImage(t, resolved); got != image {
			t.Fatalf("protected image with policy=%v = %q, want recorded %q", withPolicy, got, image)
		}
	}
}

func TestProtectedServiceImageFailureCodes(t *testing.T) {
	image := pinnedServiceImage("postgres", 'a')
	tests := []struct {
		name  string
		state ServiceRuntimeState
		code  string
	}{
		{"unsafe-revert", ServiceRuntimeState{BackupState: "enabled", PublicationVerified: true, DigestAvailable: true}, "backup_image_revert_unsafe"},
		{"unpublished", ServiceRuntimeState{BackupState: "enabled", ServiceImage: image, DigestAvailable: true}, "backup_service_image_unpublished"},
		{"unavailable", ServiceRuntimeState{BackupState: "enabled", ServiceImage: image, PublicationVerified: true}, "service_image_digest_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := serviceImageTestResolved(true).WithServiceRuntimeStates(map[string]ServiceRuntimeState{"database": test.state})
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolved.RenderServices("production")
			var projectError *Error
			if !errors.As(err, &projectError) || projectError.Code != test.code {
				t.Fatalf("render error = %v, want code %s", err, test.code)
			}
		})
	}
}

func TestProtectedServicePermitsExactVerifiedCacheDuringRegistryOutage(t *testing.T) {
	image := pinnedServiceImage("postgres", 'a')
	resolved, err := serviceImageTestResolved(true).WithServiceRuntimeStates(map[string]ServiceRuntimeState{
		"database": {BackupState: "enabled", ServiceImage: image, PublicationVerified: true, CacheVerified: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := renderServiceImage(t, resolved); got != image {
		t.Fatalf("cached protected service image = %q", got)
	}
}

func TestProtectedRefreshRetainsCurrentAndManifestImageRoots(t *testing.T) {
	current := pinnedServiceImage("postgres", 'a')
	manifest := pinnedServiceImage("postgres", 'b')
	candidate := pinnedServiceImage("postgres", 'c')
	resolved, err := serviceImageTestResolved(true).WithServiceRuntimeStates(map[string]ServiceRuntimeState{
		"database": {
			BackupState: "enabled", ServiceImage: current, PublicationVerified: true, DigestAvailable: true,
			ManifestRootImages: []string{manifest},
			RefreshCandidate:   &ServiceImageCandidate{Image: candidate, PublicationVerified: true, DigestAvailable: true, ExactTransition: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := resolved.ServiceImageForRuntime("database")
	if err != nil {
		t.Fatal(err)
	}
	if selection.Image != candidate || strings.Join(selection.RetainedImages, ",") != strings.Join([]string{current, manifest, candidate}, ",") {
		t.Fatalf("protected refresh selection = %#v", selection)
	}
	if got := renderServiceImage(t, resolved); got != candidate {
		t.Fatalf("protected refresh rendered image = %q", got)
	}
}

func TestProtectedRefreshRefusesDisablePendingAndUnqualifiedTransition(t *testing.T) {
	image := pinnedServiceImage("postgres", 'a')
	candidate := &ServiceImageCandidate{Image: pinnedServiceImage("postgres", 'b'), PublicationVerified: true, DigestAvailable: true, ExactTransition: true}
	for _, test := range []struct {
		state string
		exact bool
		code  string
	}{
		{"disable-pending", true, "service_image_patch_disable_pending"},
		{"enabled", false, "service_patch_unsupported"},
	} {
		copy := *candidate
		copy.ExactTransition = test.exact
		resolved, err := serviceImageTestResolved(true).WithServiceRuntimeStates(map[string]ServiceRuntimeState{
			"database": {BackupState: test.state, ServiceImage: image, PublicationVerified: true, DigestAvailable: true, RefreshCandidate: &copy},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolved.RenderServices("production")
		var projectError *Error
		if !errors.As(err, &projectError) || projectError.Code != test.code {
			t.Fatalf("state %s error = %v, want %s", test.state, err, test.code)
		}
	}
}

func TestProtectedServiceGoldenUsesImmutableImage(t *testing.T) {
	image := pinnedServiceImage("postgres", 'd')
	resolved, err := serviceImageTestResolved(true).WithServiceRuntimeStates(map[string]ServiceRuntimeState{
		"database": {BackupState: "enabled", ServiceImage: image, PublicationVerified: true, DigestAvailable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	documents, err := resolved.RenderServices("production")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(documents["database"]), "image: "+image+"\n") {
		t.Fatalf("protected service golden does not contain immutable image:\n%s", documents["database"])
	}
}
