// Package compose loads the user's compose file through compose-go — the same
// loader docker compose v2 uses — so the supported dialect is exactly what
// Compose accepts; Onebox does not reimplement the Compose specification.
package compose

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/config"
)

// DrainFile: the generated/wrapped healthcheck fails while this file exists,
// which is how the proxy is told to stop routing to a container (the
// traffic-shift protocol, "poison its health state").
const DrainFile = "/tmp/ob-drain"

var ident = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func Load(ctx context.Context, composePath, projectName string, envFiles ...string) (*types.Project, error) {
	return load(ctx, composePath, projectName, false, envFiles...)
}

// LoadLenient loads for READ-ONLY verbs (status, logs, exec, audit): they
// never consume interpolated values, so a missing required variable
// (`${VAR:?...}`) must not block a query — it resolves to the visible
// placeholder `${VAR}` instead of an error. Deploy-path verbs keep the strict
// contract via Load.
func LoadLenient(ctx context.Context, composePath, projectName string, envFiles ...string) (*types.Project, error) {
	return load(ctx, composePath, projectName, true, envFiles...)
}

func load(ctx context.Context, composePath, projectName string, lenient bool, envFiles ...string) (*types.Project, error) {
	fns := []cli.ProjectOptionsFn{
		cli.WithName(projectName),
		cli.WithWorkingDirectory(filepath.Dir(composePath)),
		cli.WithOsEnv,
		// Onebox models the complete production contract. Profile-gated jobs and
		// maintenance services still need classification and may be explicitly
		// targeted by the engine, so load every declared profile.
		cli.WithProfiles([]string{"*"}),
		// WithEnvFiles(envFiles...) feeds ${VAR} INTERPOLATION (image tags, etc.)
		// from the config's env_files; with none it falls back to <project-dir>/.env.
		// WithDotEnv reads them. Order matters: os env is merged first and wins
		// (compose semantics), and later env files override earlier ones.
		cli.WithEnvFiles(envFiles...),
		cli.WithDotEnv,
		// Do NOT fold `env_file:` into each service's `environment:` map. That
		// folding would inline the entire secret env file into the rendered
		// compose (and thus the plan diff/artifact), violating "secrets content
		// never logged". Interpolation of ${VAR} in the Compose
		// file itself is unaffected; env_file references survive and are shipped
		// as a mode-600 payload file that `docker compose` reads at runtime.
		cli.WithoutEnvironmentResolution,
	}
	if lenient {
		fns = append(fns, cli.WithLoadOptions(func(o *loader.Options) {
			// this mutator also runs for LoadConfigFiles' zero Options (no
			// interpolation there) — only wrap the real interpolation pass
			if o.Interpolate == nil {
				return
			}
			// Per-VARIABLE leniency via an overlay loop: substitute strictly;
			// when a ${VAR:?}/${VAR?} requirement fails (unset OR set-but-empty
			// — both error), overlay exactly that variable with the visible
			// placeholder ${VAR} and retry. Every other variable keeps exact
			// compose semantics (:- defaults, :+ presence, nested defaults) —
			// a whole-string fallback mapping would corrupt them, and the
			// template regex is too greedy for per-match interception (a
			// default value swallows following ${...} into one match).
			o.Interpolate.Substitute = func(s string, m template.Mapping) (string, error) {
				overlay := map[string]string{}
				wrapped := func(key string) (string, bool) {
					if v, ok := overlay[key]; ok {
						return v, true
					}
					return m(key)
				}
				for {
					out, err := template.Substitute(s, wrapped)
					if err == nil {
						return out, nil
					}
					var missing *template.MissingRequiredError
					if errors.As(err, &missing) {
						if _, seen := overlay[missing.Variable]; !seen {
							overlay[missing.Variable] = "${" + missing.Variable + "}"
							continue // one failing variable per pass; loop is bounded by distinct vars
						}
					}
					return out, err // non-required error, or no progress
				}
			}
		}))
	}
	opts, err := cli.NewProjectOptions([]string{composePath}, fns...)
	if err != nil {
		return nil, err
	}
	p, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("compose load %s: %w", composePath, err)
	}
	return p, nil
}

