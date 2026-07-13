package config

import (
	"strings"
	"testing"
)

func TestMinimumPlanSchemaRequiresCompletePrerelease(t *testing.T) {
	source := strings.Replace(migrationBackupPolicyConfig,
		"      require_migration_backup: true\n",
		"      minimum_plan_schema: onebox.run/executable-deploy-plan/v1alpha\n      require_migration_backup: true\n", 1)
	cfg, err := LoadBytes([]byte(source), "ob.yml")
	if err == nil {
		err = cfg.Validate()
	}
	if err == nil || !strings.Contains(err.Error(), "minimum_plan_schema") {
		t.Fatalf("error = %v, want minimum_plan_schema validation failure", err)
	}
}
