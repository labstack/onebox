package project

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// fakeRunner answers commands from a script and records what was run, so a test
// can assert that preflight only ever reads.
type fakeRunner struct {
	answers map[string]transport.Result
	ran     []string
	fail    bool
}

func (f *fakeRunner) Run(_ context.Context, cmd string) (transport.Result, error) {
	f.ran = append(f.ran, cmd)
	if f.fail {
		return transport.Result{}, context.DeadlineExceeded
	}
	// Longest pattern wins. Matching in map order made "docker network" and
	// "docker network inspect" race, which would have been a flaky test rather
	// than an honest one.
	best, found := "", false
	var out transport.Result
	for pattern, res := range f.answers {
		if strings.Contains(cmd, pattern) && len(pattern) > len(best) {
			best, out, found = pattern, res, true
		}
	}
	if found {
		return out, nil
	}
	return transport.Result{ExitCode: 0}, nil
}

func healthyRunner() *fakeRunner {
	return &fakeRunner{answers: map[string]transport.Result{
		"docker version": {Stdout: "27.1.1\n"},
		"docker ps":      {Stdout: ""},
		"docker volume":  {Stdout: ""},
		"docker network": {Stdout: "ob-ingress\t\n"},
	}}
}

const preflightProject = `api_version: onebox.run/v1
app: ledger
environments:
  production: {server: root@1.2.3.4}
workloads:
  web:
    role: application
    image: nginx
    domain: ledger.example.com
    port: 8080
    volumes: [uploads]
`

func preflight(t *testing.T, run Runner, yaml string) *Report {
	t.Helper()
	p, err := LoadBytes([]byte(yaml), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.Preflight(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestPreflightPassesOnAReadyHost(t *testing.T) {
	rep := preflight(t, healthyRunner(), preflightProject)
	if !rep.OK() {
		t.Fatalf("expected a ready host to pass: %+v", rep.Failures())
	}
}

// TestPreflightOnlyReads is the promise the phase is named for. A preflight
// that mutates is not a preflight, and the failure it would cause — a partial
// change before anything was approved — is exactly what the split prevents.
func TestPreflightOnlyReads(t *testing.T) {
	run := healthyRunner()
	preflight(t, run, preflightProject)

	mutating := []string{
		"docker run", "docker create", "docker rm", "docker start", "docker stop",
		"docker compose up", "docker volume create", "docker network create",
		"mkdir", "rm ", "touch ", "docker rename", "> ",
	}
	for _, cmd := range run.ran {
		for _, m := range mutating {
			if strings.Contains(cmd, m) {
				t.Errorf("preflight ran a mutating command: %q", cmd)
			}
		}
	}
	if len(run.ran) == 0 {
		t.Fatal("preflight asked the target nothing")
	}
}

// TestPreflightReportsEveryProblem: a caller should see all of it at once
// rather than fixing one thing and running again.
func TestPreflightReportsEveryProblem(t *testing.T) {
	run := healthyRunner()
	run.answers["-w"] = transport.Result{ExitCode: 1, Stdout: "/var/lib\n"}
	run.answers["docker ps"] = transport.Result{Stdout: "ledger_web\t\n"}
	run.answers["docker network inspect"] = transport.Result{ExitCode: 1}

	rep := preflight(t, run, preflightProject)
	if rep.OK() {
		t.Fatal("expected failures")
	}
	if len(rep.Failures()) < 3 {
		t.Fatalf("expected the base path, the collision and the network to all be reported, got %d: %+v",
			len(rep.Failures()), rep.Failures())
	}
	for _, c := range rep.Failures() {
		if c.Remedy == "" {
			t.Errorf("check %q reports a problem with no remedy", c.Name)
		}
	}
}

// TestPreviousReleaseIsNotACollision: a name held by this application is the
// normal case. Treating it as a conflict would make the second deploy fail.
func TestPreviousReleaseIsNotACollision(t *testing.T) {
	run := healthyRunner()
	run.answers["docker ps"] = transport.Result{Stdout: "ledger_web\tledger\n"}

	rep := preflight(t, run, preflightProject)
	for _, c := range rep.Failures() {
		if c.Name == "name collisions" {
			t.Fatalf("our own previous release was reported as a collision: %s", c.Detail)
		}
	}
}

// TestForeignHolderIsACollision is the other half: something Onebox did not
// create must never be adopted silently.
func TestForeignHolderIsACollision(t *testing.T) {
	run := healthyRunner()
	run.answers["docker volume"] = transport.Result{Stdout: "ob_ledger_web_uploads\t\n"}

	rep := preflight(t, run, preflightProject)
	var found bool
	for _, c := range rep.Failures() {
		if c.Name == "name collisions" && strings.Contains(c.Detail, "ob_ledger_web_uploads") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a foreign volume holding a derived name should be a collision: %+v", rep.Checks)
	}
}

// TestRuntimeFailureShortCircuits: without a container runtime every other
// check would fail too, and a wall of consequences hides the cause.
func TestRuntimeFailureShortCircuits(t *testing.T) {
	run := healthyRunner()
	run.answers["docker version"] = transport.Result{ExitCode: 127, Stderr: "docker: command not found"}

	rep := preflight(t, run, preflightProject)
	if len(rep.Checks) != 1 {
		t.Fatalf("expected only the runtime check, got %d: %+v", len(rep.Checks), rep.Checks)
	}
	if rep.Checks[0].Remedy == "" {
		t.Error("the runtime failure should say what to do about it")
	}
}

// TestUnreachableTargetIsAnError, not a failed check: nothing was learned.
func TestUnreachableTargetIsAnError(t *testing.T) {
	p, err := LoadBytes([]byte(preflightProject), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := p.Resolve("production")
	_, err = r.Preflight(context.Background(), &fakeRunner{fail: true})
	var e *Error
	if !asError(err, &e) || e.Code != "target_unreachable" {
		t.Fatalf("got %v, want target_unreachable", err)
	}
}
