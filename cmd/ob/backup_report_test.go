package main

import "testing"

func TestBackupReportFlowHasNoLegacyCommandOrFlag(t *testing.T) {
	root := newRootCmd()
	for _, command := range root.Commands() {
		if command.Name() == "backup-evidence" {
			t.Fatal("legacy backup-evidence command is registered")
		}
	}

	checks := []struct {
		path       []string
		wantFlags  []string
		legacyFlag string
	}{
		{[]string{"plan"}, []string{"backup-report-out"}, "backup-evidence"},
		{[]string{"job", "plan"}, []string{"backup-report-out"}, "backup-evidence"},
		{[]string{"approve"}, []string{"backup-report"}, "backup-evidence"},
		{[]string{"deploy"}, []string{"backup-report"}, "backup-evidence"},
		{[]string{"job", "run"}, []string{"backup-report"}, "backup-evidence"},
	}
	for _, check := range checks {
		command, _, err := root.Find(check.path)
		if err != nil {
			t.Fatalf("find %v: %v", check.path, err)
		}
		for _, flag := range check.wantFlags {
			if command.Flags().Lookup(flag) == nil {
				t.Errorf("%s missing --%s", command.CommandPath(), flag)
			}
		}
		if command.Flags().Lookup(check.legacyFlag) != nil {
			t.Errorf("%s still exposes --%s", command.CommandPath(), check.legacyFlag)
		}
	}
}
