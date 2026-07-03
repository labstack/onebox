package compose

import (
	"fmt"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/config"
)

// InjectSecretsEnv adds the rendered secrets env file to every role service
// (part of the declared, closed injection set). Call before Render.
func InjectSecretsEnv(p *types.Project, cfg *config.Config, relPath string) {
	for _, r := range cfg.Roles {
		svc, ok := p.Services[r.Service]
		if !ok {
			continue
		}
		svc.EnvFiles = append(svc.EnvFiles, types.EnvFile{Path: relPath, Required: true})
		p.Services[r.Service] = svc
	}
}

// Render produces the per-release deployable (design §02): the user's compose
// project plus a CLOSED injection set — yeet.* labels, a drain-guarded
// healthcheck, and (when declared) the secrets env file — applied to ROLE
// services only.
func Render(p *types.Project, cfg *config.Config, releaseID string) ([]byte, error) {
	for _, r := range cfg.Roles {
		svc, ok := p.Services[r.Service]
		if !ok {
			return nil, fmt.Errorf("compose has no service %q", r.Service)
		}
		if svc.Labels == nil {
			svc.Labels = types.Labels{}
		}
		svc.Labels["yeet.app"] = cfg.App
		svc.Labels["yeet.release"] = releaseID

		orig := svc.HealthCheck
		adopted := adoptedProbe(svc)
		probe := adopted
		if r.Ready != nil && r.Ready.HTTP != "" {
			probe = fmt.Sprintf("curl -fsS http://localhost:%d%s || wget -qO- http://localhost:%d%s",
				r.Ready.Port, r.Ready.HTTP, r.Ready.Port, r.Ready.HTTP)
		} else if r.Ready != nil && r.Ready.Exec != "" {
			probe = r.Ready.Exec
		}
		if probe != "" {
			// timing: ready knobs win; an ADOPTED healthcheck keeps its own
			// tuning (the app author calibrated it); else defaults
			interval, start := 5*time.Second, 5*time.Second
			var retries *uint64
			if probe == adopted && orig != nil {
				if orig.Interval != nil {
					interval = time.Duration(*orig.Interval)
				}
				if orig.StartPeriod != nil {
					start = time.Duration(*orig.StartPeriod)
				}
				retries = orig.Retries
			}
			if r.Ready != nil && r.Ready.Interval > 0 {
				interval = time.Duration(r.Ready.Interval)
			}
			if r.Ready != nil && r.Ready.StartPeriod > 0 {
				start = time.Duration(r.Ready.StartPeriod)
			}
			iv, sp := types.Duration(interval), types.Duration(start)
			svc.HealthCheck = &types.HealthCheckConfig{
				// The drain guard: while DrainFile exists the check fails, the
				// proxy drops the container, and only THEN does yeet signal it.
				Test:        types.HealthCheckTest{"CMD-SHELL", fmt.Sprintf("test ! -f %s && ( %s )", DrainFile, probe)},
				Interval:    &iv,
				StartPeriod: &sp,
				Retries:     retries,
			}
		}
		p.Services[r.Service] = svc
	}
	return p.MarshalYAML()
}

// adoptedProbe extracts a shell-runnable probe from a user-authored
// healthcheck so the drain guard can wrap it (adopt-and-wrap, design §03).
func adoptedProbe(svc types.ServiceConfig) string {
	if svc.HealthCheck == nil || len(svc.HealthCheck.Test) == 0 {
		return ""
	}
	t := svc.HealthCheck.Test
	switch t[0] {
	case "CMD-SHELL":
		if len(t) > 1 {
			return t[1]
		}
	case "CMD":
		var parts []string
		for _, a := range t[1:] {
			parts = append(parts, fmt.Sprintf("%q", a))
		}
		return strings.Join(parts, " ")
	}
	return ""
}
