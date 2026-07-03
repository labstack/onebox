package compose

import (
	"fmt"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/config"
)

// Render produces the per-release deployable (design §02): the user's compose
// project plus a CLOSED injection set — yeet.* labels and a drain-guarded
// healthcheck — applied to ROLE services only.
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

		probe := adoptedProbe(svc)
		if r.Ready != nil && r.Ready.HTTP != "" {
			probe = fmt.Sprintf("curl -fsS http://localhost:%d%s || wget -qO- http://localhost:%d%s",
				r.Ready.Port, r.Ready.HTTP, r.Ready.Port, r.Ready.HTTP)
		} else if r.Ready != nil && r.Ready.Exec != "" {
			probe = r.Ready.Exec
		}
		if probe != "" {
			interval := 5 * time.Second
			start := 5 * time.Second
			if r.Ready != nil {
				interval = time.Duration(r.Ready.Interval)
				start = time.Duration(r.Ready.StartPeriod)
			}
			if interval == 0 {
				interval = 5 * time.Second
			}
			if start == 0 {
				start = 5 * time.Second
			}
			iv, sp := types.Duration(interval), types.Duration(start)
			svc.HealthCheck = &types.HealthCheckConfig{
				// The drain guard: while DrainFile exists the check fails, the
				// proxy drops the container, and only THEN does yeet signal it.
				Test:        types.HealthCheckTest{"CMD-SHELL", fmt.Sprintf("test ! -f %s && ( %s )", DrainFile, probe)},
				Interval:    &iv,
				StartPeriod: &sp,
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
