package journal

import (
	"reflect"
	"testing"
)

func TestSummarizeRecoversMigrationBackupEvidence(t *testing.T) {
	evidence := &MigrationBackupEvidence{
		Mode: "override", OverrideDigest: "sha256:override",
		ProtectedResources: []string{"database/postgres"}, ValidUntil: "2026-07-13T18:15:00Z",
		OverrideOperator: "operator@example.test", OverrideReason: "incident INC-42",
		OverrideCreatedAt: "2026-07-13T18:01:00Z", OverrideSource: "local_cli",
	}
	summary := Summarize([]Record{
		{DeployID: "R1", Epoch: 1, Phase: "deploy", Event: "start", MigrationBackup: evidence},
		{DeployID: "R1", Epoch: 1, Phase: "pre-release", SubStep: MigrationBackupSubStep, Event: "result", Status: "ok", MigrationBackup: evidence},
	})
	if !summary.Done[MigrationBackupSubStep] {
		t.Fatal("accepted migration backup evidence was not marked complete")
	}
	if !reflect.DeepEqual(summary.MigrationBackup, evidence) {
		t.Fatalf("recovered evidence differs:\n got: %#v\nwant: %#v", summary.MigrationBackup, evidence)
	}
}

func TestSummarizeKeepsMigrationBackupRequirementSticky(t *testing.T) {
	summary := Summarize([]Record{
		{
			DeployID: "R1", Epoch: 1, Phase: "deploy", Event: "start",
			MigrationBackupRequired: true,
		},
		{
			// A resume-era record that omits the flag must not weaken the safety
			// requirement recorded by the original deployment attempt.
			DeployID: "R1", Epoch: 2, Phase: "deploy", Event: "start",
		},
		{
			DeployID: "R1", Epoch: 2, Phase: "pre-release", Event: "intent", SubStep: "job:migrate",
		},
	})
	if !summary.MigrationBackupRequired {
		t.Fatal("later journal records erased the original migration backup requirement")
	}
}
