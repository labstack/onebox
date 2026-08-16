package onebox

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
)

// codeLiteral matches the two ways a code reaches an operator: a Code() method
// on a typed error, and a literal passed to one of the CLI's public-error
// constructors. Scanning the source rather than a hand-kept list is what makes
// this a drift test instead of a second copy of the same list.
var (
	codeMethod      = regexp.MustCompile(`func \([^)]*\) Code\(\) string\s*{\s*return "([a-z_]+)"`)
	codeField       = regexp.MustCompile(`Code:\s*"([a-z_]+)"`)
	codeConstructor = regexp.MustCompile(`(?:writeStructuredCommandFailure|publicError|codedError)\([^)]*?"([a-z_]+)"`)
)

func emittedOperationCodes(t *testing.T) map[string]string {
	t.Helper()
	roots := []string{
		filepath.Join("..", "..", "cmd", "ob"),
		filepath.Join("..", "engine"),
		filepath.Join("..", "release"),
		".",
	}
	found := map[string]string{}
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			// The registry itself declares every code; scanning it would make the
			// test tautological.
			if filepath.Base(path) == "operation_errors.go" {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, pattern := range []*regexp.Regexp{codeMethod, codeField, codeConstructor} {
				for _, match := range pattern.FindAllStringSubmatch(string(body), -1) {
					if _, seen := found[match[1]]; !seen {
						found[match[1]] = path
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}
	return found
}

// owned reports whether another family already publishes this code, so the
// three tables stay disjoint and no code is documented twice with two meanings.
func owned(code string) bool {
	if _, ok := app.ErrorCodeMeaning(code); ok {
		return true
	}
	_, ok := LifecycleFailureMeaning(code)
	return ok
}

func TestEveryEmittedOperationCodeIsEnumerated(t *testing.T) {
	var missing []string
	for code, path := range emittedOperationCodes(t) {
		if owned(code) {
			continue
		}
		if _, ok := OperationFailureMeaning(code); !ok {
			missing = append(missing, code+" ("+path+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("codes reach an operator but are not enumerated in operation_errors.go, so the error reference cannot document them:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

func TestEveryEnumeratedOperationCodeIsEmitted(t *testing.T) {
	emitted := emittedOperationCodes(t)
	var unreachable []string
	for _, code := range OperationFailureCodes() {
		if _, ok := emitted[code]; !ok {
			unreachable = append(unreachable, code)
		}
	}
	if len(unreachable) > 0 {
		t.Fatalf("enumerated operation codes that nothing emits, so the reference promises failures an operator cannot cause:\n  %s",
			strings.Join(unreachable, "\n  "))
	}
}

func TestOperationCodesDoNotCollideWithOtherFamilies(t *testing.T) {
	for _, code := range OperationFailureCodes() {
		if owned(code) {
			t.Errorf("code %q is published by more than one family; a reader cannot tell which meaning applies", code)
		}
	}
}

func TestEveryOperationGuidanceCommandIsSafeAndClassified(t *testing.T) {
	// The table itself is validated at package init, so asserting the same
	// conditions over it again cannot fail. What is worth pinning is the
	// classifier's behaviour on the shapes the table actually contains, and the
	// rule that a command-less definition carries no role.
	for _, code := range OperationFailureCodes() {
		failure, _ := OperationFailureFor(code)
		if failure.Command == "" {
			continue
		}
		switch failure.GuidanceRole() {
		case "diagnostic", "next", "resolving":
		default:
			t.Errorf("code %q has command %q with no semantic role", code, failure.Command)
		}
	}
	if (OperationFailure{Message: "x"}).GuidanceRole() != "" {
		t.Error("a definition with no command reported a guidance role")
	}
}

func TestOperationGuidanceRejectsUnsafeCommands(t *testing.T) {
	for _, command := range []string{
		"ob audit > /tmp/x",          // redirect
		"ob exec -- rm -rf / ; echo", // separator
		"ob deploy --token=abc",      // credential-shaped
		"rm -rf /",                   // not an ob command
		"ob logs `id`",               // substitution
	} {
		if safeOperationCommand(command) {
			t.Errorf("unsafe guidance accepted: %q", command)
		}
	}
	for _, command := range []string{
		"ob plan --output json",
		"ob approve --plan <path>",
		"ob deploy --image <workload>=<reference>",
	} {
		if !safeOperationCommand(command) {
			t.Errorf("safe guidance rejected: %q", command)
		}
	}
}

// An expired job plan is re-planned by a different command than an expired
// deployment plan, so one shared guidance value sends half the callers to the
// wrong place.
func TestPlanExpiredGuidanceNamesTheRightReplanCommand(t *testing.T) {
	deployment := &PlanExpiredError{Kind: PlanKindDeployment}
	if got := deployment.GuidanceCommand(); got != "ob plan --output json" {
		t.Errorf("deployment plan guidance = %q", got)
	}
	job := &PlanExpiredError{Kind: PlanKindJob, Job: "migrate"}
	if got := job.GuidanceCommand(); got != "ob job plan migrate --output json" {
		t.Errorf("job plan guidance = %q", got)
	}
	if got := (&PlanExpiredError{Kind: PlanKindJob}).GuidanceCommand(); got != "ob job plan <job> --output json" {
		t.Errorf("unnamed job guidance = %q", got)
	}
	for _, err := range []*PlanExpiredError{deployment, job} {
		if !SafeGuidanceCommand(err.GuidanceCommand()) {
			t.Errorf("guidance %q is not publishable", err.GuidanceCommand())
		}
	}
}

// Guidance that immediately refuses is worse than none: an agent follows it and
// receives plan_required instead of progress.
func TestNoPublishedGuidanceIsAStructuredDeployWithoutAPlan(t *testing.T) {
	for _, code := range OperationFailureCodes() {
		failure, _ := OperationFailureFor(code)
		if refusesWithoutPlan(failure.Command) {
			t.Errorf("code %q publishes %q, which refuses without --plan", code, failure.Command)
		}
	}
}

// The registry is not the only source of published guidance: a typed error can
// override it through GuidanceCommand(), and publicError prefers the override.
// Those bypass the loop above entirely.
func TestNoTypedOverrideIsAStructuredDeployWithoutAPlan(t *testing.T) {
	for name, command := range typedGuidanceOverrides() {
		if refusesWithoutPlan(command) {
			t.Errorf("%s overrides guidance with %q, which refuses without --plan", name, command)
		}
		if command != "" && !SafeGuidanceCommand(command) {
			t.Errorf("%s overrides guidance with %q, which is not a safe Onebox command", name, command)
		}
	}
}

// Any `ob deploy` is a dead end for a structured caller, whether or not the
// published string already carries --output: an agent adds it, and a deploy
// without --plan is then refused with plan_required. Requiring the literal
// --output is what let the loader's own image_unresolved guidance through.
func refusesWithoutPlan(command string) bool {
	return strings.HasPrefix(command, "ob deploy") && !strings.Contains(command, "--plan")
}

// typedGuidanceOverrides enumerates every error that overrides published
// guidance through GuidanceCommand(). It is hand-maintained on purpose: adding
// an override without adding it here is what let the image-resolution command
// publish a structured deploy that refuses.
func typedGuidanceOverrides() map[string]string {
	return map[string]string{
		// ImageResolutionError's real command is pinned in package engine,
		// where its constructor lives — a literal here would pass no matter
		// what the constructor produces.
		"engine.MigrationGateClosedError (abort)": (&engine.MigrationGateClosedError{
			InterruptedID: "R1", Refused: "abort",
		}).GuidanceCommand(),
		"engine.MigrationGateClosedError (recovery)": (&engine.MigrationGateClosedError{
			InterruptedID: "R1", Refused: "recovery",
		}).GuidanceCommand(),
		"engine.PostActivationFailedError": (&engine.PostActivationFailedError{
			ReleaseID: "R1",
		}).GuidanceCommand(),
		"engine.PostActivationFailedError (rollback)": (&engine.PostActivationFailedError{
			ReleaseID: "R1", Guidance: "ob status --output json",
		}).GuidanceCommand(),
	}
}
