package onebox

import (
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
)

func TestOnlyARenamedTargetRetiresItsCredentialFile(t *testing.T) {
	next := app.BackupEffectiveProjection{Policy: app.BackupPolicy{Target: "offsite"}}
	sameName := &app.BackupEffectiveProjection{Policy: app.BackupPolicy{Target: "offsite"}}
	otherName := &app.BackupEffectiveProjection{Policy: app.BackupPolicy{Target: "coldline"}}

	if retiresCredentialFile(sameName, next) {
		t.Fatal("editing a target in place retired the credential file this run installs")
	}
	if !retiresCredentialFile(otherName, next) {
		t.Fatal("moving to a differently named target left its credential file behind")
	}
	if retiresCredentialFile(nil, next) {
		t.Fatal("a first enablement retired a credential file that never existed")
	}
}

// An enablement that turns archiving on and then cannot take a base backup
// must leave the service unprotected, not archiving into a repository it never
// reached. PostgreSQL retains every segment an archive_command cannot ship, so
// the alternative is unbounded disk growth established by a command that
// reported failure.
func TestAFailedEnablementIsRevertedToUnprotected(t *testing.T) {
	fresh, err := NewBackupLifecycleState("shop", "production", "postgres", 1)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := EnableBackup(fresh, backupStateProjection(),
		"postgres@sha256:"+strings.Repeat("a", 64), "postgres:18", "op-1", true, 2)
	if err != nil {
		t.Fatal(err)
	}

	pending, disabled, err := disablementAfterFailedEnablement(enabled, "op-1", time.Now())
	if err != nil {
		t.Fatalf("a freshly enabled service could not be taken back: %v", err)
	}

	if pending.State != BackupDisablePending {
		t.Errorf("the restart is not covered by a pending record: state is %v", pending.State)
	}
	if disabled.State != BackupDisabled {
		t.Errorf("the service was left protected: state is %v", disabled.State)
	}
	// The epoch is the fence. Repeating or lowering it would let an operation
	// launched against the enabled state still be accepted afterwards.
	if !(enabled.Epoch < pending.Epoch && pending.Epoch < disabled.Epoch) {
		t.Errorf("epochs do not strictly increase: enabled=%d pending=%d disabled=%d",
			enabled.Epoch, pending.Epoch, disabled.Epoch)
	}
	// What the next render binds. An unprotected runtime is what makes
	// ApplyServices restart the server without archive_mode.
	if state := disabled.RuntimeState().BackupState; state == string(BackupEnabled) {
		t.Errorf("the reverted runtime still renders a server with archiving on: %q", state)
	}
}

func TestRepositoryGenerationFollowsTheDatabaseCluster(t *testing.T) {
	projection := backupStateProjection()
	firstID := "7513211627332151223"
	secondID := "7513211627332151224"

	fresh, err := NewBackupLifecycleState("shop", "production", "postgres", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := backupRepositoryGeneration(fresh, projection, "shop", "postgres", firstID); got != firstID {
		t.Fatalf("new cluster generation = %q, want %q", got, firstID)
	}

	// An old record has no database identity and used the unversioned layout.
	// Its first explicit enable cannot prove the data volume is the old one, so
	// it starts a scoped generation and leaves the legacy history untouched.
	legacy := fresh
	legacy.LastEffective = &projection
	if err := legacy.Seal(); err != nil {
		t.Fatal(err)
	}
	if got := backupRepositoryGeneration(legacy, projection, "shop", "postgres", firstID); got != firstID {
		t.Fatalf("legacy repository generation = %q, want current cluster %q", got, firstID)
	}

	bound := legacy
	bound.DatabaseSystemIdentifier = firstID
	if err := bound.Seal(); err != nil {
		t.Fatal(err)
	}
	if got := backupRepositoryGeneration(bound, projection, "shop", "postgres", firstID); got != "" {
		t.Fatalf("unchanged legacy cluster moved to generation %q", got)
	}
	if got := backupRepositoryGeneration(bound, projection, "shop", "postgres", secondID); got != secondID {
		t.Fatalf("replacement cluster generation = %q, want %q", got, secondID)
	}

	moved := projection
	moved.Target.Bucket = "different-bucket"
	if got := backupRepositoryGeneration(bound, moved, "shop", "postgres", firstID); got != firstID {
		t.Fatalf("moved target generation = %q, want current cluster %q", got, firstID)
	}
}
