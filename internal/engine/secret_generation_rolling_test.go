package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

// web declares a health check, so it defaults to rolling; worker stays a
// recreate workload, which is what keeps the two paths visible in one push.
const rollingGenerationProject = `api_version: onebox.run/v1
app: shop
base_path: /srv/onebox
environments:
  production: {server: deploy@example.invalid}
workloads:
  web:
    image: nginx
    port: 3000
    domain: shop.example.com
    health: {exec: ["/health"]}
    env_files: [{file: web.enc.env, provider: sops}]
  worker:
    role: worker
    image: nginx
    env_files: [{file: worker.enc.env, provider: sops}]
`

type rollingGenerationState struct {
	generations   map[string]string
	worker        string
	sequence      int
	newcomerNever bool
}

// newRollingGenerationFake models the surge protocol for web: each --scale
// creates one container on the generation the compose path names, and the
// existing one keeps running until it is drained and removed.
func newRollingGenerationFake(t *testing.T) (*transport.Fake, *rollingGenerationState) {
	t.Helper()
	state := &rollingGenerationState{
		generations: map[string]string{"W1": oldSecretGeneration, "K1": oldSecretGeneration},
		worker:      "K1",
		sequence:    1,
	}
	fake := &transport.Fake{HostName: "example.invalid", TargetName: "deploy@example.invalid"}
	lastField := func(s string) string {
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return ""
		}
		return strings.Trim(fields[len(fields)-1], ";'\"")
	}
	// Engine commands are fence-wrapped — `if [ ... ]; then docker rm W1; else
	// ...` — so an id lifted straight out of one carries the trailing
	// semicolon.
	token := func(s string) string { return strings.Trim(s, ";'\"") }
	fake.Dynamic = func(command string) (transport.Result, bool) {
		removed := map[string]bool{}
		drained := map[string]bool{}
		names := map[string]string{"W1": "shop-web-1"}
		for _, previous := range fake.Commands {
			if i := strings.Index(previous, "docker rm -f "); i >= 0 {
				removed[token(strings.Fields(previous[i+len("docker rm -f "):])[0])] = true
			} else if i := strings.Index(previous, "docker rm "); i >= 0 {
				removed[token(strings.Fields(previous[i+len("docker rm "):])[0])] = true
			}
			if i := strings.Index(previous, "docker exec "); i >= 0 && strings.Contains(previous, "touch") {
				drained[token(strings.Fields(previous[i+len("docker exec "):])[0])] = true
			}
			if i := strings.Index(previous, "docker rename "); i >= 0 {
				fields := strings.Fields(previous[i+len("docker rename "):])
				if len(fields) >= 2 {
					names[token(fields[0])] = token(fields[1])
				}
			}
		}
		webIDs := func(generation string) []string {
			var ids []string
			for id, gen := range state.generations {
				// generations also holds the worker's containers.
				if !strings.HasPrefix(id, "W") || removed[id] || (generation != "" && gen != generation) {
					continue
				}
				ids = append(ids, id)
			}
			// deterministic: W1 before W2
			for i := 0; i < len(ids); i++ {
				for j := i + 1; j < len(ids); j++ {
					if ids[j] < ids[i] {
						ids[i], ids[j] = ids[j], ids[i]
					}
				}
			}
			return ids
		}
		switch {
		case strings.Contains(command, "_host/owner"):
			return transport.Result{Stdout: "shop\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/20260809-120000-current\n"}, true
		case strings.Contains(command, "/ob.snapshot.yml"):
			return transport.Result{Stdout: rollingGenerationProject}, true
		case strings.HasPrefix(strings.TrimSpace(command), "cat ") && strings.Contains(command, "/compose.yaml"):
			return transport.Result{Stdout: currentGenerationCompose(oldSecretGeneration)}, true
		case strings.Contains(command, "cmp -s"):
			return transport.Result{ExitCode: 1}, true
		case strings.Contains(command, "Config.Healthcheck"):
			return transport.Result{Stdout: `{"Test":` + guardedHealthcheck + `,"Interval":5000000000,"Retries":3}` + "\n"}, true
		case strings.Contains(command, "docker compose") && strings.Contains(command, " up -d "):
			generation := generationFromEngineSecretCommand(command)
			if generationWorkload(command) == "worker" {
				state.sequence++
				state.worker = fmt.Sprintf("K%d", state.sequence)
				state.generations[state.worker] = generation
				return transport.Result{}, true
			}
			state.sequence++
			id := fmt.Sprintf("W%d", state.sequence)
			state.generations[id] = generation
			return transport.Result{}, true
		case strings.Contains(command, "docker ps -aq") && strings.Contains(command, "status=exited"):
			return transport.Result{Stdout: "\n"}, true
		case strings.Contains(command, "compose.service='worker'"):
			if removed[state.worker] {
				return transport.Result{Stdout: "\n"}, true
			}
			return transport.Result{Stdout: state.worker + "\n"}, true
		case strings.Contains(command, "compose.service='web'"):
			generation := ""
			if i := strings.Index(command, "ob.secret-generation='"); i >= 0 {
				generation = command[i+len("ob.secret-generation='"):]
				generation = generation[:strings.IndexByte(generation, '\'')]
			}
			return transport.Result{Stdout: strings.Join(webIDs(generation), "\n") + "\n"}, true
		case strings.Contains(command, "ob.secret-generation"):
			id := lastField(command)
			if generation, ok := state.generations[id]; ok {
				return transport.Result{Stdout: generation + "\n"}, true
			}
			return transport.Result{ExitCode: 1}, true
		case strings.Contains(command, "State.Health"):
			id := lastField(command)
			if state.generations[id] == newSecretGeneration && state.newcomerNever {
				return transport.Result{Stdout: "starting\n"}, true
			}
			if drained[id] {
				return transport.Result{Stdout: "unhealthy\n"}, true
			}
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(command, "State.Status"):
			return transport.Result{Stdout: "running\n"}, true
		case strings.Contains(command, "{{.Name}}"):
			id := lastField(command)
			if n := names[id]; n != "" {
				return transport.Result{Stdout: "/" + n + "\n"}, true
			}
			return transport.Result{Stdout: "/shop-web-compose-default\n"}, true
		}
		return transport.Result{}, false
	}
	return fake, state
}

