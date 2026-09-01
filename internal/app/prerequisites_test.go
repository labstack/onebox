package app

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func prerequisiteFake(overrides map[string]transport.Result) *transport.Fake {
	defaults := map[string]transport.Result{
		"docker version":            {Stdout: "27.0.3\n"},
		"docker compose version":    {Stdout: "2.29.1\n"},
		"imagetools inspect --help": {Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n      --format string\n"},
		"docker buildx version":     {Stdout: "github.com/docker/buildx v0.33.0\n"},
	}
	for key, result := range overrides {
		defaults[key] = result
	}
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		// Longest match first: "docker buildx version" also contains "version".
		best, found := "", transport.Result{}
		for key, result := range defaults {
			if strings.Contains(cmd, key) && len(key) > len(best) {
				best, found = key, result
			}
		}
		if best == "" {
			return transport.Result{}, false
		}
		return found, true
	}
	return f
}

func TestCheckHostPrerequisitesReportsEveryOne(t *testing.T) {
	checks, err := CheckHostPrerequisites(context.Background(), prerequisiteFake(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{PrerequisiteRuntime, PrerequisiteCompose, PrerequisiteResolver}
	if len(checks) != len(want) {
		t.Fatalf("checks = %+v, want one per prerequisite", checks)
	}
	for i, name := range want {
		if checks[i].Name != name || !checks[i].OK {
			t.Fatalf("check %d = %+v, want %q satisfied", i, checks[i], name)
		}
	}
	// The observed version is what makes a bug report actionable: a client that
	// advertises the capability and ignores it passes the probe, and only the
	// version distinguishes it.
	if !strings.Contains(checks[2].Detail, "v0.33.0") {
		t.Fatalf("image resolver detail must name the client version: %q", checks[2].Detail)
	}
}

// The Compose plugin was previously asserted only by the deploy step, so
// `ob preflight` and `ob bootstrap` both reported a host ready that `ob deploy`
// then refused.
func TestCheckHostPrerequisitesFailsOnMissingCompose(t *testing.T) {
	f := prerequisiteFake(map[string]transport.Result{
		"docker compose version": {ExitCode: 125, Stderr: "docker: 'compose' is not a docker command"},
	})
	checks, err := CheckHostPrerequisites(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if check.Name != PrerequisiteCompose {
			continue
		}
		if check.OK || check.Remedy == "" || !strings.Contains(check.Detail, "not a docker command") {
			t.Fatalf("compose check = %+v, want a failure carrying the reason and a remedy", check)
		}
		return
	}
	t.Fatalf("no compose check in %+v", checks)
}

func TestCheckHostPrerequisitesFailsOnIncompatibleBuildx(t *testing.T) {
	f := prerequisiteFake(map[string]transport.Result{
		"imagetools inspect --help": {Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n"},
	})
	checks, err := CheckHostPrerequisites(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	last := checks[len(checks)-1]
	if last.Name != PrerequisiteResolver || last.OK || !strings.Contains(last.Detail, "v0.33.0") {
		t.Fatalf("resolver check = %+v, want a failure naming the client version", last)
	}
}

// Without a runtime the remaining answers are noise. Asking anyway produces a
// cascade of failures that hides the one an operator has to fix.
func TestCheckHostPrerequisitesShortCircuitsWithoutRuntime(t *testing.T) {
	f := prerequisiteFake(map[string]transport.Result{
		"docker version": {ExitCode: 127, Stderr: "docker: command not found"},
	})
	checks, err := CheckHostPrerequisites(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Name != PrerequisiteRuntime || checks[0].OK {
		t.Fatalf("checks = %+v, want only the runtime failure", checks)
	}
	for _, cmd := range f.Commands {
		if strings.Contains(cmd, "compose") || strings.Contains(cmd, "buildx") {
			t.Fatalf("a missing runtime must not be followed by %q", cmd)
		}
	}
}

func TestRequireHostPrerequisitesNamesTheUnmetOne(t *testing.T) {
	f := prerequisiteFake(map[string]transport.Result{
		"docker compose version": {ExitCode: 125, Stderr: "docker: 'compose' is not a docker command"},
	})
	err := RequireHostPrerequisites(context.Background(), f)
	if err == nil {
		t.Fatal("a missing Compose plugin must refuse")
	}
	if !strings.Contains(err.Error(), PrerequisiteCompose) || !strings.Contains(err.Error(), composeRemedy) {
		t.Fatalf("error = %v, want the prerequisite name and its remedy", err)
	}
	if err := RequireHostPrerequisites(context.Background(), prerequisiteFake(nil)); err != nil {
		t.Fatalf("a satisfied host must not refuse: %v", err)
	}
}
