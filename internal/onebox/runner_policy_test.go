package onebox

import (
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/buildinfo"
)

func TestEnforceRunnerPolicy(t *testing.T) {
	runner := buildinfo.Runner{
		Info: buildinfo.Info{Version: "v2026.8.4"},
		SupportedExecutablePlanSchemas: []string{
			"onebox.run/executable-deploy-plan/v1alpha2",
		},
	}
	policy := app.Policy{
		MinimumOneboxVersion: "v2026.8.3",
		MinimumPlanSchema:    "onebox.run/executable-deploy-plan/v1alpha1",
	}
	if err := enforceRunnerPolicy(policy, runner, "onebox.run/executable-deploy-plan/v1alpha2"); err != nil {
		t.Fatal(err)
	}

	t.Run("version", func(t *testing.T) {
		tooOld := runner
		tooOld.Version = "v2026.7.20"
		err := enforceRunnerPolicy(policy, tooOld, "onebox.run/executable-deploy-plan/v1alpha2")
		if err == nil || !strings.Contains(err.Error(), "below environment minimum") || !strings.Contains(err.Error(), "PATH") {
			t.Fatalf("old runner rejection is not actionable: %v", err)
		}
	})

	t.Run("development runner", func(t *testing.T) {
		development := runner
		development.Version = "v2026.8.3-4-gabcdef"
		err := enforceRunnerPolicy(policy, development, "onebox.run/executable-deploy-plan/v1alpha2")
		if err == nil || !strings.Contains(err.Error(), "not a released Onebox CalVer") || !strings.Contains(err.Error(), "released ob binary") {
			t.Fatalf("development runner rejection is not actionable: %v", err)
		}
	})

	t.Run("invalid minimum", func(t *testing.T) {
		invalid := policy
		invalid.MinimumOneboxVersion = "2026.8.3"
		err := enforceRunnerPolicy(invalid, runner, "onebox.run/executable-deploy-plan/v1alpha2")
		if err == nil || !strings.Contains(err.Error(), "environment minimum Onebox version is invalid") {
			t.Fatalf("invalid minimum rejection is not actionable: %v", err)
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
