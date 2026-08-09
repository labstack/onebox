package main

import "testing"

// The skeleton must match what the plan will accept.
//
// It hardcoded "not_tested". A policy with require_migration_restore_test then
// refused the templated manifest, and the skeleton carried none of the fields a
// passed test needs — reinstating the discover-by-refusal loop the command
// exists to end.
func TestTheSkeletonMatchesTheRestoreTestPolicy(t *testing.T) {
	if got := restoreTestSkeleton(false); got.State != "not_tested" {
		t.Errorf("without a restore-test policy: state = %q, want not_tested", got.State)
	}
	if got := restoreTestSkeleton(false); got.Method != "" || got.TestedAt != "" || got.ValidationDigest != "" {
		t.Errorf("not_tested refuses method/tested_at/validation_digest, so the skeleton must omit them: %+v", got)
	}
	got := restoreTestSkeleton(true)
	if got.State != "passed" {
		t.Errorf("with a restore-test policy: state = %q, want passed", got.State)
	}
	if got.Method == "" || got.TestedAt == "" || got.ValidationDigest == "" {
		t.Errorf("passed requires method, tested_at and validation_digest; skeleton must offer all three: %+v", got)
	}
}
