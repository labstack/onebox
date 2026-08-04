package app

import "strings"

// Defaults are applied by explicit assignment, after decoding and before
// validation.
//
// The previous implementation declared them beside the fields, which reads
// better and produced a real defect: a default on an optional field never
// materialised, so values the contract documented as present were silently
// absent. An assignment cannot fail that way — it either runs or it does not,
// and the canonical form shows which.
//
// Everything set here is recorded as derived, so `ob canonical` distinguishes a
// value someone wrote from one that appeared because nobody did. That
// distinction is the whole point of printing the canonical form.

func applyDefaults(p *Spec, raw map[string]any, derived map[string]Origin) {
	mark := func(path string) { derived[path] = OriginDefault }

	if p.BasePath == "" {
		p.BasePath = DefaultBasePath
		mark("base_path")
	}
	if p.Deployment.RetainReleases == 0 {
		p.Deployment.RetainReleases = 5
		mark("deployment.retain_releases")
	}
	if p.Deployment.MigrationPolicy == "" {
		p.Deployment.MigrationPolicy = "manual"
		mark("deployment.migration_policy")
	}
	if p.Proxy.Kind == "" {
		p.Proxy.Kind = "traefik-docker"
		mark("proxy.kind")
	}
	if p.Proxy.Network == "" {
		p.Proxy.Network = IngressNetwork
		mark("proxy.network")
	}

	for name := range p.Environments {
		e := p.Environments[name]
		path := "environments." + name
		// Approval and agent proposals default on: a policy that has to be
		// asked for is a policy nobody has.
		// A boolean needs the document to distinguish "false" from "absent",
		// which the decoded value cannot: both are false.
		if !stated(raw, path+".policy.require_approval") {
			e.Policy.RequireApproval = true
			mark(path + ".policy.require_approval")
		}
		if !stated(raw, path+".policy.allow_agent_proposals") {
			e.Policy.AllowAgentProposals = true
			mark(path + ".policy.allow_agent_proposals")
		}
		p.Environments[name] = e
	}

	for name := range p.Workloads {
		w := p.Workloads[name]
		path := "workloads." + name

		// Only when absent. `replicas: 0` is something someone wrote, and
		// defaulting it to 1 would turn a refusal into a silent correction.
		if w.Replicas == 0 && !stated(raw, path+".replicas") {
			w.Replicas = 1
			mark(path + ".replicas")
		}
		if w.Strategy == "" {
			w.Strategy = w.Mode()
			mark(path + ".strategy")
		}
		if w.Image != nil && w.Image.Pull == "" {
			w.Image.Pull = "missing"
			mark(path + ".image.pull")
		}
		if w.Drain != nil && w.Drain.Signal == "" {
			w.Drain.Signal = "TERM"
			mark(path + ".drain.signal")
		}
		if w.IsJob() && w.Run == "" {
			w.Run = "manual"
			mark(path + ".run")
		}
		if w.Schedule != nil && w.Schedule.Timezone == "" {
			w.Schedule.Timezone = "UTC"
			mark(path + ".schedule.timezone")
		}
		for i := range w.Routes {
			rp := indexed(path+".routes", i)
			if w.Routes[i].Path == "" {
				w.Routes[i].Path = "/"
				mark(rp + ".path")
			}
			if w.Routes[i].Entrypoint == "" {
				w.Routes[i].Entrypoint = "websecure"
				mark(rp + ".entrypoint")
			}
			if w.Routes[i].Protocol == "" {
				w.Routes[i].Protocol = "http"
				mark(rp + ".protocol")
			}
			if w.Routes[i].Scheme == "" {
				w.Routes[i].Scheme = "http"
				mark(rp + ".scheme")
			}
			if w.Routes[i].TLS == "" {
				w.Routes[i].TLS = "terminate"
				mark(rp + ".tls")
			}
		}
		for i := range w.Volumes {
			if w.Volumes[i].Mode == "" {
				w.Volumes[i].Mode = "rw"
				mark(indexed(path+".volumes", i) + ".mode")
			}
		}
		for i := range w.Ports {
			pp := indexed(path+".ports", i)
			// Bound to loopback unless stated. A published port on every
			// interface is how a database ends up on the internet.
			if w.Ports[i].Bind == "" {
				w.Ports[i].Bind = "127.0.0.1"
				mark(pp + ".bind")
			}
			if w.Ports[i].Protocol == "" {
				w.Ports[i].Protocol = "tcp"
				mark(pp + ".protocol")
			}
		}
		if w.Persistence != nil && w.Persistence.Mode == "" {
			w.Persistence.Mode = "durable"
			mark(path + ".persistence.mode")
		}
		p.Workloads[name] = w
	}

	for name := range p.Services {
		s := p.Services[name]
		if s.Persistence != nil && s.Persistence.Mode == "" {
			s.Persistence.Mode = "durable"
			mark("services." + name + ".persistence.mode")
		}
		p.Services[name] = s
	}

	for name := range p.Notifications {
		n := p.Notifications[name]
		if n.Format == "" {
			n.Format = "text"
			mark("notifications." + name + ".format")
		}
		p.Notifications[name] = n
	}
	for name := range p.Secrets {
		s := p.Secrets[name]
		if s.Provider == "" {
			s.Provider = "sops"
			mark("secrets." + name + ".provider")
		}
		p.Secrets[name] = s
	}
	// Every remaining default is the zero value — advisory false, migration
	// backup not required — and needs no assignment. A check that cannot fail
	// a deploy has to be asked for; the alternative is a suite that proves
	// nothing.
}

// stated reports whether the document wrote a value at this path, which is the
// only way to tell a declared false from an absent one.
func stated(raw map[string]any, path string) bool {
	cur := any(raw)
	for _, part := range splitPath(path) {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[part]
		if !ok {
			return false
		}
	}
	return true
}

func splitPath(path string) []string {
	var out []string
	for _, part := range strings.Split(path, ".") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