// upCommandsFor picks the `compose up` invocations for one service. Engine
// commands are fence-wrapped, so the service name is not at the end of the line.
func upCommandsFor(commands []string, svc string) []string {
	var out []string
	for _, command := range commands {
		if !strings.Contains(command, " up -d ") {
			continue
		}
		if strings.Contains(command, " "+svc+";") || strings.HasSuffix(strings.TrimSpace(command), " "+svc) {
			out = append(out, command)
		}
	}
	return out
}

func rollingGenerationEngine(t *testing.T, fake *transport.Fake, output *bytes.Buffer) *Engine {
	t.Helper()
	resolved := resolvedSecretGraph(t, rollingGenerationProject)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	return New(resolved, nil, fake, Options{
		Environment: "production", Out: output, Sleep: noSleep,
		Now:              func() time.Time { now = now.Add(time.Second); return now },
		SecretGeneration: func() (string, error) { return newSecretGeneration, nil },
	})
}

// A rolling workload must rotate its secret the way it takes a release. It used
// to be force-recreated: every replica destroyed before the first health check.
func TestSecretGenerationRollsRollingWorkloads(t *testing.T) {
	fake, state := newRollingGenerationFake(t)
	var output bytes.Buffer
	engine := rollingGenerationEngine(t, fake, &output)
	if _, err := engine.SecretsPushBatch(context.Background(), generationPayloads()); err != nil {
		t.Fatalf("push: %v\n%s", err, strings.Join(fake.Commands, "\n"))
	}
	commands := strings.Join(fake.Commands, "\n")
	webUp := upCommandsFor(fake.Commands, "web")
	if len(webUp) == 0 {
		t.Fatalf("web was never replaced:\n%s", commands)
	}
	for _, command := range webUp {
		if strings.Contains(command, "--force-recreate") {
			t.Fatalf("rolling workload was force-recreated:\n%s", command)
		}
		if !strings.Contains(command, "--no-recreate") || !strings.Contains(command, "--scale web=") {
			t.Fatalf("web was not surged one replica at a time:\n%s", command)
		}
	}
	// The old replica keeps serving until the newcomer is healthy, so it must
	// be drained rather than destroyed alongside it.
	if !strings.Contains(commands, "docker exec W1") {
		t.Fatalf("old replica was not drained:\n%s", commands)
	}
	// worker declares no health check, so it stays a recreate workload.
	if !strings.Contains(commands, "--force-recreate") {
		t.Fatalf("recreate workloads must keep their contract:\n%s", commands)
	}
	if state.generations["W2"] != newSecretGeneration {
		t.Fatalf("newcomer did not adopt the generation: %#v", state.generations)
	}
}

