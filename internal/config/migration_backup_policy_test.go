package config

import (
	"strings"
	"testing"
	"time"
)

const migrationBackupPolicyConfig = `
api_version: onebox.run/v1
app: demo
compose: compose.yaml
environments:
  production:
    target: deploy@example.test
    policy:
      require_migration_backup: true
      migration_backup_max_age: 24h
      require_migration_restore_test: true
      migration_backup_key_material: [application_encryption_key, runtime_environment]
components:
  web:
    type: application
    deployment: { strategy: recreate }
  migrate:
    type: job
    data_effect: migration
  database:
    type: postgres
    persistence: { mode: durable, volumes: [db_data] }
deployment: { order: [web] }
`

func TestMigrationBackupEnvironmentPolicy(t *testing.T) {
	cfg, err := LoadBytes([]byte(migrationBackupPolicyConfig), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	policy := cfg.Environments["production"].Policy
	if !policy.MigrationBackupRequired() || !policy.MigrationRestoreTestRequired() {
		t.Fatalf("migration backup policy defaults were not preserved: %#v", policy)
	}
	if got := time.Duration(policy.MigrationBackupMaxAge); got != 24*time.Hour {
		t.Fatalf("max age = %s, want 24h", got)
	}

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "missing freshness window",
			config:  strings.Replace(migrationBackupPolicyConfig, "      migration_backup_max_age: 24h\n", "", 1),
			wantErr: "migration_backup_max_age",
		},
		{
			name:    "override without approval ceremony",
			config:  strings.Replace(migrationBackupPolicyConfig, "      require_migration_backup: true\n", "      require_approval: false\n      require_migration_backup: true\n", 1),
			wantErr: "requires require_approval",
		},
		{
			name:    "inactive backup settings",
			config:  strings.Replace(migrationBackupPolicyConfig, "require_migration_backup: true", "require_migration_backup: false", 1),
			wantErr: "migration backup settings require",
		},
		{
			name:    "duplicate key material",
			config:  strings.Replace(migrationBackupPolicyConfig, "[application_encryption_key, runtime_environment]", "[runtime_environment, runtime_environment]", 1),
			wantErr: "duplicate",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := LoadBytes([]byte(test.config), "ob.yml")
			if err == nil {
				err = cfg.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}
