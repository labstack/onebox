package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/project"
	"github.com/labstack/onebox/internal/transport"
)

// `ob up` is the first execution path on the declarative contract.
//
// It is deliberately not `ob deploy`. Deploy is the engine's word and carries
// the engine's guarantees — lock, fence, journal, drain, rollback, verification
// — none of which exist on this path yet. Naming it deploy would claim them.
func addUpCommand(root *cobra.Command, g *globalFlags) {
	var (
		releaseID  string
		imageFlags []string
		wait       time.Duration
		skipHealth bool
		bootstrap  bool
	)

	cmd := &cobra.Command{
		Use:   "up",
		Short: "stage and start a release on the target (no lock, fence, journal or rollback yet)",
		Long: "Render the project, preflight the target, stage the release, bring it up and\n" +
			"wait for health.\n\n" +
			"This is not `deploy`. The execution engine's guarantees — a deploy lock,\n" +
			"runner fencing, an append-only journal, connection drain, automatic rollback\n" +
			"and release verification — are not on this path, and it reports that rather\n" +
			"than implying otherwise. Use it to prove a target works, not to run\n" +
			"something you cannot afford to break.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := project.Load(g.ConfigPath)
			if err != nil {
				return explain(err)
			}
			resolved, err := p.Resolve(g.Env)
			if err != nil {
				return explain(err)
			}

			env, ok := p.Environments[g.Env]
			if !ok {
				return fmt.Errorf("environment %q is not declared", g.Env)
			}
			addr := env.Server.Host
			if env.Server.User != "" {
				addr = env.Server.User + "@" + env.Server.Host
			}

			t, err := transport.NewSSHContext(cmd.Context(), addr)
			if err != nil {
				return fmt.Errorf("cannot reach %s: %w", addr, err)
			}
			defer t.Close()

			out := cmd.OutOrStdout()
			if bootstrap {
				if err := bootstrapTarget(cmd, t, resolved); err != nil {
					return err
				}
			}

			images := project.Images{}
			for _, pair := range imageFlags {
				name, ref, ok := strings.Cut(pair, "=")
				if !ok {
					return fmt.Errorf("--image expects workload=reference, got %q", pair)
				}
				images[name] = ref
			}
			if releaseID == "" {
				releaseID = time.Now().UTC().Format("20060102-150405")
			}

			result, err := resolved.Deploy(cmd.Context(), t, releaseID,
				project.DeployOptions{Images: images, Wait: wait, SkipHealth: skipHealth})
			if err != nil {
				return explain(err)
			}

			if g.Output == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(result); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(out, "release  %s\n", result.Release)
				fmt.Fprintf(out, "digest   %s\n", result.Digest[:16])
				fmt.Fprintf(out, "staged   %s\n", result.ReleaseDir)
				if len(result.Healthy) > 0 {
					fmt.Fprintf(out, "healthy  %s\n", strings.Join(result.Healthy, ", "))
				}
				if len(result.Unhealthy) > 0 {
					fmt.Fprintf(out, "UNHEALTHY %s\n", strings.Join(result.Unhealthy, ", "))
				}
				fmt.Fprintf(out, "\nnot done by this path: %s\n", strings.Join(result.Skipped, ", "))
			}

			if !result.OK() {
				return fmt.Errorf("%d workload(s) did not become healthy", len(result.Unhealthy))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&releaseID, "release", "", "release identity (default: UTC timestamp)")
	cmd.Flags().StringArrayVar(&imageFlags, "image", nil, "resolved image as workload=reference (repeatable)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "how long to wait for health (default 3m)")
	cmd.Flags().BoolVar(&skipHealth, "skip-health", false, "do not wait for health")
	cmd.Flags().BoolVar(&bootstrap, "bootstrap", false,
		"prepare a bare host first: install the container runtime and create the ingress network")
	root.AddCommand(cmd)
}

// bootstrapTarget prepares a host that has never run Onebox. It is the smallest
// thing that makes `up` work on a fresh server, not the host-provisioning
// capability — that is its own change, with its own guarantees.
func bootstrapTarget(cmd *cobra.Command, t *transport.SSH, r *project.Resolved) error {
	out := cmd.OutOrStdout()
	ctx := cmd.Context()

	steps := []struct{ name, cmd string }{
		{"container runtime", `command -v docker >/dev/null 2>&1 || ` +
			`(curl -fsSL https://get.docker.com | sh && systemctl enable --now docker)`},
		{"base path", fmt.Sprintf("mkdir -p %q", r.NamesFor(r.Env).AppDir())},
		{"ingress network", fmt.Sprintf(
			`docker network inspect %s >/dev/null 2>&1 || docker network create %s`,
			r.Proxy.Network, r.Proxy.Network)},
	}
	for _, s := range steps {
		fmt.Fprintf(out, "bootstrap: %s\n", s.name)
		res, err := t.Run(ctx, s.cmd)
		if err != nil {
			return fmt.Errorf("bootstrap %s: %w", s.name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("bootstrap %s: %s", s.name, strings.TrimSpace(res.Stderr))
		}
	}
	return nil
}
