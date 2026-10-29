package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/onebox"
	"github.com/labstack/onebox/internal/transport"
	"github.com/labstack/onebox/internal/ui"
	"github.com/spf13/cobra"
)

// Use the real planner and executor; only the SSH/host boundary is simulated.
func cliNoOpDeployPlan(t *testing.T) (string, string, onebox.DeployPlan, *transport.Fake) {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "ob.yml")
	project := `apiVersion: onebox.run/v1alpha2
kind: Application
metadata: {name: demo}
spec:
  environments:
    production: {server: deploy@example.invalid}
  workloads:
    temporal: {image: 'example/app@sha256:` + strings.Repeat("1", 64) + `', strategy: Recreate}
    ui: {image: 'example/app@sha256:` + strings.Repeat("2", 64) + `', strategy: Recreate}
    gate: {role: Job, image: 'example/app@sha256:` + strings.Repeat("3", 64) + `', command: 'true', deploymentPhase: PreRelease, dataEffect: None}
  deployment: {order: [temporal, ui]}
`
	if err := os.WriteFile(config, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	liveCompose, livePayload, containers := "services: {}\n", strings.Repeat("0", 64), ""
	fake := &transport.Fake{HostName: "example.invalid", TargetName: "deploy@example.invalid"}
	fake.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "/_host/owner"):
			return transport.Result{Stdout: "demo production\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/R0\n"}, true
		case strings.Contains(command, "docker ps") && strings.Contains(command, "--format"):
			return transport.Result{Stdout: containers}, true
		case strings.Contains(command, "docker ps -q"):
			return transport.Result{Stdout: "C1\n"}, true
		case strings.Contains(command, "docker inspect") && strings.Contains(command, "{{.Image}}"):
			return transport.Result{Stdout: "sha256:" + strings.Repeat("a", 64)}, true
		case strings.Contains(command, "cat ") && strings.Contains(command, "compose.yaml"):
			return transport.Result{Stdout: liveCompose}, true
		case strings.Contains(command, "find . -type f"):
			return transport.Result{Stdout: livePayload + "\n"}, true
		case command == app.BuildxCapabilityCommand:
			return transport.Result{Stdout: "Usage: docker buildx imagetools inspect [OPTIONS] NAME\n      --format string\n"}, true
		case strings.HasPrefix(command, ": onebox-epoch-probe;"):
			return transport.Result{ExitCode: app.ProbeAbsent}, true
		}
		return transport.Result{}, false
	}
	previous := cliConnector
	cliConnector = func(context.Context, transport.Route) (transport.Transport, error) { return fake, nil }
	t.Cleanup(func() { cliConnector = previous })
	svc := onebox.New(onebox.Options{ConfigPath: config, Connect: cliConnector})
	first, err := svc.PlanDeploy(t.Context(), onebox.PlanDeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	liveCompose, livePayload = first.Artifact.RenderedCompose, first.Operation.Binding.PayloadDigest
	for _, step := range first.Operation.Steps {
		if step.Kind == onebox.StepWorkloadRelease {
			containers += fmt.Sprintf("%s|%s|R0|%s|Up (healthy)\n", step.Service, step.Service, step.Revision)
		}
	}
	plan, err := svc.PlanDeploy(t.Context(), onebox.PlanDeployRequest{})
	if err != nil || !plan.NoOp {
		t.Fatalf("plan unchanged runtime: no_op=%t err=%v", plan.NoOp, err)
	}
	path := filepath.Join(dir, "plan.json")
	if err := plan.Save(path); err != nil {
		t.Fatal(err)
	}
	fake.Commands = nil
	return config, path, plan, fake
}

func TestNoOpDeployReportsOutcome(t *testing.T) {
	for _, mode := range []string{"saved human", "interactive human", "json", "ndjson"} {
		t.Run(mode, func(t *testing.T) {
			config, path, _, fake := cliNoOpDeployPlan(t)
			var stdout, stderr bytes.Buffer
			root := newRootCmd()
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			args := []string{"--config", config, "deploy"}
			if mode != "interactive human" {
				args = append(args, "--plan", path)
			}
			if mode == "json" || mode == "ndjson" {
				args = append(args, "--output", mode)
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("deploy: %v\nstderr: %s", err, &stderr)
			}
			if strings.HasSuffix(mode, "human") {
				if !strings.Contains(stdout.String(), "\n✓ nothing to deploy — R0 is current (`--redeploy` forces a fresh roll)\n") {
					t.Errorf("missing standalone no-op outcome:\n%s", &stdout)
				}
			} else {
				lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
				body := stdout.Bytes()
				if mode == "ndjson" {
					body = lines[len(lines)-1]
				}
				var envelope cliEnvelope
				if err := json.Unmarshal(body, &envelope); err != nil || envelope.Outcome != cliOutcomeNoOp {
					t.Errorf("no-op envelope = %+v, decode=%v\n%s", envelope, err, &stdout)
				}
			}
			if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
				t.Errorf("no-op transferred a release: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
			}
			for _, command := range fake.Commands {
				if strings.Contains(command, "--force-recreate") || strings.Contains(command, " run --rm") || strings.Contains(command, "ln -sfn") {
					t.Errorf("no-op mutated application runtime: %s", command)
				}
			}
		})
	}
}

func TestNoOpPlanShowsRedeployAsConditional(t *testing.T) {
	_, _, plan, _ := cliNoOpDeployPlan(t)
	// This list is also persisted in the sealed JSON plan, so the condition must
	// be established by the planner, not just added by the human CLI renderer.
	commands := strings.Join(plan.Artifact.Commands, "\n")
	if !strings.HasPrefix(commands, "without --redeploy: no changes — R0 remains current\nwith --redeploy only:\n") {
		t.Errorf("no-op plan lists unconditional runtime mutations:\n%s", commands)
	}
	for _, command := range plan.Artifact.Commands[2:] {
		if !strings.HasPrefix(command, "  ") {
			t.Errorf("command escaped the conditional redeploy branch: %q", command)
		}
	}
	for _, workload := range []string{"temporal", "ui"} {
		if !strings.Contains(commands, "  release "+workload+" (recreate):") {
			t.Errorf("missing conditional fresh roll for %s:\n%s", workload, commands)
		}
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	renderDeployPlan(cmd, ui.New(&out, false), plan)
	if !strings.Contains(out.String(), "operation (with --redeploy only):") {
		t.Errorf("no-op graph does not state its execution condition:\n%s", &out)
	}
}
