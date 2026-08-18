package app

import "strings"

// Applying the constraints is explicit and typed, so the compiler knows when a
// field moves and a reader can see exactly which rule a field is held to. The
// grammars stay declarative in one table; only their application lives here.
//
// It runs after defaults, because a defaulted value is as much part of the
// runtime as a written one and has to be as valid.

func validateSpec(p *Spec) error {
	if err := checkAppName(p.Name); err != nil {
		return err
	}
	if err := gAbsPath.checkOptional("base_path", p.BasePath); err != nil {
		return err
	}

	for _, name := range sortedKeys(p.Environments) {
		// The name, not only the block under it. Every other map here checks its
		// key, and this one did not — which was harmless while the environment
		// was a lookup key and nothing else. It is now written into the host
		// owner record and read back through a grammar, so a name this validator
		// accepts and that parser rejects claims a host no later command can
		// operate: bootstrap writes the record, and every mutation after it
		// refuses an owner record it cannot parse.
		if err := gIdent.check("environments."+name, name); err != nil {
			return err
		}
		if err := validateEnvironment(p.Environments[name], "environments."+name); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(p.BackupTargets) {
		if err := gIdent.check("backup_targets."+name, name); err != nil {
			return err
		}
		if err := validateBackupTarget(p.BackupTargets[name], "backup_targets."+name); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(p.ExternalServices) {
		if err := gIdent.check("external_services."+name, name); err != nil {
			return err
		}
		if err := validateExternalService(p.ExternalServices[name], "external_services."+name); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(p.Workloads) {
		if err := gIdent.check("workloads."+name, name); err != nil {
			return err
		}
		if err := validateWorkload(p.Workloads[name], "workloads."+name); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(p.Services) {
		if err := gIdent.check("services."+name, name); err != nil {
			return err
		}
		if err := validateService(p.Services[name], "services."+name); err != nil {
			return err
		}
	}
	return validateTopLevel(p)
}

func validateTopLevel(p *Spec) error {
	if err := checkEnum("proxy.kind", p.Proxy.Kind, eProxyKind); err != nil {
		return err
	}
	if err := checkOptionalImageRef("proxy.image", p.Proxy.Image); err != nil {
		return err
	}
	if err := gRepoPath.checkOptional("proxy.config", p.Proxy.Config); err != nil {
		return err
	}
	if err := checkEnum("deployment.migration_policy", p.Deployment.MigrationPolicy, eMigrationPolicy); err != nil {
		return err
	}
	if err := checkPositive("deployment.retain_releases", p.Deployment.RetainReleases); err != nil {
		return err
	}
	for _, name := range sortedKeys(p.Registries) {
		r := p.Registries[name]
		if err := gRegistryHost.check("registries."+name+".server", r.Server); err != nil {
			return err
		}
		if err := gRegistryUser.checkOptional("registries."+name+".username", r.Username); err != nil {
			return err
		}
		if err := gEnvName.checkOptional("registries."+name+".password_env", r.PasswordEnv); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(p.Notifications) {
		if err := checkEnum("notifications."+name+".format", p.Notifications[name].Format, eNotifyFormat); err != nil {
			return err
		}
		// notify.Send fires only on an outcome it recognises, so an unlisted
		// event is a webhook that never calls — the block reads as configured
		// and does nothing.
		for i, event := range p.Notifications[name].On {
			if err := checkEnum(indexed("notifications."+name+".on", i), event, eNotifyEvent); err != nil {
				return err
			}
		}
	}
	for _, name := range sortedKeys(p.Hooks) {
		// A hook key is either a lifecycle seam the engine invokes, or the name
		// of a declared job — `hooks: {migrate: {run: ...}}` replaces the command
		// used to run the `migrate` job (engine/gate.go, engine/plan.go). Both
		// are real; anything else is a typo that would load and never fire.
		if !isHookSeam(name) && !p.Workloads[name].IsJob() {
			return errf("project_invalid", "hooks."+name, "",
				"%q is neither a lifecycle seam (%s) nor a declared job",
				name, strings.Join(eHookSeam, ", "))
		}
		if p.Hooks[name].Run == "" {
			return errf("project_invalid", "hooks."+name, "", "a hook must declare a command to run")
		}
	}
	if p.Runtime != nil {
		if err := validateEnvFiles(p.Runtime.EnvFiles, "runtime.env_files"); err != nil {
			return err
		}
		for i, c := range p.Runtime.EnvChecks {
			if err := gRepoPath.check(indexed("runtime.env_checks", i)+".file", c.File); err != nil {
				return err
			}
			for _, k := range append(append([]string{}, c.Require...), c.Present...) {
				if err := gEnvName.check(indexed("runtime.env_checks", i), k); err != nil {
					return err
				}
			}
		}
	}
	for i, v := range p.Verifications {
		if err := validateVerification(v, indexed("verifications", i)); err != nil {
			return err
		}
	}
	if p.Observability != nil {
		// Each sub-block is optional and independent. Checking one behind the
		// other's nil guard both skipped the check and dereferenced a nil.
		if p.Observability.Logs != nil {
			if err := gDur.checkOptional("observability.logs.retention", p.Observability.Logs.Retention); err != nil {
				return err
			}
		}
		if p.Observability.Alerts != nil {
			if err := gDur.checkOptional("observability.alerts.unhealthy_after", p.Observability.Alerts.UnhealthyAfter); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateEnvFiles holds every entry to the same rules wherever it is declared,
// so a scope cannot quietly accept something another scope refuses.
func validateEnvFiles(entries []EnvFile, path string) error {
	for i, entry := range entries {
		at := indexed(path, i)
		if entry.File == "" {
			return errf("project_invalid", at, "", "an environment file entry must name a file")
		}
		if err := gRepoPath.check(at, entry.File); err != nil {
			return err
		}
		if err := checkEnum(at+".provider", entry.Provider, eSecretProvider); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvironment(e Environment, path string) error {
	if e.Server.Host == "" {
		return errf("project_invalid", path+".server", "", "an environment must name a server")
	}
	if e.Server.Port != 0 {
		if err := checkPort(path+".server.port", e.Server.Port); err != nil {
			return err
		}
	}
	if err := gAbsPath.checkOptional(path+".base_path", e.BasePath); err != nil {
		return err
	}
	if err := validateEnvFiles(e.EnvFiles, path+".env_files"); err != nil {
		return err
	}
	if err := gDur.checkOptional(path+".policy.migration_backup_maximum_age", e.Policy.MigrationBackupMaximumAge); err != nil {
		return err
	}
	if err := gPlanSchema.checkOptional(path+".policy.minimum_plan_schema", e.Policy.MinimumPlanSchema); err != nil {
		return err
	}
	return gCalVer.checkOptional(path+".policy.minimum_onebox_version", e.Policy.MinimumOneboxVersion)
}

func validateWorkload(w Workload, path string) error {
	if err := checkEnum(path+".role", w.Role, eRole); err != nil {
		return err
	}
	if err := checkPositive(path+".replicas", w.Replicas); err != nil {
		return err
	}
	if err := checkEnum(path+".strategy", w.Strategy, eStrategy); err != nil {
		return err
	}
	if w.Image != nil {
		if err := checkImageRef(path+".image", w.Image.Reference); err != nil {
			return err
		}
		if err := checkEnum(path+".image.pull", w.Image.Pull, eImagePull); err != nil {
			return err
		}
	}
	if w.Build != nil {
		if err := gRepoPath.check(path+".build.context", w.Build.Context); err != nil {
			return err
		}
		if err := gRepoPath.checkOptional(path+".build.dockerfile", w.Build.Dockerfile); err != nil {
			return err
		}
	}
	if w.Compose != "" {
		file, service, ok := strings.Cut(w.Compose, "#")
		if !ok || service == "" {
			return errf("project_invalid", path+".compose", "",
				"%q must name a file and a service, as path#service", w.Compose)
		}
		if err := gRepoPath.check(path+".compose", file); err != nil {
			return err
		}
		if err := gComposeService.check(path+".compose", service); err != nil {
			return err
		}
	}
	if w.Domain != "" && w.Port != 0 {
		if err := checkPort(path+".port", w.Port); err != nil {
			return err
		}
	}
	for i, r := range w.Routes {
		rp := indexed(path+".routes", i)
		if r.Domain == "" {
			return errf("project_invalid", rp+".domain", "", "a route must name a domain")
		}
		if err := gURLPath.check(rp+".path", r.Path); err != nil {
			return err
		}
		if err := checkPort(rp+".port", r.Port); err != nil {
			return err
		}
		if err := checkEnum(rp+".protocol", r.Protocol, eRouteProtocol); err != nil {
			return err
		}
		if err := checkEnum(rp+".scheme", r.Scheme, eRouteScheme); err != nil {
			return err
		}
		if err := checkEnum(rp+".tls", r.TLS, eRouteTLS); err != nil {
			return err
		}
		for j, middleware := range r.Middlewares {
			mp := indexed(rp+".middlewares", j)
			if err := gMiddlewareRef.check(mp, string(middleware)); err != nil {
				return err
			}
		}
		// Passthrough means the proxy forwards the encrypted stream without
		// looking at it, which an HTTP router cannot do — it exists to read
		// the request. Accepting it here would generate a router that quietly
		// terminates instead, and the backend would answer either way.
		if r.TLS == "passthrough" && r.Protocol != "tcp" {
			return errf("project_invalid", rp+".tls", "",
				"%q requires protocol %q: an http route terminates TLS in order to read the request it routes on",
				"passthrough", "tcp")
		}
	}
	if err := validateHealth(w.Health, path+".health"); err != nil {
		return err
	}
	if w.Drain != nil {
		if err := gSignal.checkOptional(path+".drain.signal", w.Drain.Signal); err != nil {
			return err
		}
		for field, value := range map[string]string{"wait": w.Drain.Wait, "grace": w.Drain.Grace} {
			if err := gDur.checkOptional(path+".drain."+field, value); err != nil {
				return err
			}
		}
	}
	if err := validateResources(w.Resources, path+".resources"); err != nil {
		return err
	}
	for name := range w.Env {
		if err := gEnvName.check(path+".env", name); err != nil {
			return err
		}
	}
	if err := validateEnvFiles(w.EnvFiles, path+".env_files"); err != nil {
		return err
	}
	if w.Logging != nil {
		// Both land verbatim in the generated runtime, so an unchecked value
		// fails at container create on the server — after validate, preview and
		// plan have all said the project is fine.
		if err := gLogDriver.checkOptional(path+".logging.driver", w.Logging.Driver); err != nil {
			return err
		}
		for _, key := range sortedKeys(w.Logging.Options) {
			if err := gLogOption.check(path+".logging.options."+key, key); err != nil {
				return err
			}
		}
	}
	for i, v := range w.Volumes {
		vp := indexed(path+".volumes", i)
		if err := checkEnum(vp+".mode", v.Mode, eMountMode); err != nil {
			return err
		}
		if v.IsBind() {
			if err := gAbsPath.check(vp+".path", v.Path); err != nil {
				return err
			}
		} else {
			if err := gIdent.check(vp+".name", v.Name); err != nil {
				return err
			}
			// A workload's named volume must say where it mounts. Without a
			// path there is nothing to mount it at, and generation would emit
			// `name:` with an empty destination — a runtime the container
			// engine refuses to parse, discovered at deploy rather than load.
			if v.Path == "" {
				return errf("project_invalid", vp+".path", "",
					"volume %q does not say where it mounts; give it a path", v.Name)
			}
			if err := gAbsPath.check(vp+".path", v.Path); err != nil {
				return err
			}
		}
	}
	for i, port := range w.PublishedPorts {
		pp := indexed(path+".published_ports", i)
		if err := checkPort(pp+".host", port.Host); err != nil {
			return err
		}
		if err := checkPort(pp+".container", port.Container); err != nil {
			return err
		}
		if err := checkEnum(pp+".protocol", port.Protocol, ePortProtocol); err != nil {
			return err
		}
	}
	if len(w.PublishedPorts) > 0 && w.Mode() == "rolling" {
		return errf("project_invalid", path+".published_ports", "",
			"rolling workloads cannot publish fixed host ports because the serving and replacement replicas must coexist; remove published_ports or set strategy to recreate")
	}
	if w.Persistence != nil {
		if err := checkEnum(path+".persistence.mode", w.Persistence.Mode, ePersistence); err != nil {
			return err
		}
	}
	for i, n := range w.Needs {
		np := indexed(path+".needs", i)
		if err := gIdent.check(np, n.Name); err != nil {
			return err
		}
		if err := checkEnum(np+".condition", n.Condition, eNeedCondition); err != nil {
			return err
		}
		for variable, part := range n.Env {
			if err := gEnvName.check(np+".env", variable); err != nil {
				return err
			}
			if err := checkEnum(np+".env."+variable, part, eConnectionPart); err != nil {
				return err
			}
		}
	}
	if err := gAbsPath.checkOptional(path+".working_dir", w.WorkingDir); err != nil {
		return err
	}
	for key := range w.Labels {
		if strings.HasPrefix(key, "ob.") || strings.HasPrefix(key, "traefik.") {
			return errf("project_invalid", path+".labels", "",
				"%q is in a namespace Onebox generates into; choose another key", key)
		}
	}
	if w.IsJob() {
		if err := checkEnum(path+".when", w.When, eJobWhen); err != nil {
			return err
		}
		if err := checkEnum(path+".data_effect", string(w.DataEffect), eDataEffect); err != nil {
			return err
		}
		if w.DataEffect == "" {
			return errf("project_invalid", path+".data_effect", "",
				"a job must declare its data effect: %s", strings.Join(quoteAll(eDataEffect), ", "))
		}
		if err := validateSchedule(w.Schedule, path+".schedule"); err != nil {
			return err
		}
	} else if w.When != "" || w.DataEffect != "" || w.Schedule != nil {
		return errf("project_invalid", path, "",
			"when, data_effect and schedule belong to a job; this workload's role is %q", w.Role)
	}
	return nil
}

func validateHealth(h *Health, path string) error {
	if h == nil {
		return nil
	}
	if err := gURLPath.checkOptional(path+".http", h.HTTP); err != nil {
		return err
	}
	if h.Port != 0 {
		if err := checkPort(path+".port", h.Port); err != nil {
			return err
		}
	}
	for field, value := range map[string]string{
		"interval": h.Interval, "start_period": h.StartPeriod, "within": h.Within,
	} {
		if err := gDur.checkOptional(path+"."+field, value); err != nil {
			return err
		}
	}
	if h.Retries < 0 {
		return errf("project_invalid", path+".retries", "", "must not be negative")
	}
	return nil
}

func validateService(s Service, path string) error {
	if err := gIdent.checkOptional(path+".driver", s.Driver); err != nil {
		return err
	}
	if s.Version == nil || versionString(s.Version) == "" {
		return errf("project_invalid", path+".version", "",
			"a service must declare its version; an unpinned database is a future surprise")
	}
	for i, v := range s.Volumes {
		if err := gIdent.check(indexed(path+".volumes", i), v); err != nil {
			return err
		}
	}
	// Settings keys are interpolated into a generated shell command without
	// quoting, so an unchecked key is a command-injection path from a project
	// file to a root shell on the server.
	for _, key := range sortedKeys(s.Settings) {
		if err := gSettingKey.check(path+".settings."+key, key); err != nil {
			return err
		}
	}
	if s.Persistence != nil {
		if err := checkEnum(path+".persistence.mode", s.Persistence.Mode, ePersistence); err != nil {
			return err
		}
	}
	if s.Protection != nil {
		if err := validateProtectionPolicy(*s.Protection, path+".protection"); err != nil {
			return err
		}
	}
	return validateResources(s.Resources, path+".resources")
}

func validateResources(r *Resources, path string) error {
	if r == nil {
		return nil
	}
	if err := gSize.checkOptional(path+".memory", r.Memory); err != nil {
		return err
	}
	return gCpus.checkOptional(path+".cpus", r.CPUs)
}

func validateSchedule(s *Schedule, path string) error {
	if s == nil {
		return nil
	}
	if err := gCron.check(path+".cron", s.Cron); err != nil {
		return err
	}
	if len(strings.Fields(s.Cron)) != 5 {
		return errf("project_invalid", path+".cron", "", "%q must contain exactly five cron fields", s.Cron)
	}
	return gTZ.checkOptional(path+".timezone", s.Timezone)
}

func indexed(path string, i int) string {
	return path + "[" + itoa(i) + "]"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// validateVerification enforces that a check is exactly one kind. The four
// kinds probe different things and carry different fields; a check that named
// two would have to be run as one of them, and nothing would say which.
func validateVerification(v Verification, path string) error {
	kinds := 0
	for _, present := range []bool{v.HTTP != "", v.Exec != "", v.URL != "", v.MigrationRevisions != nil} {
		if present {
			kinds++
		}
	}
	if kinds != 1 {
		return errf("project_invalid", path, "",
			"a verification declares exactly one of http, exec, url or migration_revisions (found %d)", kinds)
	}

	// A container probe runs inside a workload, so it has to name one.
	if v.HTTP != "" || v.Exec != "" {
		if v.Workload == "" {
			return errf("project_invalid", path+".workload", "",
				"an http or exec check runs inside a workload and must name it")
		}
		if err := gIdent.check(path+".workload", v.Workload); err != nil {
			return err
		}
	} else if v.Workload != "" {
		return errf("project_invalid", path+".workload", "",
			"a url or migration check does not run inside a workload")
	}

	if err := gURLPath.checkOptional(path+".http", v.HTTP); err != nil {
		return err
	}
	if v.URL != "" {
		if err := gHTTPURL.check(path+".url", v.URL); err != nil {
			return err
		}
	}
	if v.Port != 0 {
		if err := checkPort(path+".port", v.Port); err != nil {
			return err
		}
	}
	for _, code := range v.StatusCodes {
		if code < 100 || code > 599 {
			return errf("project_invalid", path+".status_codes", "",
				"%d is not an HTTP status code", code)
		}
	}
	// Response-shape assertions describe a response, which only a url check
	// receives; on any other kind they would be silently ignored.
	if v.URL == "" && (len(v.StatusCodes) > 0 || len(v.RequiredHeaders) > 0 ||
		len(v.JSONAssertions) > 0 || v.Contains != "") {
		return errf("project_invalid", path, "",
			"status_codes, required_headers, json_assertions and contains describe a response, so they belong to a url check")
	}
	if v.MigrationRevisions != nil {
		if err := gIdent.check(path+".migration_revisions.job", v.MigrationRevisions.Job); err != nil {
			return err
		}
	}
	return nil
}
