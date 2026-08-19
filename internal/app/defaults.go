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
		// Enabling the backup requirement made this field mandatory, and its
		// absence produced an untyped complaint about an empty duration for a
		// field the author had never heard of. A default is the answer the
		// evolution rules already allow.
		if e.Policy.Migrations.RequireBackup && e.Policy.Migrations.BackupMaximumAge == "" {
			e.Policy.Migrations.BackupMaximumAge = "24h"
			mark(path + ".policy.migrations.backup_maximum_age")
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
		// An HTTP probe with no port of its own probes the port the workload
		// routes on. Without this it probes port 0 — the shorthand
		// `health: /healthz` carries a path and nothing else — and the check
		// can never pass, so a rolling release waits out its whole budget and
		// reports the container unhealthy. The failure names the container and
		// says nothing about the port, which is the hardest kind to place.
		if w.Health != nil && w.Health.HTTP != "" && w.Health.Port == 0 {
			if port := w.ProbePort(); port != 0 {
				w.Health.Port = port
				mark(path + ".health.port")
			}
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
		if w.IsJob() && w.When == "" {
			w.When = "manual"
			mark(path + ".when")
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
		for i := range w.PublishedPorts {
			pp := indexed(path+".published_ports", i)
			// Bound to loopback unless stated. A published port on every
			// interface is how a database ends up on the internet.
			if w.PublishedPorts[i].Bind == "" {
				w.PublishedPorts[i].Bind = "127.0.0.1"
				mark(pp + ".bind")
			}
			if w.PublishedPorts[i].Protocol == "" {
				w.PublishedPorts[i].Protocol = "tcp"
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
		path := "services." + name
		if s.Persistence != nil && s.Persistence.Mode == "" {
			s.Persistence.Mode = "durable"
			mark(path + ".persistence.mode")
		}
		if s.Backup != nil {
			if s.Backup.Schedule.Cron == "" {
				s.Backup.Schedule.Cron = "0 2 * * *"
				mark(path + ".backup.schedule.cron")
			}
			if s.Backup.Schedule.Timezone == "" {
				s.Backup.Schedule.Timezone = "UTC"
				mark(path + ".backup.schedule.timezone")
			}
			if s.Backup.Retention.MinimumGenerations == 0 && !stated(raw, path+".backup.retention.keep") {
				s.Backup.Retention.MinimumGenerations = 7
				mark(path + ".backup.retention.keep")
			}
			if s.Backup.Retention.RecoveryWindow == "" {
				s.Backup.Retention.RecoveryWindow = "7d"
				mark(path + ".backup.retention.window")
			}
			if s.Backup.RestoreDrill.Schedule.Cron == "" {
				s.Backup.RestoreDrill.Schedule.Cron = "0 3 * * 0,3"
				mark(path + ".backup.drill.schedule.cron")
			}
			if s.Backup.RestoreDrill.Schedule.Timezone == "" {
				s.Backup.RestoreDrill.Schedule.Timezone = "UTC"
				mark(path + ".backup.drill.schedule.timezone")
			}
			if s.Backup.RestoreDrill.ProofMaximumAge == "" {
				s.Backup.RestoreDrill.ProofMaximumAge = "7d"
				mark(path + ".backup.drill.maximum_age")
			}
		}
		p.Services[name] = s
	}

	for name := range p.BackupTargets {
		target := p.BackupTargets[name]
		path := "backup_targets." + name
		if target.TLS == "" {
			target.TLS = "verify"
			mark(path + ".tls")
		}
		if target.Credentials.Provider == "" {
			target.Credentials.Provider = "sops"
			mark(path + ".credentials.provider")
		}
		p.BackupTargets[name] = target
	}

	for name := range p.ExternalServices {
		external := p.ExternalServices[name]
		path := "external_services." + name
		if external.Connection.Source.Provider == "" {
			external.Connection.Source.Provider = "sops"
			mark(path + ".connection.source.provider")
		}
		if external.Probe != nil {
			if external.Probe.Kind == "" {
				external.Probe.Kind = "driver-health"
				mark(path + ".probe.kind")
			}
			if external.Probe.Timeout == "" {
				external.Probe.Timeout = "5s"
				mark(path + ".probe.timeout")
			}
			if external.Probe.MaximumAge == "" {
				external.Probe.MaximumAge = "5m"
				mark(path + ".probe.maximum_age")
			}
		}
		p.ExternalServices[name] = external
	}

	for name := range p.Notifications {
		n := p.Notifications[name]
		if n.Format == "" {
			n.Format = "text"
			mark("notifications." + name + ".format")
		}
		// A webhook with no events declared matched no outcome and never
		// fired, which reads as configured and does nothing.
		if n.On == nil {
			n.On = append([]string(nil), eNotifyEvent...)
			mark("notifications." + name + ".on")
		}
		p.Notifications[name] = n
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
