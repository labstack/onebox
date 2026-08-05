package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	ctypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"

	"github.com/labstack/onebox/internal/compose"
)

// wellKnownServices matches supporting infrastructure that should be
// converged, not released. Data services are classified more specifically by
// classify before this fallback is used.
var wellKnownServices = regexp.MustCompile(
	"(^|[[:space:]/_-])(memcached|rabbitmq|kafka|minio|traefik|ofelia|dozzle|nats|etcd|typesense|nginx|caddy)([[:space:]:@/_-]|$)")

var (
	postgresService = regexp.MustCompile("(^|[[:space:]/_-])postgres(?:ql)?([[:space:]:@/_-]|$)")
	mysqlService    = regexp.MustCompile("(^|[[:space:]/_-])(mysql|mariadb)([[:space:]:@/_-]|$)")
	redisService    = regexp.MustCompile("(^|[[:space:]/_-])(redis|valkey)([[:space:]:@/_-]|$)")
	workerService   = regexp.MustCompile("(^|[[:space:]_/-])(worker|sidekiq|celery|taskiq|rq)([[:space:]_/-]|$)")
	httpHealthcheck = regexp.MustCompile("https?://(localhost|127\\.0\\.0\\.1|\\[::1\\]):([0-9]+)(/[^[:space:]\"']*)")
)

func addInitCommand(root *cobra.Command, g *globalFlags) {
	root.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "scaffold ob.yml from the compose file + rollability doctor",
		Long:  "Scaffold `ob.yml` from the Compose file already in this repository.\n\nA starting point, not permission to deploy: read what it inferred about\nroles, persistence, health and job data effects before planning. Writes only\nin this repository and contacts nothing.",
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
	application := sanitizeApp(filepath.Base(mustAbs(dir)))
	p, err := compose.Load(ctx, filepath.Join(dir, composePath), application)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	var workloads, dataServices, services, jobs []string
	types := map[string]string{}
	rolling := map[string]bool{}
	for name, svc := range p.Services {
		typ := classify(name, svc)
		types[name] = typ
		switch typ {
		case "postgres", "mysql", "redis":
			dataServices = append(dataServices, name)
		case "service":
			services = append(services, name)
		case "job":
			jobs = append(jobs, name)
		case "application", "worker":
			workloads = append(workloads, name)
			rolling[name] = hasUsableHealthcheck(svc) || hasTraefikLabels(svc)
		}
	}
	sort.Strings(workloads)
	sort.Strings(dataServices)
	sort.Strings(services)
	sort.Strings(jobs)
	componentNames := append([]string{}, workloads...)
	componentNames = append(componentNames, dataServices...)
	componentNames = append(componentNames, services...)
	componentNames = append(componentNames, jobs...)
	sort.Strings(componentNames)

	var b strings.Builder
	// The schema reference first, so an editor offers completion and inline
	// errors from the moment the file exists rather than after someone finds
	// out it could.
	fmt.Fprintf(&b, "# yaml-language-server: $schema=%s\n", app.SchemaID)
	b.WriteString("api_version: onebox.run/v1\n")
	fmt.Fprintf(&b, "app: %s\n", application)
	b.WriteString("environments:\n  production:\n    server: deploy@CHANGE-ME\n")
	b.WriteString("workloads:\n")
	for _, name := range componentNames {
		typ := types[name]
		role := map[string]string{
			"application": "application", "worker": "worker", "job": "job",
		}[typ]
		if role == "" {
			// Everything else in a Compose file is a long-running process
			// Onebox does not route: a database, a cache, a sidecar. The
			// declaration says what it is; nothing is guessed from the image.
			role = "daemon"
		}
		fmt.Fprintf(&b, "  %s:\n    role: %s\n", name, role)
		// The workload keeps referencing the Compose service it came from, so
		// adoption changes nothing about how it runs on the first deploy. Move
		// fields into the declaration when you want Onebox to own them.
		fmt.Fprintf(&b, "    compose: %q\n", composePath+"#"+name)
		switch role {
		case "application", "worker":
			strategy := "recreate"
			if rolling[name] {
				strategy = "rolling"
			}
			fmt.Fprintf(&b, "    strategy: %s\n", strategy)
			if strategy == "rolling" {
				path, port, ok := inferHTTPReadiness(p.Services[name])
				switch {
				case ok:
					fmt.Fprintf(&b, "    health: { http: %s, port: %d }\n", path, port)
				case p.Services[name].HealthCheck == nil:
					b.WriteString("    health: { http: /healthz, port: CHANGE-ME }\n")
				}
			}
		case "job":
			effect := "unknown"
			if isMigration(name, p.Services[name]) {
				effect = "migration"
			}
			fmt.Fprintf(&b, "    data_effect: %s\n", effect)
		case "daemon":
			// Durability is scaffolded from what the image is, not from
			// whether a volume happens to be declared. A Postgres written
			// down as ephemeral is a data-loss default, and the operator
			// reading the scaffold is the one who would have to notice.
			mode := "ephemeral"
			switch {
			case typ == "postgres" || typ == "mysql":
				mode = "durable"
			case len(p.Services[name].Volumes) > 0:
				mode = "durable"
			}
			fmt.Fprintf(&b, "    persistence: { mode: %s }\n", mode)
		}
	}
	if len(workloads) > 0 {
		b.WriteString("deployment:\n")
		fmt.Fprintf(&b, "  order: [%s]\n", strings.Join(workloads, ", "))
	}
	if err := os.WriteFile(g.ConfigPath, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s (%s, %s, %s, %s)\n\n", g.ConfigPath,
		countLabel(len(workloads), "workload"), countLabel(len(dataServices), "data service"),
		countLabel(len(services), "supporting service"), countLabel(len(jobs), "job"))

	// The doctor reports the exact compose delta each rolling candidate needs.
	fmt.Fprintln(out, "rollability doctor:")
	clean := true
	for _, name := range workloads {
		if !rolling[name] {
			continue
		}
		svc := p.Services[name]
		if svc.ContainerName != "" {
			clean = false
			fmt.Fprintf(out, "  %s: remove `container_name: %s` — two copies can't share a name\n", name, svc.ContainerName)
		}
		for _, port := range svc.Ports {
			if port.Published != "" {
				clean = false
				fmt.Fprintf(out, "  %s: unbind host port %s:%d — the proxy routes instead; two containers can't share a host port\n", name, port.Published, port.Target)
			}
		}
		if svc.Deploy != nil && svc.Deploy.Replicas != nil {
			clean = false
			fmt.Fprintf(out, "  %s: remove `deploy.replicas` — ob manages scale during rolls\n", name)
		}
	}
	if clean {
		fmt.Fprintln(out, "  no blockers — rolling components are deploy-ready")
	}
	fmt.Fprintln(out, "\nnext: fill in CHANGE-ME values, then `ob validate`")
	return nil
}