// Classify verifies every Compose service has exactly one class
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
			return fmt.Errorf("components.%s: Compose has no service %q", name, r.Service)
		}
		if err := claim(r.Service, "workload"); err != nil {
			return err
		}
	}
	for _, a := range cfg.Accessories {
		if _, ok := p.Services[a]; !ok {
			return fmt.Errorf("supporting/data component: Compose has no service %q", a)
		}
		if err := claim(a, "supporting/data service"); err != nil {
			return err
		}
	}
	for _, j := range cfg.Jobs {
		if _, ok := p.Services[j]; !ok {
			return fmt.Errorf("job component: Compose has no service %q", j)
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
		return fmt.Errorf("every Compose service needs exactly one component classification; unclassified: %v", orphans)
	}
	for name, component := range cfg.Components {
		if component.Persistence == nil || len(component.Persistence.Volumes) == 0 {
			continue
		}
		service := p.Services[component.Service]
		mounted := map[string]bool{}
		for _, volume := range service.Volumes {
			if volume.Type == types.VolumeTypeVolume && volume.Source != "" {
				mounted[volume.Source] = true
			}
		}
		for _, volume := range component.Persistence.Volumes {
			if !mounted[volume] {
				return fmt.Errorf("components.%s.persistence.volumes: named volume %q is not mounted by Compose service %q", name, volume, component.Service)
			}
		}
	}
	return nil
}

// CheckRollable enforces overlap preconditions on services that run more
// than one container — every rolling role (which briefly runs two) and any role
// with replicas > 1 (which runs N). Such a service can't carry a fixed
// container_name or a published host port, and a rolling one must gate on a
// healthcheck.
func CheckRollable(p *types.Project, cfg *config.Config) []error {
	var errs []error
	for roleName, r := range cfg.Roles {
		svc, ok := p.Services[r.Service]
		if !ok {
			continue // Classify reports this
		}
		// managed proxy reaches workloads via the injected ingress network;
		// network_mode (host/container:) excludes `networks:` entirely, so
		// the proxy could never route to this workload — refuse at validate
		if cfg.Proxy.Managed && svc.NetworkMode != "" {
			errs = append(errs, fmt.Errorf("components.%s (%q): network_mode %q conflicts with the managed proxy — workloads must join the shared ingress network", roleName, r.Service, svc.NetworkMode))
		}
		multi := r.Mode == "rolling" || r.Count() > 1
		if !multi {
			continue
		}
		if svc.ContainerName != "" {
			errs = append(errs, fmt.Errorf("components.%s (%q): container_name forbids running multiple copies — remove it", roleName, r.Service))
		}
		for _, port := range svc.Ports {
			if port.Published != "" {
				errs = append(errs, fmt.Errorf("components.%s (%q): host port %s:%d — copies cannot share a host port; route via the proxy instead", roleName, r.Service, port.Published, port.Target))
			}
		}
		if svc.Deploy != nil && svc.Deploy.Replicas != nil {
			errs = append(errs, fmt.Errorf("components.%s (%q): deploy.replicas conflicts with Onebox-managed scaling — use components.%s.deployment.replicas", roleName, r.Service, roleName))
		}
		// readiness rule: rolling gates on a healthcheck — from
		// ready.http/exec, or ADOPTED from the compose file's own
		if r.Mode == "rolling" {
			hasReadyKind := r.Ready != nil && (r.Ready.HTTP != "" || r.Ready.Exec != "")
			hasComposeHC := adoptedProbe(svc) != ""
			if !hasReadyKind && !hasComposeHC {
				errs = append(errs, fmt.Errorf("components.%s (%q): rolling requires readiness.http/exec, or an executable healthcheck in the Compose file to adopt", roleName, r.Service))
			}
		}
	}
	return errs
}
