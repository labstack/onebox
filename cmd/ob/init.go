package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	ctypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/compose"
)

// wellKnownAccessories matches images of stateful deps and infra that should
// never be rolled — converged, not released.
var wellKnownAccessories = regexp.MustCompile(
	`(^|/)(postgres|mysql|mariadb|redis|valkey|memcached|rabbitmq|kafka|minio|traefik|ofelia|dozzle|nats|etcd|typesense|nginx|caddy)([:@-]|$)`)

func addInitCommand(root *cobra.Command, g *globalFlags) {
	root.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "scaffold ob.yml from the compose file + rollability doctor",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), cmd, g)
		},
	})
}

func runInit(ctx context.Context, cmd *cobra.Command, g *globalFlags) error {
	if _, err := os.Stat(g.ConfigPath); err == nil {
		return fmt.Errorf("%s already exists — init refuses to overwrite", g.ConfigPath)
	}
	dir := filepath.Dir(g.ConfigPath)
	composePath := ""
	for _, cand := range []string{"docker-compose.yaml", "docker-compose.yml", "compose.yaml", "compose.yml"} {
		if _, err := os.Stat(filepath.Join(dir, cand)); err == nil {
			composePath = cand
			break
		}
	}
	if composePath == "" {
		return fmt.Errorf("no compose file found in %s", dir)
	}
	app := sanitizeApp(filepath.Base(mustAbs(dir)))
	p, err := compose.Load(ctx, filepath.Join(dir, composePath), app)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	var roles, accessories, jobs []string
	rolling := map[string]bool{}
	for name, svc := range p.Services {
		switch classify(name, svc) {
		case "accessory":
			accessories = append(accessories, name)
		case "job":
			jobs = append(jobs, name)
		default:
			roles = append(roles, name)
			rolling[name] = svc.HealthCheck != nil || hasTraefikLabels(svc)
		}
	}
	sort.Strings(roles)
	sort.Strings(accessories)
	sort.Strings(jobs)

	var b strings.Builder
	fmt.Fprintf(&b, "app: %s\ncompose: %s\n", app, composePath)
	b.WriteString("environments:\n  production: { hosts: [deploy@CHANGE-ME] }\n")
	b.WriteString("roles:\n")
	for _, r := range roles {
		if rolling[r] {
			fmt.Fprintf(&b, "  %s: { service: %s, mode: rolling, ready: { http: /healthz, port: CHANGE-ME } }\n", r, r)
		} else {
			fmt.Fprintf(&b, "  %s: { service: %s, mode: recreate }\n", r, r)
		}
	}
	fmt.Fprintf(&b, "order: [%s]\n", strings.Join(roles, ", "))
	if len(accessories) > 0 {
		fmt.Fprintf(&b, "accessories: [%s]\n", strings.Join(accessories, ", "))
	}
	if len(jobs) > 0 {
		fmt.Fprintf(&b, "jobs: [%s]\n", strings.Join(jobs, ", "))
		fmt.Fprintf(&b, "hooks: { migrate: docker compose run --rm --no-deps %s }\n", jobs[0])
	}
	if err := os.WriteFile(g.ConfigPath, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s (%d roles, %d accessories, %d jobs)\n\n", g.ConfigPath, len(roles), len(accessories), len(jobs))

	// the doctor: the exact compose delta each rolling candidate needs
	fmt.Fprintln(out, "rollability doctor:")
	clean := true
	for _, r := range roles {
		if !rolling[r] {
			continue
		}
		svc := p.Services[r]
		if svc.ContainerName != "" {
			clean = false
			fmt.Fprintf(out, "  %s: remove `container_name: %s` — two copies can't share a name\n", r, svc.ContainerName)
		}
		for _, port := range svc.Ports {
			if port.Published != "" {
				clean = false
				fmt.Fprintf(out, "  %s: unbind host port %s:%d — the proxy routes instead; two containers can't share a host port\n", r, port.Published, port.Target)
			}
		}
		if svc.Deploy != nil && svc.Deploy.Replicas != nil {
			clean = false
			fmt.Fprintf(out, "  %s: remove `deploy.replicas` — ob manages scale during rolls\n", r)
		}
	}
	if clean {
		fmt.Fprintln(out, "  no blockers — rolling roles are deploy-ready")
	}
	fmt.Fprintln(out, "\nnext: fill in CHANGE-ME values, then `ob validate`")
	return nil
}

func classify(name string, svc ctypes.ServiceConfig) string {
	command := strings.Join(svc.Command, " ")
	if name == "migrate" || strings.Contains(command, "migrate") ||
		strings.Contains(command, "upgrade head") {
		return "job"
	}
	if wellKnownAccessories.MatchString(svc.Image) {
		return "accessory"
	}
	return "role"
}

func hasTraefikLabels(svc ctypes.ServiceConfig) bool {
	for k := range svc.Labels {
		if strings.HasPrefix(k, "traefik.") {
			return true
		}
	}
	return false
}

var unsafeApp = regexp.MustCompile(`[^a-z0-9-]`)

func sanitizeApp(s string) string {
	s = unsafeApp.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "app-" + s
	}
	return s
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}
