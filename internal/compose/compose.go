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

func Load(ctx context.Context, composePath, projectName string) (*types.Project, error) {
	opts, err := cli.NewProjectOptions(
		[]string{composePath},
		cli.WithName(projectName),
		cli.WithWorkingDirectory(filepath.Dir(composePath)),
		cli.WithOsEnv,
		cli.WithDotEnv,
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

// CheckRollable enforces design §03 rolling preconditions on rolled services.
func CheckRollable(p *types.Project, cfg *config.Config) []error {
	var errs []error
	for roleName, r := range cfg.Roles {
		if r.Mode != "rolling" {
			continue
		}
		svc, ok := p.Services[r.Service]
		if !ok {
			continue // Classify reports this
		}
		if svc.ContainerName != "" {
			errs = append(errs, fmt.Errorf("roles.%s (%q): container_name forbids running two copies — remove it", roleName, r.Service))
		}
		for _, port := range svc.Ports {
			if port.Published != "" {
				errs = append(errs, fmt.Errorf("roles.%s (%q): host port %s:%d — two containers cannot share a host port; route via the proxy instead", roleName, r.Service, port.Published, port.Target))
			}
		}
		if svc.Deploy != nil && svc.Deploy.Replicas != nil {
			errs = append(errs, fmt.Errorf("roles.%s (%q): deploy.replicas conflicts with yeet-managed scaling", roleName, r.Service))
		}
		// readiness rule (design §03): rolling gates on a healthcheck — from
		// ready.http/exec, or ADOPTED from the compose file's own
		hasReadyKind := r.Ready != nil && (r.Ready.HTTP != "" || r.Ready.Exec != "")
		hasComposeHC := svc.HealthCheck != nil && len(svc.HealthCheck.Test) > 0
		if !hasReadyKind && !hasComposeHC {
			errs = append(errs, fmt.Errorf("roles.%s (%q): rolling requires ready.http/exec, or a healthcheck in the compose file to adopt", roleName, r.Service))
		}
	}
	return errs
}
