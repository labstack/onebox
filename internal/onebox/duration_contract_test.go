package onebox

import (
	"strings"
	"testing"
)

// The plan path must accept every duration the loader does.
//
// The contract's grammar admits a `d` suffix (`gDur`, and the reference says
// "30s, 5m, 1h30m or 14d"), but time.ParseDuration does not. Parsing policy
// durations with the standard library meant `migrations: {backup_max_age: 14d}`
// passed `ob validate` and then failed at plan time with "must be a positive
// duration" — telling the author their value is not a duration when it is the
// syntax the reference gives as an example.
func TestPolicyDurationsAcceptTheContractGrammar(t *testing.T) {
	resource := MigrationBackupResource{Component: "db", Service: "postgres", Type: "service", Persistence: "durable"}
	for _, value := range []string{"14d", "24h", "1h30m", "30s"} {
		requirement := MigrationBackupRequirement{MaxAge: value, Resources: []MigrationBackupResource{resource}}
		if err := requirement.validate(); err != nil && strings.Contains(err.Error(), "max_age") {
			t.Errorf("%s: the loader accepts this duration and the plan path refuses it: %v", value, err)
		}
	}
}
