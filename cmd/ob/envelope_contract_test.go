package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/onebox"
)

// A code cannot carry one sentence in the envelope and a different one in the
// generated reference: an operator reading the docs and an agent reading the
// envelope would be looking at the same code and told two things. Call sites
// that hardcode SafeMessage are how the two drift apart.
func TestHardcodedEnvelopeMessagesMatchTheRegistry(t *testing.T) {
	// Scanning the source is the only way to pin this: a call site that
	// hardcodes the sentence still compiles, still passes every behavioural
	// test, and only diverges from the generated reference — which no unit
	// test reads. The registry is the single source, so the rule is that no
	// literal message may sit beside a registered code.
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`Code:\s*"([a-z_]+)"\s*,\s*SafeMessage:\s*"`)
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range literal.FindAllStringSubmatch(string(body), -1) {
			code := match[1]
			if registered, ok := onebox.OperationFailureMeaning(code); ok && registered != "" {
				t.Errorf("%s: %s hardcodes a message beside a registered code; use safeMessageForCode so the envelope and the reference cannot drift (reference says %q)",
					source, code, registered)
			}
		}
	}
}

// publicError resolves error.code from three shapes, two of which carry Code
// as a struct field with no method. If typedFailure recognises fewer, an
// interrupt joined onto such an error ships outcome "cancelled" beside a
// specific error.code — the divergence the guard exists to prevent.
func TestTypedFailureRecognisesEveryShapePublicErrorResolves(t *testing.T) {
	interrupted := context.Canceled
	for name, err := range map[string]error{
		"project error":     errors.Join(&app.Error{Code: "project_invalid"}, interrupted),
		"lifecycle failure": errors.Join(onebox.LifecycleFailure{Code: "secret_cleanup_pending"}, interrupted),
		"coded method":      errors.Join(codedTestError{}, interrupted),
	} {
		t.Run(name, func(t *testing.T) {
			if !typedFailure(err, "operation_failed") {
				t.Errorf("typedFailure missed %s, so the envelope would report it as cancelled", name)
			}
			// The guard must still let a plain interrupt be a cancellation.
			if typedFailure(interrupted, "operation_failed") {
				t.Error("a bare interrupt was treated as a typed failure")
			}
		})
	}
}

// A code equal to the fallback is rewritten to "cancelled" by publicError, so
// the outcome must agree rather than claiming a typed failure.
func TestFallbackCodedInterruptStaysACancellation(t *testing.T) {
	err := errors.Join(codedFallbackError{}, context.Canceled)
	if typedFailure(err, "operation_failed") {
		t.Error("a code equal to the fallback was treated as typed; publicError rewrites it to \"cancelled\"")
	}
	if got := cancellationExitCode(err, "operation_failed"); got != 2 {
		t.Errorf("exit code = %d, want 2 to match the cancelled envelope", got)
	}
}

// The published contract says the exit code matches the terminal outcome: 2
// means cancelled and nothing was changed, which a committed-and-live
// post-commit failure must never claim.
func TestTypedInterruptDoesNotExitAsCancelled(t *testing.T) {
	err := errors.Join(codedTestError{}, context.Canceled)
	if got := cancellationExitCode(err, "operation_failed"); got != 1 {
		t.Errorf("exit code = %d, want 1: the envelope reports outcome error for this case", got)
	}
}

type codedFallbackError struct{}

func (codedFallbackError) Error() string { return "generic" }
func (codedFallbackError) Code() string  { return "operation_failed" }

type codedTestError struct{}

func (codedTestError) Error() string { return "coded" }
func (codedTestError) Code() string  { return "secret_cleanup_pending" }

// Guidance is advertised as a safe Onebox command, so a literal one has to
// satisfy the same check setCommandGuidance applies to the rest.
func TestLiteralGuidanceCommandsAreSafe(t *testing.T) {
	for _, command := range []string{
		"ob logs <workload> --follow --output ndjson",
	} {
		if !onebox.SafeGuidanceCommand(command) {
			t.Errorf("guidance %q is published but fails SafeGuidanceCommand", command)
		}
	}
}