func classify(name string, svc ctypes.ServiceConfig) string {
	if isMigration(name, svc) {
		return "job"
	}
	haystack := strings.ToLower(name + " " + svc.Image)
	switch {
	case postgresService.MatchString(haystack):
		return "postgres"
	case mysqlService.MatchString(haystack):
		return "mysql"
	case redisService.MatchString(haystack):
		return "redis"
	case wellKnownServices.MatchString(haystack):
		return "service"
	case workerService.MatchString(strings.ToLower(name + " " + strings.Join(svc.Command, " "))):
		return "worker"
	default:
		return "application"
	}
}

func isMigration(name string, svc ctypes.ServiceConfig) bool {
	command := strings.ToLower(strings.Join(svc.Command, " "))
	return name == "migrate" || strings.Contains(command, "migrate") ||
		strings.Contains(command, "upgrade head")
}

func inferHTTPReadiness(svc ctypes.ServiceConfig) (string, int, bool) {
	if svc.HealthCheck == nil {
		return "", 0, false
	}
	match := httpHealthcheck.FindStringSubmatch(strings.Join(svc.HealthCheck.Test, " "))
	if len(match) != 4 {
		return "", 0, false
	}
	port, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, false
	}
	return match[3], port, true
}

func hasUsableHealthcheck(svc ctypes.ServiceConfig) bool {
	if svc.HealthCheck == nil || len(svc.HealthCheck.Test) < 2 {
		return false
	}
	return svc.HealthCheck.Test[0] == "CMD" || svc.HealthCheck.Test[0] == "CMD-SHELL"
}

func hasTraefikLabels(svc ctypes.ServiceConfig) bool {
	for k := range svc.Labels {
		if strings.HasPrefix(k, "traefik.") {
			return true
		}
	}
	return false
}

func countLabel(count int, singular string) string {
	label := singular
	if count != 1 {
		label += "s"
	}
	return fmt.Sprintf("%d %s", count, label)
}

var unsafeApp = regexp.MustCompile("[^a-z0-9-]")

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
