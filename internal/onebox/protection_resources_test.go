package onebox

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func protectionResourceFixture() []ProtectionResource {
	owned := func(identity string, kind ProtectionResourceKind) ProtectionResource {
		return ProtectionResource{Identity: identity, Kind: kind, OwnerApplication: "example", OwnerEnvironment: "production", Service: "database"}
	}
	return []ProtectionResource{
		owned("ob-example-database-backup.timer", ProtectionResourceUnit),
		owned("/var/lib/onebox/example/protection/archive-hook.json", ProtectionResourceHook),
		owned("s3://backups/example/database/generation-7", ProtectionRemoteBackup),
		owned("manifest-7", ProtectionManifest),
		owned("postgres@sha256:"+strings.Repeat("a", 64), ProtectionManifestImage),
		owned("example-database-data", ProtectionServiceVolume),
		owned("example-database-data-previous", ProtectionPreviousVolume),
		{Identity: "ob-foreign-database-backup.timer", Kind: ProtectionResourceUnit, OwnerApplication: "other", OwnerEnvironment: "production", Service: "database"},
	}
}

func TestProtectionDisableRemovalTouchesOnlyOwnedLocalResources(t *testing.T) {
	inspection, err := InspectProtectionResources("example", "production", protectionResourceFixture())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewProtectionRemovalPlan(inspection, ProtectionRemovalRequest{
		Mode: KindProtectionDisable, Application: "example", Environment: "production", Service: "database",
		ProtectionState: "disabled", StateDigest: "sha256:" + strings.Repeat("b", 64), PrerequisitesVerifiedAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	authorization := ProtectionRemovalAuthorization{Operation: plan.Mode, PlanDigest: plan.PlanDigest, StateDigest: plan.StateDigest}
	if err := ApplyProtectionRemoval(plan, authorization, func(resource ProtectionResource) error {
		removed = append(removed, resource.Identity)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/var/lib/onebox/example/protection/archive-hook.json", "ob-example-database-backup.timer"}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed = %#v, want %#v", removed, want)
	}
	for _, protected := range []string{"generation-7", "manifest-7", "postgres@sha256", "data-previous", "foreign"} {
		for _, identity := range removed {
			if strings.Contains(identity, protected) {
				t.Fatalf("removed preserved or foreign resource %q", identity)
			}
		}
	}
}

func TestProtectionPendingRemovalIsRefusedBeforeMutation(t *testing.T) {
	inspection, err := InspectProtectionResources("example", "production", protectionResourceFixture())
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewProtectionRemovalPlan(inspection, ProtectionRemovalRequest{
		Mode: KindProtectionDisable, Application: "example", Environment: "production", Service: "database",
		ProtectionState: "disable-pending", StateDigest: "sha256:" + strings.Repeat("b", 64), PrerequisitesVerifiedAbsent: true,
	})
	var failure LifecycleFailure
	if !errors.As(err, &failure) || failure.Code != "protection_disable_pending" {
		t.Fatalf("pending removal error = %v", err)
	}
}

func TestProtectionRemovalRequiresMatchingAuthorization(t *testing.T) {
	inspection, err := InspectProtectionResources("example", "production", protectionResourceFixture())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewProtectionRemovalPlan(inspection, ProtectionRemovalRequest{
		Mode: KindProtectionDisable, Application: "example", Environment: "production", Service: "database",
		ProtectionState: "disabled", StateDigest: "sha256:" + strings.Repeat("b", 64), PrerequisitesVerifiedAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = ApplyProtectionRemoval(plan, ProtectionRemovalAuthorization{
		Operation: plan.Mode, PlanDigest: "sha256:" + strings.Repeat("c", 64), StateDigest: plan.StateDigest,
	}, func(ProtectionResource) error { called = true; return nil })
	var failure LifecycleFailure
	if !errors.As(err, &failure) || failure.Code != "protection_disablement_not_authorized" || called {
		t.Fatalf("authorization refusal = %v, remover called = %v", err, called)
	}
}

func TestDestroyRemovesOnlyUnreferencedOwnedExecutables(t *testing.T) {
	resources := protectionResourceFixture()
	resources = append(resources,
		ProtectionResource{Identity: "/var/lib/onebox/runner", Kind: ProtectionResourceRunner, OwnerApplication: "example", OwnerEnvironment: "production", Referenced: true},
		ProtectionResource{Identity: "/var/lib/onebox/envelope", Kind: ProtectionResourceEnvelope, OwnerApplication: "example", OwnerEnvironment: "production"},
	)
	inspection, err := InspectProtectionResources("example", "production", resources)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewProtectionRemovalPlan(inspection, ProtectionRemovalRequest{
		Mode: KindDestroy, Application: "example", Environment: "production", ProtectionState: "disabled",
		StateDigest: "sha256:" + strings.Repeat("d", 64), PrerequisitesVerifiedAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	removed := make(map[string]bool)
	authorization := ProtectionRemovalAuthorization{Operation: plan.Mode, PlanDigest: plan.PlanDigest, StateDigest: plan.StateDigest}
	if err := ApplyProtectionRemoval(plan, authorization, func(resource ProtectionResource) error {
		removed[resource.Identity] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !removed["/var/lib/onebox/envelope"] || removed["/var/lib/onebox/runner"] {
		t.Fatalf("destroy removal set = %#v", removed)
	}
	for _, resource := range inspection.Preserved {
		if removed[resource.Identity] {
			t.Fatalf("destroy removed preserved resource %q", resource.Identity)
		}
	}
}

func TestProtectionRemovalPlanTamperIsRefused(t *testing.T) {
	inspection, err := InspectProtectionResources("example", "production", protectionResourceFixture())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewProtectionRemovalPlan(inspection, ProtectionRemovalRequest{
		Mode: KindDestroy, Application: "example", Environment: "production", ProtectionState: "disabled",
		StateDigest: "sha256:" + strings.Repeat("d", 64), PrerequisitesVerifiedAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.Remove = append(plan.Remove, ProtectionResource{Identity: "remote", Kind: ProtectionRemoteBackup, OwnerApplication: "example", OwnerEnvironment: "production"})
	if err := ApplyProtectionRemoval(plan, ProtectionRemovalAuthorization{}, func(ProtectionResource) error { return nil }); err == nil {
		t.Fatal("tampered removal plan was accepted")
	}
}
