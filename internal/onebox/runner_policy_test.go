package onebox

import (
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/config"
)

func TestEnforceRunnerPolicy(t *testing.T) {
	runner := buildinfo.Runner{
		Info: buildinfo.Info{Version: "1.4.0"},
		SupportedExecutablePlanSchemas: []string{
			"onebox.run/executable-deploy-plan/v1alpha2",
		},
	}
	policy := config.EnvironmentPolicy{
		MinimumOneboxVersion: "1.3.2",
		MinimumPlanSchema:    "onebox.run/executable-deploy-plan/v1alpha1",
	}
	if err := enforceRunnerPolicy(policy, runner, "onebox.run/executable-deploy-plan/v1alpha2"); err != nil {
		t.Fatal(err)
	}

	t.Run("version", func(t *testing.T) {
		tooOld := runner
		tooOld.Version = "1.2.9"
		err := enforceRunnerPolicy(policy, tooOld, "onebox.run/executable-deploy-plan/v1alpha2")
		if err == nil || !strings.Contains(err.Error(), "below environment minimum") || !strings.Contains(err.Error(), "PATH") {
			t.Fatalf("old runner rejection is not actionable: %v", err)
		}
	})

	t.Run("plan schema", func(t *testing.T) {
		policy.MinimumPlanSchema = "onebox.run/executable-deploy-plan/v1alpha3"
		err := enforceRunnerPolicy(policy, runner, "onebox.run/executable-deploy-plan/v1alpha2")
		if err == nil || !strings.Contains(err.Error(), "below environment minimum") {
			t.Fatalf("old plan schema was not rejected: %v", err)
		}
	})
}

func TestExecutableSchemaOrdering(t *testing.T) {
	tests := []struct {
		actual, minimum string
		want            bool
	}{
		{"onebox.run/executable-deploy-plan/v1alpha2", "onebox.run/executable-deploy-plan/v1alpha1", true},
		{"onebox.run/executable-deploy-plan/v1beta1", "onebox.run/executable-deploy-plan/v1alpha99", true},
		{"onebox.run/executable-deploy-plan/v1", "onebox.run/executable-deploy-plan/v1beta9", true},
		{"onebox.run/executable-deploy-plan/v2alpha1", "onebox.run/executable-deploy-plan/v1", true},
		{"onebox.run/executable-deploy-plan/v1alpha1", "onebox.run/executable-deploy-plan/v1alpha2", false},
	}
	for _, test := range tests {
		got, err := executableSchemaAtLeast(test.actual, test.minimum)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("schemaAtLeast(%q, %q) = %v, want %v", test.actual, test.minimum, got, test.want)
		}
	}
}

func TestExecutableSchemaRejectsNumericOverflow(t *testing.T) {
	overflow := strings.Repeat("9", 100)
	for _, schema := range []string{
		"onebox.run/executable-deploy-plan/v" + overflow,
		"onebox.run/executable-deploy-plan/v1alpha" + overflow,
	} {
		if _, err := parseExecutableSchemaVersion(schema); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("overflowing schema %q error = %v", schema, err)
		}
	}
}
