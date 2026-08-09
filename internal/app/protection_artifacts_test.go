package app

import (
	"bytes"
	"strings"
	"testing"
)

func protectionArtifactFixture() (*Resolved, ProtectionPolicy, BackupTarget) {
	policy := ProtectionPolicy{
		Target: "offsite", RecoveryKind: "pitr", MaximumDataLoss: "5m",
		Schedule:  Schedule{Cron: "17 */6 * * *", Timezone: "UTC"},
		Retention: ProtectionRetention{MinimumGenerations: 7, RecoveryWindow: "7d"},
		RestoreDrill: RestoreDrillPolicy{
			Schedule:        Schedule{Cron: "23 4 * * 1,4", Timezone: "UTC"},
			ProofMaximumAge: "7d", StagingFilesystem: "/srv/onebox-restore",
		},
	}
	target := BackupTarget{
		Kind: "s3-compatible", Endpoint: "https://objects.example.test", Bucket: "onebox-backups",
		Prefix: "production/example", Region: "us-east-1", TLS: "required",
		FailureDomain: FailureDomain{Identity: "provider-a/us-east-1/account-42"},
		Credentials: CredentialReference{
			File: "secrets/backup.env", Provider: "sops", AccessKeyEntry: "BACKUP_ACCESS_KEY_ID",
			SecretKeyEntry: "BACKUP_SECRET_ACCESS_KEY", SessionTokenEntry: "BACKUP_SESSION_TOKEN",
		},
		Encryption: TargetEncryption{PITR: "archive-password"},
	}
	resolved := &Resolved{
		Spec: &Spec{
			Name: "example", BasePath: "/var/lib/onebox",
			Services:      map[string]Service{"database": {Driver: "postgres", Version: 17, Protection: &policy}},
			BackupTargets: map[string]BackupTarget{"offsite": target},
		},
		Env: "production",
	}
	return resolved, policy, target
}

func TestGenerateProtectionArtifactsIsDeterministicRedactedAndComplete(t *testing.T) {
	resolved, _, _ := protectionArtifactFixture()
	first, err := resolved.GenerateProtectionArtifacts("database")
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolved.GenerateProtectionArtifacts("database")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Artifacts) != 11 || len(second.Artifacts) != len(first.Artifacts) {
		t.Fatalf("artifact counts = %d and %d, want 11", len(first.Artifacts), len(second.Artifacts))
	}
	wantClasses := []string{
		"archive-hook", "backup-schedule", "disablement", "drill-schedule", "enablement", "inputs",
		"lifecycle-state", "provenance-sbom", "restore-template", "retention", "service-image",
	}
	seen := make(map[string]bool, len(first.Artifacts))
	for index, artifact := range first.Artifacts {
		seen[artifact.Class] = true
		if artifact.Digest != second.Artifacts[index].Digest || !bytes.Equal(artifact.Content, second.Artifacts[index].Content) {
			t.Fatalf("artifact %q is not deterministic", artifact.Class)
		}
		if !strings.HasPrefix(artifact.Path, "/var/lib/onebox/") || !lifecycleDigest.MatchString(artifact.Digest) {
			t.Fatalf("artifact metadata is not sealed: %#v", artifact)
		}
		for _, forbidden := range []string{"secrets/backup.env", "database-content-canary", "secret-value-canary"} {
			if bytes.Contains(artifact.Content, []byte(forbidden)) {
				t.Fatalf("artifact %q leaked %q: %s", artifact.Class, forbidden, artifact.Content)
			}
		}
	}
	for _, class := range wantClasses {
		if !seen[class] {
			t.Errorf("missing artifact class %q", class)
		}
	}
	if seen["helper"] {
		t.Fatal("derived-image PostgreSQL unexpectedly received an external helper artifact")
	}
	if got := string(artifactContent(t, first, "backup-schedule")); !strings.Contains(got, `"cron":"17 */6 * * *"`) {
		t.Fatalf("backup schedule is not exact: %s", got)
	}
	if got := string(artifactContent(t, first, "drill-schedule")); !strings.Contains(got, `"cron":"23 4 * * 1,4"`) {
		t.Fatalf("drill schedule is not exact: %s", got)
	}
}

func TestProtectionArtifactsRetainLastEffectiveProjectionAndDetectRealDrift(t *testing.T) {
	resolved, policy, target := protectionArtifactFixture()
	original, err := resolved.GenerateProtectionArtifacts("database")
	if err != nil {
		t.Fatal(err)
	}
	withoutIntent := *resolved
	withoutIntent.Spec = &Spec{Name: "example", BasePath: "/var/lib/onebox", Services: map[string]Service{
		"database": {Driver: "postgres", Version: 17},
	}}
	retained, err := withoutIntent.WithServiceRuntimeStates(map[string]ServiceRuntimeState{
		"database": {
			ProtectionState: "disable-pending", ServiceImage: pinnedServiceImage("postgres", '1'),
			PublicationVerified: true, DigestAvailable: true,
			LastEffective: &ProtectionEffectiveProjection{Policy: policy, Target: target},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	desired, err := retained.GenerateProtectionArtifacts("database")
	if err != nil {
		t.Fatal(err)
	}
	if desired.Source != "last-effective" {
		t.Fatalf("artifact source = %q, want last-effective", desired.Source)
	}
	observed := make(map[string]string, len(original.Artifacts))
	for index, artifact := range original.Artifacts {
		if desired.Artifacts[index].Class != artifact.Class || desired.Artifacts[index].Digest != artifact.Digest {
			t.Fatalf("retained artifact %d differs: got %#v want %#v", index, desired.Artifacts[index], artifact)
		}
		observed[artifact.Class] = artifact.Digest
	}
	if drift := CompareProtectionArtifacts(desired, observed); len(drift) != 0 {
		t.Fatalf("matching retained artifacts reported drift: %#v", drift)
	}
	for _, artifact := range desired.Artifacts {
		changed := make(map[string]string, len(observed))
		for class, digest := range observed {
			changed[class] = digest
		}
		changed[artifact.Class] = "sha256:" + strings.Repeat("f", 64)
		if changed[artifact.Class] == artifact.Digest {
			changed[artifact.Class] = "sha256:" + strings.Repeat("e", 64)
		}
		drift := CompareProtectionArtifacts(desired, changed)
		if len(drift) != 1 || drift[0].Class != artifact.Class {
			t.Fatalf("real drift for %q = %#v", artifact.Class, drift)
		}
	}
}

func TestSnapshotOnlyDriversReceiveNoReplayArtifact(t *testing.T) {
	for name, capability := range lifecycleCapabilities {
		record := capability.Record()
		if record.recoveryKinds["snapshot"] && requiresReplayArtifact("snapshot") {
			t.Fatalf("snapshot-only driver %q would receive a replay artifact", name)
		}
	}
}

func artifactContent(t *testing.T, set ProtectionArtifactSet, class string) []byte {
	t.Helper()
	for _, artifact := range set.Artifacts {
		if artifact.Class == class {
			return artifact.Content
		}
	}
	t.Fatalf("artifact class %q not found", class)
	return nil
}
