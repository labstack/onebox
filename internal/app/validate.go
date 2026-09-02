package app

import (
	"fmt"
	"strings"
	"time"

	obtarget "github.com/labstack/onebox/internal/target"
)

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
		if err := validateService(p.Name, name, p.Services[name], "services."+name); err != nil {
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
	seenEntrypointPorts := map[int]string{80: "web", 443: "websecure"}
	for _, name := range sortedKeys(p.Proxy.Entrypoints) {
		path := "proxy.entrypoints." + name
		if err := gIdent.check(path, name); err != nil {
			return err
		}
		if name == "web" || name == "websecure" {
			return errf("project_invalid", path, "",
				"proxy entrypoint %q is built in and cannot be redeclared", name)
		}
		port := p.Proxy.Entrypoints[name].Port
		if err := checkPort(path+".port", port); err != nil {
			return err
		}
		if previous, exists := seenEntrypointPorts[port]; exists {
			return errf("project_invalid", path+".port", "",
				"proxy entrypoints %q and %q both publish port %d", previous, name, port)
		}
		seenEntrypointPorts[port] = name
	}
	// Compose reserves `default` for the application's implicit network, and
	// Onebox owns the two derived app-scoped networks. Letting ingress reuse one
	// makes the proxy create it first under different Compose ownership, after
	// which bootstrap must either adopt a foreign network or refuse.
	if p.Proxy.Kind != "none" && p.routesAnywhere() {
		n := p.NamesFor("")
		for _, reserved := range []string{"default", n.ApplicationNetwork(), n.ServiceNetwork()} {
			if p.Proxy.Network == reserved {
				return errf("project_invalid", "proxy.network", "",
					"proxy network %q is reserved by Onebox; choose another external network name", p.Proxy.Network)
			}
		}
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
	if err := validateChecks(p.Checks); err != nil {
		return err
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
	if err := validateAddress("server", path+".server", e.Server.Host, e.Server.User, e.Server.Port); err != nil {
		return err
	}
	if err := validateJump(e.Jump, path+".jump"); err != nil {
		return err
	}
	if err := gAbsPath.checkOptional(path+".base_path", e.BasePath); err != nil {
		return err
	}
	if err := validateEnvFiles(e.EnvFiles, path+".env_files"); err != nil {
		return err
	}
	if err := gDur.checkOptional(path+".policy.migrations.backup_max_age", e.Policy.Migrations.BackupMaxAge); err != nil {
		return err
	}
	if err := gPlanSchema.checkOptional(path+".policy.min_plan_schema", e.Policy.MinPlanSchema); err != nil {
		return err
	}
	return gCalVer.checkOptional(path+".policy.min_onebox_version", e.Policy.MinOneboxVersion)
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
			if err := checkLifecycleDuration(path+".drain."+field, field, value); err != nil {
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
			if err := gBindSource.check(vp+".source", v.Source); err != nil {
				return err
			}
			for _, part := range strings.Split(v.Source, "/") {
				if part == ".." {
					return errf("path_parent_reference", vp+".source", "",
						"bind source %q must not contain a parent-directory segment", v.Source)
				}
			}
			if !strings.HasPrefix(v.Source, "/") {
				if v.Mode != "ro" {
					return errf("project_invalid", vp+".mode", "",
						"relative bind source %q is release-scoped content, not durable host state; set mode: ro for versioned release content, or use an absolute source for writable host state", v.Source)
				}
			}
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
		pp := indexed(path+".ports", i)
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
		return errf("project_invalid", path+".ports", "",
			"rolling workloads cannot publish fixed host ports because the serving and replacement replicas must coexist; remove ports or set strategy to recreate")
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
		if err := validateJobSchedule(w.Schedule, path+".schedule"); err != nil {
			return err
		}
		if w.Schedule != nil && w.Schedule.DeployLock == "pinned" {
			if w.DataEffect != DataEffectNone {
				return errf("project_invalid", path+".schedule.deploy_lock", "",
					"pinned scheduled runs require data_effect %q; %q jobs remain exclusive because a deployment may change data they are using",
					DataEffectNone, w.DataEffect)
			}
			if w.Compose != "" {
				return errf("project_invalid", path+".schedule.deploy_lock", "",
					"pinned scheduled runs require a Onebox-rendered workload; adopted Compose may reference files outside the leased release")
			}
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
		if err := checkLifecycleDuration(path+"."+field, field, value); err != nil {
			return err
		}
	}
	// Bounded above as well as below. Retries multiplies the probe interval to
	// give the rollout's drain budget, and a count large enough to overflow
	// that arithmetic yields a negative budget — one that expires immediately,
	// stopping a container the proxy may still be routing to. No real
	// healthcheck needs a four-figure count, so the bound costs nothing.
	if h.Retries < 0 {
		return errf("project_invalid", path+".retries", "", "must not be negative")
	}
	if h.Retries > maxHealthRetries {
		return errf("project_invalid", path+".retries", "",
			"must not exceed %d — retries multiplies the probe interval to give the rollout's drain budget", maxHealthRetries)
	}
	return nil
}

func validateService(appName, name string, s Service, path string) error {
	if err := gIdent.checkOptional(path+".driver", s.Driver); err != nil {
		return err
	}
	if s.Version == nil || versionString(s.Version) == "" {
		return errf("project_invalid", path+".version", "",
			"a service must declare its version; an unpinned database is a future surprise")
	}
	if s.Features != nil {
		driver, _, known := driverOf(name, s)
		if !known || driver != "postgres" {
			return errf("project_invalid", path+".features", "",
				"service features are supported only by the postgres driver")
		}
		if len(s.Features.Extensions) > 0 && versionString(s.Version) != "18" {
			return errf("project_invalid", path+".features.extensions", "",
				"PostgreSQL extensions require version 18, the version published by the Onebox PostgreSQL image")
		}
		for _, extension := range sortedKeys(s.Features.Extensions) {
			if err := gExtension.check(path+".features.extensions."+extension, extension); err != nil {
				return err
			}
		}
		if _, requested := s.Features.Extensions["pg_cron"]; requested {
			if database, authored := s.Settings["cron.database_name"]; authored && fmt.Sprint(database) != appName {
				return errf("project_invalid", path+".settings.cron.database_name", "",
					"pg_cron must use the managed application database %q", appName)
			}
			if workers, authored := s.Settings["cron.use_background_workers"]; authored {
				switch strings.ToLower(fmt.Sprint(workers)) {
				case "1", "on", "true", "yes":
				default:
					return errf("project_invalid", path+".settings.cron.use_background_workers", "",
						"pg_cron requires background workers in a Onebox-managed database")
				}
			}
		}
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
	if s.Backup != nil {
		if err := validateBackupPolicy(*s.Backup, path+".backup"); err != nil {
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

func validateJobSchedule(s *JobSchedule, path string) error {
	if s == nil {
		return nil
	}
	if err := validateScheduleFields(s.Cron, s.Timezone, path); err != nil {
		return err
	}
	if err := gDur.check(path+".timeout", s.Timeout); err != nil {
		return err
	}
	return checkEnum(path+".deploy_lock", s.DeployLock, eScheduleDeployLock)
}

func validateSchedule(s *Schedule, path string) error {
	if s == nil {
		return nil
	}
	return validateScheduleFields(s.Cron, s.Timezone, path)
}

func validateScheduleFields(cron, timezone, path string) error {
	if err := gCron.check(path+".cron", cron); err != nil {
		return err
	}
	if len(strings.Fields(cron)) != 5 {
		return errf("project_invalid", path+".cron", "", "%q must contain exactly five cron fields", cron)
	}
	return gTZ.checkOptional(path+".timezone", timezone)
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
// validateChecks validates each group against its own shape.
//
// The union check this replaced — "exactly one of http, exec, url or
// migration_revisions" — is gone because the shape can no longer express more
// than one. So is the rule that response assertions belong to a url check: they
// are now fields of URLCheck and nowhere else, which the schema enforces before
// this function runs.
func validateChecks(c Checks) error {
	for i, check := range c.HTTP {
		path := indexed("checks.http", i)
		if err := gIdent.check(path+".workload", check.Workload); err != nil {
			return err
		}
		if err := gURLPath.check(path+".path", check.Path); err != nil {
			return err
		}
		if check.Port != 0 {
			if err := checkPort(path+".port", check.Port); err != nil {
				return err
			}
		}
	}
	for i, check := range c.URL {
		path := indexed("checks.url", i)
		if err := gHTTPURL.check(path+".url", check.URL); err != nil {
			return err
		}
		for _, code := range check.StatusCodes {
			if code < 100 || code > 599 {
				return errf("project_invalid", path+".status_codes", "", "%d is not an HTTP status code", code)
			}
		}
	}
	for i, check := range c.Exec {
		path := indexed("checks.exec", i)
		if err := gIdent.check(path+".workload", check.Workload); err != nil {
			return err
		}
		if strings.TrimSpace(check.Run) == "" {
			return errf("project_invalid", path+".run", "", "an exec check must name a command to run")
		}
	}
	for i, check := range c.Migrations {
		path := indexed("checks.migrations", i)
		if err := gIdent.check(path+".job", check.Job); err != nil {
			return err
		}
		if strings.TrimSpace(check.Provider) == "" {
			return errf("project_invalid", path+".provider", "", "a migration check must name the provider that produced the revisions")
		}
	}
	return nil
}

// validateJump holds the jump to the same address grammar the transport dials
// with, so a bastion that cannot be reached is reported while reading the
// project rather than after the operator has approved a plan built around it.
func validateJump(jump *Jump, path string) error {
	if jump == nil {
		return nil
	}
	if jump.Host == "" {
		return errf("project_invalid", path, "", "a jump must name a host")
	}
	return validateAddress("jump", path, jump.Host, jump.User, jump.Port)
}

// validateAddress holds an authored SSH endpoint to the grammar the transport
// dials with. Each part is checked as it was written: recomposing them into one
// string and parsing that cannot tell a bad host from a bad user, and lets a
// host smuggle in a port or a user that only fails once something dials it.
//
// Nothing re-parses these fields on the way to the dialler, so an address that
// is only checked at connect time is an address checked after the operator has
// already approved a plan built around it.
func validateAddress(kind, path, host, user string, port int) error {
	if port != 0 {
		if err := checkPort(path+".port", port); err != nil {
			return err
		}
	}
	if !obtarget.ValidHost(host) {
		return errf("project_invalid", path+".host", "",
			"%s host %q must be a DNS name, an IPv4 address, or an unbracketed IPv6 address; write the port as `port` and the user as `user`", kind, host)
	}
	if user != "" && !obtarget.ValidUser(user) {
		return errf("project_invalid", path+".user", "",
			"%s user %q must start with a letter, digit, or underscore and contain only letters, digits, dot, underscore, or hyphen", kind, user)
	}
	return nil
}

// checkLifecycleDuration holds a deploy-lifecycle duration to the grammar and
// to a ceiling. The ceiling matters because these values are multiplied and
// summed to give budgets a rollout waits on: a duration in the thousands of
// days wraps that arithmetic, and every one of them is a duration the deploy
// would sit and sleep through if it did not.
func checkLifecycleDuration(path, field, value string) error {
	if err := gDur.checkOptional(path, value); err != nil {
		return err
	}
	if value == "" {
		return nil
	}
	// The grammar matches the shape; parsing decides whether the value is
	// representable. A day count beyond int64 nanoseconds satisfies the first
	// and fails the second, and treating that as "no bound to apply" would let
	// the very value the ceiling exists for pass unchecked.
	d, ok := ParseDuration(value)
	if !ok {
		return errf("project_invalid", path, "", "%s %q is too large to represent", field, value)
	}
	if d > maxLifecycleDuration {
		return errf("project_invalid", path, "", "%s must not exceed %s", field, maxLifecycleDuration)
	}
	return nil
}

const (
	// maxHealthRetries and maxLifecycleDuration keep retries × interval far
	// inside int64 nanoseconds while staying orders of magnitude above any real
	// healthcheck: a week between probes is already far past the point where a
	// health check is measuring anything, and a week of drain wait is a deploy
	// nobody is waiting for.
	maxHealthRetries     = 1000
	maxLifecycleDuration = 7 * 24 * time.Hour
)
