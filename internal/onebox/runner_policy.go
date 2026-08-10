package onebox

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/buildinfo"
)

var executableSchemaVersion = regexp.MustCompile(`^onebox\.run/executable-(?:deploy|job)-plan/v([0-9]+)(?:(alpha|beta)([0-9]+))?$`)

// CheckRunnerCompatibility applies the same version and executable-plan schema
// policy used at execution without loading a plan or performing any mutation.
func CheckRunnerCompatibility(policy app.Policy, runner buildinfo.Runner) error {
	return enforceRunnerPolicy(policy, runner, ExecutableDeployPlanSchemaVersion)
}

func enforceRunnerPolicy(policy app.Policy, runner buildinfo.Runner, planSchema string) error {
	if minimum := strings.TrimSpace(policy.MinimumOneboxVersion); minimum != "" {
		minimumVersion, err := buildinfo.ParseReleaseVersion(minimum)
		if err != nil {
			return fmt.Errorf("environment minimum Onebox version is invalid: %w", err)
		}
		currentVersion, err := buildinfo.ParseReleaseVersion(runner.Version)
		if err != nil {
			return fmt.Errorf(
				"runner version %q is not a released Onebox CalVer and cannot be compared with environment minimum %q — install a released ob binary and run `ob doctor`",
				runner.Version,
				minimum,
			)
		}
		if buildinfo.CompareReleaseVersions(currentVersion, minimumVersion) < 0 {
			return fmt.Errorf(
				"runner version %s is below environment minimum %s — update the ob binary selected by PATH and run `ob doctor`",
				runner.Version,
				minimum,
			)
		}
	}
	if minimum := strings.TrimSpace(policy.MinimumPlanSchema); minimum != "" {
		atLeast, err := executableSchemaAtLeast(planSchema, minimum)
		if err != nil {
			return err
		}
		if !atLeast {
			return fmt.Errorf(
				"executable plan schema %s is below environment minimum %s — update ob and re-plan",
				planSchema,
				minimum,
			)
		}
		supported := false
		for _, schema := range runner.SupportedExecutablePlanSchemas {
			atLeast, err := executableSchemaAtLeast(schema, minimum)
			if err == nil && atLeast {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf(
				"runner %s supports no executable plan schema meeting environment minimum %s — update ob",
				runner.Version,
				minimum,
			)
		}
	}
	return nil
}

type schemaVersion struct {
	major int
	stage int // alpha=0, beta=1, stable=2
	minor int
}

func parseExecutableSchemaVersion(schema string) (schemaVersion, error) {
	matches := executableSchemaVersion.FindStringSubmatch(schema)
	if matches == nil {
		return schemaVersion{}, fmt.Errorf("unsupported executable plan schema %q", schema)
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return schemaVersion{}, fmt.Errorf("unsupported executable plan schema %q: major version is out of range", schema)
	}
	version := schemaVersion{major: major, stage: 2}
	if matches[2] != "" {
		version.stage = 0
		if matches[2] == "beta" {
			version.stage = 1
		}
		minor, err := strconv.Atoi(matches[3])
		if err != nil {
			return schemaVersion{}, fmt.Errorf("unsupported executable plan schema %q: prerelease version is out of range", schema)
		}
		version.minor = minor
	}
	return version, nil
}

func executableSchemaAtLeast(actual, minimum string) (bool, error) {
	a, err := parseExecutableSchemaVersion(actual)
	if err != nil {
		return false, err
	}
	m, err := parseExecutableSchemaVersion(minimum)
	if err != nil {
		return false, err
	}
	if a.major != m.major {
		return a.major > m.major, nil
	}
	if a.stage != m.stage {
		return a.stage > m.stage, nil
	}
	return a.minor >= m.minor, nil
}
