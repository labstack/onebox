package engine

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

// injectedVerificationPath is a path the project grammar accepts. gURLPath
// refuses quotes, `$`, backticks, backslashes, spaces and control characters —
// but not `;`, `>` or `|`, which is enough to end one command and start another.
// The space-free spelling is deliberate: it is what makes this a value the
// loader lets through rather than one only this test can construct.
const injectedVerificationPath = "/healthz;id>/tmp/ob-owned"

// The engine builds the probe as one remote command. An authored path is data
// inside it, and data that ends up outside quoting is a command: a project file
// arriving through a pull request would run it as whatever account deploys.
//
// The check is the emitted command rather than an observed side effect. There is
// no shell in this test to run the payload, and asserting on quoting says
// precisely what must hold — the same property the neighbouring `exec` branch
// gets from q().
func TestVerifyHTTPPathIsQuotedIntoTheRemoteCommand(t *testing.T) {
	fake := happyFake()
	engine := New(verificationProject(t, injectedVerificationPath), testProject(t), fake,
		Options{Out: io.Discard, Sleep: noSleep})
	if err := engine.Verify(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}

	probe := commandContaining(t, fake.Commands, "curl -fsS")
	// The container address is assigned by the fake, so the assertion is about
	// the shape of the argument rather than its full text: the URL opens a quote
	// after the flags and the authored path is still inside it at the end.
	if !strings.Contains(probe, "curl -fsS -m 5 '") || !strings.HasSuffix(probe, injectedVerificationPath+"'") {
		t.Fatalf("verification probe does not carry the authored path as one quoted argument:\n%s", probe)
	}
}

// Quoting a value is the easiest way to accidentally stop sending it. This is
// the other half of the pair: an ordinary path still reaches the probe intact,
// on the workload's health port, so the fix above cannot pass by emitting a
// command that no longer probes anything.
func TestVerifyHTTPProbeStillCarriesThePortAndPath(t *testing.T) {
	fake := happyFake()
	engine := New(verificationProject(t, "/healthz"), testProject(t), fake,
		Options{Out: io.Discard, Sleep: noSleep})
	if err := engine.Verify(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	probe := commandContaining(t, fake.Commands, "curl -fsS")
	if !strings.Contains(probe, ":7500/healthz") {
		t.Fatalf("verification probe lost its address or path:\n%s", probe)
	}
}

// verificationProject loads a project whose single verification probes path
// over HTTP. It goes through the real loader so the test proves the value is one
// a project file may legally carry, not one only a struct literal can hold.
func verificationProject(t *testing.T, path string) *app.Resolved {
	t.Helper()
	spec, err := app.LoadBytes([]byte(`
api_version: onebox.run/v1
app: sample
environments:
  production:
    server: deploy@h
workloads:
  web:
    role: application
    image: ghcr.io/x/app:v2
    health: {http: /healthz, port: 7500, interval: 5s, start_period: 5s, within: 120s}
verifications:
  - workload: web
    http: `+path+`
`), "ob.yml")
	if err != nil {
		t.Fatalf("the project grammar rejected %q, so this path cannot reach the engine: %v", path, err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func commandContaining(t *testing.T, commands []string, substring string) string {
	t.Helper()
	for _, cmd := range commands {
		if strings.Contains(cmd, substring) {
			return cmd
		}
	}
	t.Fatalf("no command containing %q was issued; got %d commands", substring, len(commands))
	return ""
}
