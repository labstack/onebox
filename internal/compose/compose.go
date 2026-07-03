// Package compose loads the user's compose file through compose-go — the same
// loader docker compose v2 uses — so the supported dialect is exactly what
// compose accepts (design rev 5: never re-implement the spec).
package compose

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/config"
)

// DrainFile: the generated/wrapped healthcheck fails while this file exists,
// which is how the proxy is told to stop routing to a container (rev 5
// traffic-shift protocol, "poison its health state").
const DrainFile = "/tmp/yeet-drain"

var ident = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func Load(ctx context.Context, composePath, projectName string, envFiles ...string) (*types.Project, error) {
	opts, err := cli.NewProjectOptions(
		[]string{composePath},
		cli.WithName(projectName),
		cli.WithWorkingDirectory(filepath.Dir(composePath)),
		cli.WithOsEnv,
		// WithEnvFiles(envFiles...) feeds ${VAR} INTERPOLATION (image tags, etc.)
		// from the config's env_files; with none it falls back to <project-dir>/.env.
		// WithDotEnv reads them. Order matters: os env is merged first and wins
		// (compose semantics), and later env files override earlier ones.
		cli.WithEnvFiles(envFiles...),
		cli.WithDotEnv,
		// Do NOT fold `env_file:` into each service's `environment:` map. That
		// folding would inline the entire secret env file into the rendered
		// compose (and thus the plan diff/artifact), violating "secrets content
		// never logged" (design §07). Interpolation of ${VAR} in the compose
		// file itself is unaffected; env_file references survive and are shipped
		// as a mode-600 payload file that `docker compose` reads at runtime.
		cli.WithoutEnvironmentResolution,
	)
	if err != nil {
		return nil, err
	}
	p, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("compose load %s: %w", composePath, err)
	}
	return p, nil
}

// Classify verifies every compose service has exactly one class (design §03)
// and that all names are shell-safe identifiers (command-injection rule).
func Classify(p *types.Project, cfg *config.Config) error {
	for name := range p.Services {
		if !ident.MatchString(name) {
			return fmt.Errorf("service name %q is not identifier-safe (%s)", name, ident)
		}
	}
	class := map[string]string{}
	claim := func(svc, cls string) error {
		if prev, ok := class[svc]; ok {
			return fmt.Errorf("service %q claimed as both %s and %s", svc, prev, cls)
		}
		class[svc] = cls
		return nil
	}
	for name, r := range cfg.Roles {
		if _, ok := p.Services[r.Service]; !ok {
			return fmt.Errorf("roles.%s: compose has no service %q", name, r.Service)
		}
		if err := claim(r.Service, "role"); err != nil {
			return err
		}
	}
	for _, a := range cfg.Accessories {
		if _, ok := p.Services[a]; !ok {
			return fmt.Errorf("accessories: compose has no service %q", a)
		}
		if err := claim(a, "accessory"); err != nil {
			return err
		}
	}
	for _, j := range cfg.Jobs {
		if _, ok := p.Services[j]; !ok {
			return fmt.Errorf("jobs: compose has no service %q", j)
		}
		if err := claim(j, "job"); err != nil {
			return err
		}
	}
	var orphans []string
	for name := range p.Services {
		if _, ok := class[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		return fmt.Errorf("every service needs exactly one class (role|accessory|job); unclassified: %v", orphans)
	}
	return nil
}

// CheckRollable enforces design §03 preconditions on services that run more
// than one container — every rolling role (which briefly runs two) and any role
// with replicas > 1 (which runs N). Such a service can't carry a fixed
// container_name or a published host port, and a rolling one must gate on a
// healthcheck.
func CheckRollable(p *types.Project, cfg *config.Config) []error {
	var errs []error
	for roleName, r := range cfg.Roles {
		multi := r.Mode == "rolling" || r.Count() > 1
		if !multi {
			continue
		}
		svc, ok := p.Services[r.Service]
		if !ok {
			continue // Classify reports this
		}
		if svc.ContainerName != "" {
			errs = append(errs, fmt.Errorf("roles.%s (%q): container_name forbids running multiple copies — remove it", roleName, r.Service))
		}
		for _, port := range svc.Ports {
			if port.Published != "" {
				errs = append(errs, fmt.Errorf("roles.%s (%q): host port %s:%d — copies cannot share a host port; route via the proxy instead", roleName, r.Service, port.Published, port.Target))
			}
		}
		if svc.Deploy != nil && svc.Deploy.Replicas != nil {
			errs = append(errs, fmt.Errorf("roles.%s (%q): deploy.replicas conflicts with yeet-managed scaling — use replicas: in yeet.yml", roleName, r.Service))
		}
		// readiness rule (design §03): rolling gates on a healthcheck — from
		// ready.http/exec, or ADOPTED from the compose file's own
		if r.Mode == "rolling" {
			hasReadyKind := r.Ready != nil && (r.Ready.HTTP != "" || r.Ready.Exec != "")
			hasComposeHC := svc.HealthCheck != nil && len(svc.HealthCheck.Test) > 0
			if !hasReadyKind && !hasComposeHC {
				errs = append(errs, fmt.Errorf("roles.%s (%q): rolling requires ready.http/exec, or a healthcheck in the compose file to adopt", roleName, r.Service))
			}
		}
	}
	return errs
}
