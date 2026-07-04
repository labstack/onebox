package compose

import (
	"sort"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/config"
)

// Infer fills config fields that can be DERIVED from the compose project, so
// ob.yml states only what the spec cannot express. Every inferred value is a
// default: an explicit setting in ob.yml always wins. Every source is a
// compose-spec property (universal to any compose app) — nothing here is
// app-specific.
//
//   - classification: any service not named an accessory or job becomes a role
//     (keyed by its service name).
//   - mode: a role rolls if it CAN (no container_name / published host port /
//     deploy.replicas) AND has a healthcheck to gate on; otherwise recreate.
//   - order: depends_on topology among roles, alphabetical role name to break
//     ties (deterministic).
//
// Drain is deliberately NOT inferred: it is a choreography judgment (how long
// to bleed connections after de-routing, which signal to send), not something
// stop_grace_period expresses — docker already honors that grace on stop.
func Infer(cfg *config.Config, p *types.Project) {
	if cfg.Roles == nil {
		cfg.Roles = map[string]config.Role{}
	}
	claimed := map[string]bool{}
	for _, a := range cfg.Accessories {
		claimed[a] = true
	}
	for _, j := range cfg.Jobs {
		claimed[j] = true
	}
	// An explicit role stanza may omit `service`, defaulting it to the role key.
	for name, r := range cfg.Roles {
		if r.Service == "" {
			r.Service = name
			cfg.Roles[name] = r
		}
		claimed[r.Service] = true
	}
	// Auto-classify every remaining service as a role named after the service.
	var svcNames []string
	for name := range p.Services {
		svcNames = append(svcNames, name)
	}
	sort.Strings(svcNames)
	for _, name := range svcNames {
		if !claimed[name] {
			cfg.Roles[name] = config.Role{Service: name}
		}
	}
	// Per-role defaults from the service definition.
	for rn, r := range cfg.Roles {
		svc, ok := p.Services[r.Service]
		if !ok {
			continue // Classify reports the dangling reference
		}
		if r.Mode == "" {
			r.Mode = inferMode(svc)
		}
		cfg.Roles[rn] = r
	}
	if len(cfg.Order) == 0 {
		cfg.Order = inferOrder(cfg, p)
	}
}

// inferMode: rollable AND has a healthcheck ⇒ rolling; else recreate. Rollable
// mirrors CheckRollable's preconditions (two copies must be able to coexist and
// a readiness signal must exist to gate the join).
func inferMode(svc types.ServiceConfig) string {
	rollable := svc.ContainerName == ""
	for _, port := range svc.Ports {
		if port.Published != "" {
			rollable = false
		}
	}
	if svc.Deploy != nil && svc.Deploy.Replicas != nil {
		rollable = false
	}
	hasHC := svc.HealthCheck != nil && len(svc.HealthCheck.Test) > 0 && svc.HealthCheck.Test[0] != "NONE"
	if rollable && hasHC {
		return "rolling"
	}
	return "recreate"
}

// inferOrder topologically sorts roles by their services' depends_on edges,
// breaking ties by role name so the result is deterministic.
func inferOrder(cfg *config.Config, p *types.Project) []string {
	svcToRole := map[string]string{}
	for rn, r := range cfg.Roles {
		svcToRole[r.Service] = rn
	}
	// Edges between roles: role A depends on role B if A's service depends_on B's.
	deps := map[string]map[string]bool{} // role -> set of roles it depends on
	for rn, r := range cfg.Roles {
		deps[rn] = map[string]bool{}
		svc, ok := p.Services[r.Service]
		if !ok {
			continue
		}
		for dep := range svc.DependsOn {
			if drole, ok := svcToRole[dep]; ok && drole != rn {
				deps[rn][drole] = true
			}
		}
	}
	var order []string
	done := map[string]bool{}
	var names []string
	for rn := range cfg.Roles {
		names = append(names, rn)
	}
	sort.Strings(names)
	// Kahn-ish: repeatedly emit the alphabetically-first role whose deps are done.
	for len(order) < len(names) {
		progressed := false
		for _, rn := range names {
			if done[rn] {
				continue
			}
			ready := true
			for d := range deps[rn] {
				if !done[d] {
					ready = false
					break
				}
			}
			if ready {
				order = append(order, rn)
				done[rn] = true
				progressed = true
			}
		}
		if !progressed { // a cycle — emit the rest in name order, honestly incomplete
			for _, rn := range names {
				if !done[rn] {
					order = append(order, rn)
					done[rn] = true
				}
			}
		}
	}
	return order
}
