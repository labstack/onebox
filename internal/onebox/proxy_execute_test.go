package onebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

func TestExecuteBindsSealedProxyAction(t *testing.T) {
	for _, noOp := range []bool{false, true} {
		t.Run(map[bool]string{false: "deploy", true: "no-op"}[noOp], func(t *testing.T) {
			f := serviceFake()
			svc := newTestService(t, f)
			svc.now = func() time.Time { return time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC) }
			body, err := os.ReadFile(svc.configPath)
			if err != nil {
				t.Fatal(err)
			}
			body = []byte(strings.Replace(string(body), "  workloads:", "  proxy: {managed: true}\n  workloads:", 1))
			if err := os.WriteFile(svc.configPath, body, 0o600); err != nil {
				t.Fatal(err)
			}
			lp, err := svc.loadProject(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			staging := t.TempDir()
			spec := lp.resolved
			id, err := proxy.StageForAppManaged("", staging, spec.Proxy.Image, proxy.DiscoveryImage(svc.runner.Version),
				spec.Name, spec.Proxy.Network, spec.Proxy.Entrypoints, spec.HasExactTerminatingTLS(), spec.HasWildcardTerminatingTLS(), spec.Proxy.DNSChallenge)
			if err != nil {
				t.Fatal(err)
			}
			proxyCompose, err := os.ReadFile(filepath.Join(staging, "compose.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			drift := false
			liveCompose, payload := "", ""
			base := f.Dynamic
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				switch {
				case strings.Contains(cmd, "readlink") && !noOp:
					return transport.Result{}, true
				case cmd == "cat '/var/lib/onebox/_host/proxy/config.hash'":
					if drift {
						return transport.Result{ExitCode: 1}, true
					}
					return transport.Result{Stdout: id.String()}, true
				case strings.HasPrefix(cmd, "cat '/var/lib/onebox/_host/proxy/compose.yaml'"):
					return transport.Result{Stdout: string(proxyCompose)}, true
				case strings.Contains(cmd, "project='onebox-proxy'") && strings.Contains(cmd, "service=proxy"):
					return transport.Result{Stdout: "PX1"}, true
				case strings.Contains(cmd, "project='onebox-proxy'") && strings.Contains(cmd, "service=discovery"):
					return transport.Result{Stdout: "D1"}, true
				case strings.Contains(cmd, "stat -c %i"):
					return transport.Result{Stdout: "12"}, true
				case strings.HasPrefix(cmd, "docker image inspect -f '{{.Id}}'"):
					return transport.Result{Stdout: "sha256:" + strings.Repeat("ef", 32)}, true
				case strings.Contains(cmd, "cat ") && strings.Contains(cmd, "compose.yaml") && liveCompose != "":
					return transport.Result{Stdout: liveCompose}, true
				case strings.Contains(cmd, "find . -type f") && payload != "":
					return transport.Result{Stdout: payload}, true
				case strings.Contains(cmd, "docker version"):
					return transport.Result{Stdout: "27.0.3"}, true
				case strings.Contains(cmd, "compose version"):
					return transport.Result{Stdout: "2.29.1"}, true
				}
				return base(cmd)
			}
			plan, err := svc.PlanDeploy(context.Background(), PlanDeployRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if noOp {
				redacted := regexp.MustCompile("redacted:sha256:[0-9a-f]{12}")
				liveCompose = redacted.ReplaceAllString(plan.Artifact.RenderedCompose, testSecret)
				payload = plan.Operation.Binding.PayloadDigest
				plan, err = svc.PlanDeploy(context.Background(), PlanDeployRequest{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if plan.NoOp != noOp || plan.ProxyAction != "unchanged" {
				t.Fatalf("fixture plan: no-op=%v proxy=%q", plan.NoOp, plan.ProxyAction)
			}
			approval := approvalForTestPlan(t, &plan)
			drift = true
			f.Err = func(cmd string) error {
				if strings.Contains(cmd, "find '/var/lib/onebox/_host/proxy'") || strings.Contains(cmd, "mkdir -p '/var/lib/onebox/_host/proxy/acme'") {
					return errors.New("proxy mutation reached without the bound action")
				}
				return nil
			}
			_, err = svc.Execute(context.Background(), ExecuteRequest{Kind: KindDeploy, Plan: &plan, Approval: &approval})
			if err == nil || !strings.Contains(err.Error(), "unchanged") || !strings.Contains(err.Error(), "restart Traefik (configuration changed)") || !strings.Contains(err.Error(), "re-plan") {
				t.Fatalf("sealed proxy action must reach the engine on this execution path: %v", err)
			}
			if len(f.Uploads) != 0 {
				t.Fatalf("proxy drift reached release transfer: %v", f.Uploads)
			}
		})
	}
}
