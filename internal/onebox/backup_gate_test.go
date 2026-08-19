package onebox

import "testing"

// The gate must be satisfiable in the configuration everyone starts from.
//
// A policy requiring no key material leaves RequiredKeyMaterial nil, while a
// receipt with none carries a zero-length slice. reflect.DeepEqual called those
// different, so every earlier locally wrapped report was refused
// and the feature could not be used at all unless migrations.backup_key_material
// happened to be declared.
func TestKeyMaterialSatisfiesTreatsNilAndEmptyAsTheSame(t *testing.T) {
	if !keyMaterialSatisfies(nil, nil) {
		t.Error("nil receipt against nil requirement must satisfy")
	}
	if !keyMaterialSatisfies([]MigrationBackupKeyMaterialEvidence{}, nil) {
		t.Error("a receipt with no key material must satisfy a requirement asking for none")
	}
	if !keyMaterialSatisfies([]MigrationBackupKeyMaterialEvidence{{Name: "app_key"}}, []string{"app_key"}) {
		t.Error("matching names must satisfy")
	}
	if keyMaterialSatisfies(nil, []string{"app_key"}) {
		t.Error("a missing required key must not satisfy")
	}
	if keyMaterialSatisfies([]MigrationBackupKeyMaterialEvidence{{Name: "other"}}, []string{"app_key"}) {
		t.Error("a different key must not satisfy")
	}
}
