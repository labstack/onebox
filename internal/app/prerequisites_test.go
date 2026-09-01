package app

import (
	"context"
	"errors"
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
	// Typed, so a structured caller branches on a code and is handed a command
	// rather than parsing the sentence.
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("refusal is not typed: %T", err)
	}
	if typed.Code != "host_prerequisite_unmet" || typed.Next != "ob preflight" {
		t.Fatalf("typed refusal = %+v, want the registered code and a diagnostic command", typed)
	}
	if _, known := ErrorCodeMeaning(typed.Code); !known {
		t.Fatalf("%q is not published in the error registry", typed.Code)
	}
	if err := RequireHostPrerequisites(context.Background(), prerequisiteFake(nil)); err != nil {
		t.Fatalf("a satisfied host must not refuse: %v", err)
	}
}

// A failing `docker version` has three ordinary causes and they need three
// different fixes. Telling an operator to install Docker on a host where Docker
// is installed and the account simply cannot reach the socket is worse than
// saying nothing: it sends them to reinstall something that is already there.
func TestRuntimeRemedyNamesTheActualCause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
		want   string
	}{
		{"absent", "docker: command not found", runtimeAbsentRemedy},
		{"denied", "permission denied while trying to connect to the Docker daemon socket", runtimeDeniedRemedy},
		{"daemon down", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", runtimeUnreachableRemedy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := prerequisiteFake(map[string]transport.Result{
				"docker version": {ExitCode: 1, Stderr: tc.stderr},
			})
			checks, err := CheckHostPrerequisites(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			if checks[0].Remedy != tc.want {
				t.Fatalf("remedy = %q, want %q", checks[0].Remedy, tc.want)
			}
			if checks[0].Detail == "" {
				t.Fatal("the runtime's own words must survive into the check")
			}
		})
	}
}

// Every remedy has to end in something the operator can do. A refusal that
// names only an abstraction is the dead end this guards against.
func TestEveryRemedyNamesAnAction(t *testing.T) {
	for _, remedy := range []string{runtimeAbsentRemedy, runtimeDeniedRemedy, runtimeUnreachableRemedy, composeRemedy, BuildxRemedy} {
		if !strings.Contains(remedy, "ob preflight") {
			t.Errorf("remedy does not say how to confirm the fix: %q", remedy)
		}
		for _, abstraction := range []string{"operator-managed provisioning", "configuration management"} {
			if strings.Contains(remedy, abstraction) {
				t.Errorf("remedy names an abstraction rather than an action: %q", remedy)
			}
		}
	}
	if !strings.Contains(runtimeAbsentRemedy, prerequisiteDocs) || !strings.Contains(composeRemedy, prerequisiteDocs) {
		t.Error("an install remedy must point at the documented procedure rather than synthesising one")
	}
}

// Some clients report the failure on stdout. Reading only the first line of an
// untrimmed stderr+stdout join silently dropped it: an empty stderr left a
// leading newline, the first line was "", and the detail arrived blank — which
// for the runtime also loses the cause that selects the remedy, so an operator
// whose account simply cannot reach the socket was told to install Docker.
func TestPrerequisiteDetailSurvivesOnStdout(t *testing.T) {
	f := prerequisiteFake(map[string]transport.Result{
		"docker version": {ExitCode: 1, Stdout: "permission denied while trying to connect to the Docker daemon socket"},
	})
	checks, err := CheckHostPrerequisites(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(checks[0].Detail, "permission denied") {
		t.Fatalf("detail = %q, want the reason the runtime printed", checks[0].Detail)
	}
	if checks[0].Remedy != runtimeDeniedRemedy {
		t.Fatalf("remedy = %q, want the permission fix rather than an install", checks[0].Remedy)
	}

	// A failure with nothing on either stream still has to say something.
	f = prerequisiteFake(map[string]transport.Result{"docker compose version": {ExitCode: 125}})
	checks, err = CheckHostPrerequisites(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(checks[1].Detail, "exited with status 125") {
		t.Fatalf("detail = %q, want the exit status when both streams are empty", checks[1].Detail)
	}
}
