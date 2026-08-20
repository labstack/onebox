package onebox

import (
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
)

func protectionStateProjection() app.ProtectionEffectiveProjection {
	return app.ProtectionEffectiveProjection{
		Policy: app.BackupPolicy{
			Target: "offsite", RecoveryKind: "pitr", MaximumDataLoss: "5m",
			Schedule:  app.Schedule{Cron: "17 */6 * * *", Timezone: "UTC"},
			Retention: app.BackupRetention{MinimumGenerations: 7, RecoveryWindow: "7d"},
			RestoreDrill: app.RestoreDrill{
				Schedule: app.Schedule{Cron: "23 4 * * 1,4", Timezone: "UTC"}, ProofMaximumAge: "7d",
			},
		},
		Target: app.BackupTarget{
			Kind: "s3-compatible", Endpoint: "https://objects.example.test", Bucket: "onebox-backups",
			TLS: "verify", FailureDomain: app.FailureDomain{Identity: "provider-a/us-east-1/account-42"},
			Credentials: app.CredentialReference{
				File: "secrets/backup.env", Provider: "sops", AccessKeyEntry: "BACKUP_ACCESS_KEY_ID", SecretKeyEntry: "BACKUP_SECRET_ACCESS_KEY",
			},
			Encryption: app.TargetEncryption{PITR: "client-side"},
		},
	}
}

// pendingProtectionState returns a service part-way through disablement: the
// decision recorded, the work not yet done.
func pendingProtectionState(t *testing.T) ProtectionLifecycleState {
	t.Helper()
	state, err := NewProtectionLifecycleState("example", "production", "database", 1)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := EnableProtection(state, protectionStateProjection(), "postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "op-1", true, 2)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := BeginProtectionDisable(enabled, "op-2", time.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

// Disablement is two states, not one write. The pending state exists because
// stopping archiving, restarting the service and removing credentials all take
// time and can fail: a single write to "disabled" would claim the work was done
// before it was, and a failure halfway would leave a record saying the service
// is not archiving while it still is.
func TestProtectionDisableRecordsIntentBeforeDoingTheWork(t *testing.T) {
	pending := pendingProtectionState(t)
	if pending.State != ProtectionDisablePending {
		t.Fatalf("state = %q, want disable-pending", pending.State)
	}
	// Still archiving, still holding its runtime — rendering must keep producing
	// the protected server until the work actually completes.
	if !pending.PrerequisiteEffective || !pending.LocalSupportInstalled || pending.LastEffective == nil {
		t.Fatalf("pending state stopped describing a protected service: %#v", pending)
	}
	if runtime := pending.RuntimeState(); runtime.ProtectionState != string(ProtectionDisablePending) {
		t.Fatalf("runtime state = %q, want the pending state rendering keys on", runtime.ProtectionState)
	}

	done, err := DisableProtection(pending, "op-2", pending.Epoch+1)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != ProtectionDisabled || done.PrerequisiteEffective || done.LocalSupportInstalled {
		t.Fatalf("completed disablement = %#v", done)
	}
	if len(done.Schedules) != 0 {
		t.Fatal("a disabled service kept a schedule, which would keep pushing to a repository the project no longer describes")
	}
	// The record of what it was protected by survives, so the repository can
	// still be named after the fact.
	if done.LastEffective == nil {
		t.Fatal("disablement discarded the projection it was protected by")
	}
}

// Re-running disablement is how an operator recovers a run that failed halfway.
func TestProtectionDisableIsResumable(t *testing.T) {
	pending := pendingProtectionState(t)
	again, err := BeginProtectionDisable(pending, "op-2", time.Now(), pending.Epoch+1)
	if err != nil {
		t.Fatalf("resuming a pending disablement: %v", err)
	}
	if again.State != ProtectionDisablePending {
		t.Fatalf("resumed state = %q", again.State)
	}
	done, err := DisableProtection(again, "op-2", again.Epoch+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DisableProtection(done, "op-2", done.Epoch+1); err != nil {
		t.Fatalf("disabling an already-disabled service must be a no-op: %v", err)
	}
}

// A service whose project intent has been removed but whose durable state is
// disable-pending must still resolve to the projection it was enabled with.
// Rendering depends on it: the server is still archiving to that repository, and
// resolving from the edited project would point it somewhere its own history is
// not.
func TestProtectionDisablePendingKeepsTheProjectionItWasEnabledWith(t *testing.T) {
	pending := pendingProtectionState(t)
	projection := protectionStateProjection()
	withoutIntent := &app.Resolved{
		Spec: &app.Spec{Name: "example", BasePath: "/var/lib/onebox", Services: map[string]app.Service{
			"database": {Driver: "postgres", Version: 17},
		}},
		Env: "production",
	}
	retained, err := withoutIntent.WithServiceRuntimeStates(map[string]app.ServiceRuntimeState{"database": pending.RuntimeState()})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := retained.EffectiveProtectionProjection("database")
	if err != nil {
		t.Fatalf("retained pending projection is unresolvable: %v", err)
	}
	if resolved.Policy.Target != projection.Policy.Target || resolved.Target.Bucket != projection.Target.Bucket {
		t.Fatalf("retained projection = %#v, want the one enablement recorded", resolved)
	}
}

func TestRuntimeStateDoesNotInferImageEvidenceFromReference(t *testing.T) {
	state, err := NewProtectionLifecycleState("example", "production", "database", 1)
	if err != nil {
		t.Fatal(err)
	}
	state.State = ProtectionDisabled
	state.Phase = ProtectionPhaseIdle
	state.ServiceImage = "postgres@sha256:" + strings.Repeat("a", 64)
	if err := state.Seal(); err != nil {
		t.Fatal(err)
	}
	runtime := state.RuntimeState()
	if runtime.PublicationVerified || runtime.DigestAvailable || runtime.CacheVerified {
		t.Fatalf("runtime inferred evidence from an image string: %#v", runtime)
	}
}

// A disablement that died between its two state writes must not trap the
// operator. Re-enabling is how they change their mind; refusing would leave the
// only way out as completing a disable they no longer want.
func TestEnableReconvergesFromAHalfFinishedDisable(t *testing.T) {
	pending := pendingProtectionState(t)
	enabled, err := EnableProtection(pending, protectionStateProjection(),
		"postgres@sha256:"+strings.Repeat("a", 64), "op-3", true, pending.Epoch+1)
	if err != nil {
		t.Fatalf("re-enabling a pending disablement: %v", err)
	}
	if enabled.State != ProtectionEnabled || !enabled.PrerequisiteEffective {
		t.Fatalf("re-enabled state = %#v", enabled)
	}
}
