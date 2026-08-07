package onebox

import (
	"errors"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

func protectedIdentityConfig(serviceName string, withPolicy bool) *app.Resolved {
	service := app.Service{Driver: "postgres", Version: 17, Volumes: []string{"data"}}
	if withPolicy {
		service.Protection = &app.ProtectionPolicy{Target: "offsite"}
	}
	return &app.Resolved{
		Spec: &app.Spec{Name: "example", BasePath: "/var/lib/ob", Services: map[string]app.Service{serviceName: service}},
		Env:  "production",
	}
}

func TestProtectedServiceIdentityBindsEveryGeneratedName(t *testing.T) {
	record, err := NewProtectedServiceIdentity(protectedIdentityConfig("database", true), "database", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	if record.ServiceProject != "ob_example_database" || record.RestoreProject != "ob_example_database_restore" ||
		record.RestoreContainer != "example_database_restore" || record.RestoreNetwork != "ob_example_database_restore-net" ||
		record.RestoreVolume != "ob_example_database_restore-stage" || record.StatePath != "/var/lib/ob/example/protection/state/database.active-volume.json" {
		t.Fatalf("protected identity = %#v", record)
	}
	if len(record.Timers) != 4 {
		t.Fatalf("protected timers = %#v", record.Timers)
	}
}

func TestManifestBindsIdentityAfterPolicyRemoval(t *testing.T) {
	record, err := NewProtectedServiceIdentity(protectedIdentityConfig("database", false), "database", true)
	if err != nil {
		t.Fatal(err)
	}
	if !record.ManifestBound {
		t.Fatal("manifest-bound identity lost its binding")
	}
	if err := ValidateProtectedServiceIdentity(protectedIdentityConfig("database", false), record); err != nil {
		t.Fatalf("validate retained manifest identity: %v", err)
	}
}

func TestProtectedServiceRenameFailsClosed(t *testing.T) {
	record, err := NewProtectedServiceIdentity(protectedIdentityConfig("database", true), "database", true)
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateProtectedServiceIdentity(protectedIdentityConfig("db", true), record)
	var failure LifecycleFailure
	if !errors.As(err, &failure) || failure.Code != "protected_service_identity_changed" {
		t.Fatalf("rename error = %v", err)
	}
}

func TestProtectedServiceIdentityDetectsTamper(t *testing.T) {
	record, err := NewProtectedServiceIdentity(protectedIdentityConfig("database", true), "database", false)
	if err != nil {
		t.Fatal(err)
	}
	record.RestoreVolume = "foreign_volume"
	if err := record.Validate(); err == nil {
		t.Fatal("tampered protected identity was accepted")
	}
}
