package app

import (
	"strings"
	"testing"
)

func TestProtectedServiceReservesRestoreRuntimeNames(t *testing.T) {
	spec := &Spec{
		Name: "example",
		Services: map[string]Service{
			"database": {Driver: "postgres", Version: 17, Volumes: []string{"data"}, Protection: &ProtectionPolicy{Target: "offsite"}},
		},
	}
	all := spec.All("production")
	for _, reserved := range []string{
		"ob_example_database_restore",
		"example-database-restore-1",
		"ob_example_database_restore-net",
		"ob_example_database_restore-stage",
	} {
		if !contains(all, reserved) {
			t.Errorf("protected runtime name %q is not reserved: %#v", reserved, all)
		}
	}
}

func TestProtectedForeignCollisionFailsClosedWithoutAdoption(t *testing.T) {
	reserved := []string{"ob_example_database_restore-stage"}
	checks := collisionChecks("example", reserved, map[string]string{reserved[0]: "other-app"})
	if len(checks) != 1 || checks[0].OK || !strings.Contains(checks[0].Detail, "owned by application other-app") || !strings.Contains(checks[0].Remedy, "will not adopt") {
		t.Fatalf("foreign collision checks = %#v", checks)
	}
	owned := collisionChecks("example", reserved, map[string]string{reserved[0]: "example"})
	if len(owned) != 1 || !owned[0].OK {
		t.Fatalf("own prior resource was treated as foreign: %#v", owned)
	}
}