// The reported outage: the replacement never becomes healthy. Before, every
// replica was already gone by then. The surviving replica is the whole point.
func TestSecretGenerationRollingKeepsServingReplicaWhenNewcomerNeverHealthy(t *testing.T) {
	fake, state := newRollingGenerationFake(t)
	state.newcomerNever = true
	var output bytes.Buffer
	engine := rollingGenerationEngine(t, fake, &output)
	if _, err := engine.SecretsPushBatch(context.Background(), generationPayloads()); err == nil {
		t.Fatalf("unhealthy newcomer must fail the push:\n%s", strings.Join(fake.Commands, "\n"))
	}
	commands := strings.Join(fake.Commands, "\n")
	if !strings.Contains(commands, "docker rm -f W2") {
		t.Fatalf("unhealthy newcomer was not removed:\n%s", commands)
	}
	if strings.Contains(commands, "docker rm -f W1") || strings.Contains(commands, "docker rm W1") {
		t.Fatalf("the serving replica was destroyed:\n%s", commands)
	}
	if state.generations["W1"] != oldSecretGeneration {
		t.Fatalf("survivor left its generation: %#v", state.generations)
	}
}

// Recovery lands in forceSecretGeneration too. After a roll that removed its
// own newcomer, every replica is already on the old generation and there is
// nothing to replace — which the identity check used to report as a failure.
func TestForceSecretGenerationIsNoOpWhenAlreadyConverged(t *testing.T) {
	fake, _ := newRollingGenerationFake(t)
	var output bytes.Buffer
	engine := rollingGenerationEngine(t, fake, &output)
	checkpoint, err := release.NewSecretCheckpoint(
		"20260809-120000-current", oldSecretGeneration, newSecretGeneration,
		[]string{"web", "worker"},
		[]string{".ob-decrypted-sops-web.enc.env", ".ob-decrypted-sops-worker.enc.env"},
		time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	before := len(fake.Commands)
	if err := engine.forceSecretGeneration(context.Background(), checkpoint, "web", oldSecretGeneration); err != nil {
		t.Fatalf("converged workload reported a failure: %v", err)
	}
	for _, command := range fake.Commands[before:] {
		if strings.Contains(command, " up -d ") || strings.Contains(command, "docker rm") {
			t.Fatalf("converged workload was replaced anyway:\n%s", command)
		}
	}
}

// An unreadable generation label is not evidence that a workload needs
// replacing. Answering "not converged" there would let a transport failure or
// a broken inspect fall through into mutating containers whose state could not
// be established.
func TestForceSecretGenerationRefusesWhenTheLabelCannotBeRead(t *testing.T) {
	fake, _ := newRollingGenerationFake(t)
	inner := fake.Dynamic
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "ob.secret-generation") && strings.HasSuffix(strings.TrimSpace(command), "W1") {
			return transport.Result{ExitCode: 1, Stderr: "no such object"}, true
		}
		return inner(command)
	}
	var output bytes.Buffer
	engine := rollingGenerationEngine(t, fake, &output)
	checkpoint, err := release.NewSecretCheckpoint(
		"20260809-120000-current", oldSecretGeneration, newSecretGeneration,
		[]string{"web", "worker"},
		[]string{".ob-decrypted-sops-web.enc.env", ".ob-decrypted-sops-worker.enc.env"},
		time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	before := len(fake.Commands)
	err = engine.forceSecretGeneration(context.Background(), checkpoint, "web", newSecretGeneration)
	if err == nil || !strings.Contains(err.Error(), "read secret generation label") {
		t.Fatalf("unreadable label = %v, want a refusal naming the read", err)
	}
	for _, command := range fake.Commands[before:] {
		if strings.Contains(command, " up -d ") || strings.Contains(command, "docker rm") {
			t.Fatalf("containers were mutated despite an unreadable label:\n%s", command)
		}
	}
}
