package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBackupFactsManifestIsStrictAndCapped(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "facts.json")
	if err := os.WriteFile(valid, []byte(`{
  "schema_version":"onebox.run/migration-backup-facts/v1alpha1",
  "resources":[],
  "key_material":[]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBackupFactsManifest(valid)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != backupFactsSchemaVersion {
		t.Fatalf("schema = %q", manifest.SchemaVersion)
	}

	unknown := filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"schema_version":"onebox.run/migration-backup-facts/v1alpha1","resources":[],"secret":"must-not-be-accepted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBackupFactsManifest(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error = %v", err)
	}
}

func TestBackupEvidenceCreateCommandIsRegistered(t *testing.T) {
	root := newRootCmd()
	command, _, err := root.Find([]string{"backup-evidence", "create"})
	if err != nil {
		t.Fatal(err)
	}
	if command.CommandPath() != "ob backup-evidence create" {
		t.Fatalf("command path = %q", command.CommandPath())
	}
	for _, name := range []string{"plan", "manifest", "out"} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
}
