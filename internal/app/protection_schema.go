package app

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func validateBackupTarget(target BackupTarget, path string) error {
	if err := checkEnum(path+".kind", target.Kind, eBackupTargetKind); err != nil {
		return err
	}
	if err := validateBackupEndpoint(target.Endpoint, target.TLS, path); err != nil {
		return err
	}
	if err := checkEnum(path+".tls", target.TLS, eBackupTLS); err != nil {
		return err
	}
	if err := gFailureDomain.check(path+".failure_domain.identity", target.FailureDomain.Identity); err != nil {
		return err
	}
	if err := gFailureDomain.checkOptional(path+".failure_domain.host", target.FailureDomain.Host); err != nil {
		return err
	}
	if err := validateCredentialReference(target.Credentials, path+".credentials"); err != nil {
		return err
	}
	if target.Bucket != "" {
		if err := gBucket.check(path+".bucket", target.Bucket); err != nil {
			return err
		}
	}
	if err := gObjectPrefix.checkOptional(path+".prefix", target.Prefix); err != nil {
		return err
	}
	if err := gS3Region.checkOptional(path+".region", target.Region); err != nil {
		return err
	}
	if err := validateTargetEncryption(target.Encryption, path+".encryption"); err != nil {
		return err
	}

	if target.Bucket == "" {
		return errf("project_invalid", path+".bucket", "ob validate", "an S3-compatible target must name an existing bucket")
	}
	return nil
}

// ValidateBackupTarget keeps lifecycle adapters on the same closed target
// contract as project loading. Adapters receive already-resolved values, but
// revalidate at their trust boundary rather than assuming every caller loaded
// a complete project first.
func ValidateBackupTarget(name string, target BackupTarget) error {
	if err := gIdent.check("backup_targets."+name, name); err != nil {
		return err
	}
	return validateBackupTarget(target, "backup_targets."+name)
}

// BackupTargetEncryptionMode returns the authored mode for one recovery kind.
// An empty result is deliberately not a default: the lifecycle adapter must
// refuse protection whose encryption evidence cannot be established.
func BackupTargetEncryptionMode(target BackupTarget, recoveryKind string) string {
	return encryptionFor(target.Encryption, recoveryKind)
}

func validateBackupEndpoint(endpoint, tls, path string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errf("project_invalid", path+".endpoint", "ob validate", "%q must be an absolute http or https endpoint", endpoint)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errf("project_invalid", path+".endpoint", "ob validate", "a backup endpoint may not contain userinfo, query credentials, or a fragment")
	}
	if u.Scheme != "https" && tls != "insecure" {
		return errf("project_invalid", path+".endpoint", "ob validate", "an http backup endpoint requires tls: insecure; use https for verified transport")
	}
	return nil
}

func validateCredentialReference(ref CredentialReference, path string) error {
	if err := gRepoPath.check(path+".file", ref.File); err != nil {
		return err
	}
	if err := checkEnum(path+".provider", ref.Provider, eSecretProvider); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"access_key_entry": ref.AccessKeyEntry,
		"secret_key_entry": ref.SecretKeyEntry,
	} {
		if err := gEnvName.check(path+"."+field, value); err != nil {
			return err
		}
	}
	return gEnvName.checkOptional(path+".session_token_entry", ref.SessionTokenEntry)
}

func validateTargetEncryption(encryption TargetEncryption, path string) error {
	for field, value := range map[string]string{
		"snapshot": encryption.Snapshot,
		"pitr":     encryption.PITR,
		"cold":     encryption.Cold,
	} {
		if err := checkEnum(path+"."+field, value, eEncryptionMode); err != nil {
			return err
		}
	}
	return nil
}

func validateProtectionPolicy(policy ProtectionPolicy, path string) error {
	if err := gIdent.check(path+".target", policy.Target); err != nil {
		return err
	}
	if err := checkEnum(path+".recovery_kind", policy.RecoveryKind, eRecoveryKind); err != nil {
		return err
	}
	maximumDataLoss, err := positiveDuration(policy.MaximumDataLoss)
	if err != nil {
		return errf("project_invalid", path+".maximum_data_loss", "ob validate", "maximum_data_loss must be a positive duration: %v", err)
	}
	if maximumDataLoss < time.Minute {
		return errf("recovery_objective_unsupported", path+".maximum_data_loss", "ob validate", "maximum_data_loss must be at least one minute because the host scheduler has one-minute resolution")
	}
	if err := validateSchedule(&policy.Schedule, path+".schedule"); err != nil {
		return err
	}
	if policy.Retention.MinimumGenerations <= 0 {
		return errf("backup_retention_unsupported", path+".retention.minimum_generations", "ob validate", "minimum_generations must be a positive whole number")
	}
	if _, err := positiveDuration(policy.Retention.RecoveryWindow); err != nil {
		return errf("backup_retention_unsupported", path+".retention.recovery_window", "ob validate", "recovery_window must be a positive duration: %v", err)
	}
	if err := validateSchedule(&policy.RestoreDrill.Schedule, path+".restore_drill.schedule"); err != nil {
		return err
	}
	proofAge, err := positiveDuration(policy.RestoreDrill.ProofMaxAge)
	if err != nil {
		return errf("project_invalid", path+".restore_drill.proof_max_age", "ob validate", "proof_max_age must be a positive duration: %v", err)
	}
	gap, exact := maximumCronGap(policy.RestoreDrill.Schedule.Cron)
	if !exact {
		return errf("restore_drill_schedule_too_sparse", path+".restore_drill.schedule.cron", "ob validate", "restore drill cadence cannot be proven against proof_max_age; use a daily or weekday-based schedule")
	}
	if gap >= proofAge {
		return errf("restore_drill_schedule_too_sparse", path+".restore_drill.schedule.cron", "ob validate", "restore drill maximum interval %s reaches or exceeds proof_max_age %s; use a more frequent schedule", gap, proofAge)
	}
	if err := gAbsPath.checkOptional(path+".restore_drill.staging_filesystem", policy.RestoreDrill.StagingFilesystem); err != nil {
		return err
	}
	return nil
}

func validateProtectionSelection(p *Spec, serviceName, driverName string, service Service) error {
	if service.Protection == nil {
		return nil
	}
	path := "services." + serviceName + ".protection"
	policy := service.Protection
	target, ok := p.BackupTargets[policy.Target]
	if !ok {
		return errf("backup_target_unknown", path+".target", "ob validate", "protection target %q is not declared in backup_targets", policy.Target)
	}

	capability, exists := lifecycleCapabilityFor(driverName)
	version := versionString(service.Version)
	if !exists || !capability.ProtectionQualified(version) {
		return errf("backup_driver_unsupported", path, "ob validate", "driver %q version %q is runnable but has no qualified executable protection contract; remove the policy or choose a qualified driver version", driverName, version)
	}
	if !capability.SupportsRecoveryKind(version, policy.RecoveryKind) {
		return errf("recovery_objective_unsupported", path+".recovery_kind", "ob validate", "driver %q version %q does not support recovery kind %q in its qualified contract", driverName, version, policy.RecoveryKind)
	}
	if policy.RecoveryKind == "cold" && !policy.AllowBackupInterruption {
		return errf("backup_interruption_not_authorized", path+".allow_backup_interruption", "ob validate", "driver %q requires an explicitly permitted recurring stopped-service backup window", driverName)
	}
	if target.Kind != "s3-compatible" {
		return errf("recovery_objective_unsupported", path+".target", "ob validate", "%s recovery requires an s3-compatible repository target", policy.RecoveryKind)
	}
	if mode := encryptionFor(target.Encryption, policy.RecoveryKind); mode == "" {
		return errf("backup_encryption_unverified", "backup_targets."+policy.Target+".encryption."+policy.RecoveryKind, "ob validate", "target %q has no encryption policy for recovery kind %q", policy.Target, policy.RecoveryKind)
	}

	for _, envName := range sortedKeys(p.Environments) {
		host := p.Environments[envName].Server.Host
		if sameHost(host, target.FailureDomain.Host) || sameEndpointHost(host, target.Endpoint) {
			return errf("backup_target_not_independent", "backup_targets."+policy.Target+".failure_domain", "ob validate", "target %q resolves to protected host %q in environment %q", policy.Target, host, envName)
		}
	}
	return nil
}

func encryptionFor(encryption TargetEncryption, recoveryKind string) string {
	switch recoveryKind {
	case "snapshot":
		return encryption.Snapshot
	case "pitr":
		return encryption.PITR
	case "cold":
		return encryption.Cold
	default:
		return ""
	}
}

func positiveDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") && !strings.ContainsAny(strings.TrimSuffix(value, "d"), ".+-") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 32)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("%q is not positive", value)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not positive", value)
	}
	return d, nil
}

// maximumCronGap recognizes the exact daily/weekly/monthly shapes accepted by
// the protection contract. Unknown advanced expressions remain executable but
// are not used to prove a sparse schedule safe until the scheduler owns a full
// next-occurrence calculation.
func maximumCronGap(expression string) (time.Duration, bool) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return 0, false
	}
	dayOfMonth, month, dayOfWeek := fields[2], fields[3], fields[4]
	if month != "*" {
		return 0, false
	}
	if dayOfMonth != "*" {
		return 0, false
	}
	if dayOfWeek == "*" {
		return 24 * time.Hour, true
	}
	days, ok := cronWeekdays(dayOfWeek)
	if !ok || len(days) == 0 {
		return 0, false
	}
	seen := map[int]bool{}
	for _, day := range days {
		seen[day] = true
	}
	maxGap, previous := 0, -1
	first := -1
	for day := 0; day < 7; day++ {
		if !seen[day] {
			continue
		}
		if first < 0 {
			first = day
		}
		if previous >= 0 && day-previous > maxGap {
			maxGap = day - previous
		}
		previous = day
	}
	if wrap := first + 7 - previous; wrap > maxGap {
		maxGap = wrap
	}
	return time.Duration(maxGap) * 24 * time.Hour, true
}

func cronWeekdays(value string) ([]int, bool) {
	names := map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}
	var out []int
	for _, part := range strings.Split(strings.ToUpper(value), ",") {
		if strings.Contains(part, "/") {
			base, stepText, ok := strings.Cut(part, "/")
			step, err := strconv.Atoi(stepText)
			if !ok || err != nil || step <= 0 || step > 7 {
				return nil, false
			}
			start, end := 0, 7
			if base != "*" {
				bounds := strings.Split(base, "-")
				if len(bounds) != 2 {
					return nil, false
				}
				var valid bool
				start, valid = cronWeekday(bounds[0], names)
				if !valid {
					return nil, false
				}
				end, valid = cronWeekday(bounds[1], names)
				if !valid || end < start {
					return nil, false
				}
			}
			for day := start; day <= end; day += step {
				out = append(out, day%7)
			}
			continue
		}
		if strings.Contains(part, "-") {
			bounds := strings.Split(part, "-")
			if len(bounds) != 2 {
				return nil, false
			}
			start, ok := cronWeekday(bounds[0], names)
			if !ok {
				return nil, false
			}
			end, ok := cronWeekday(bounds[1], names)
			if !ok || end < start {
				return nil, false
			}
			for day := start; day <= end; day++ {
				out = append(out, day%7)
			}
			continue
		}
		day, ok := cronWeekday(part, names)
		if !ok {
			return nil, false
		}
		out = append(out, day%7)
	}
	return out, true
}

func cronWeekday(value string, names map[string]int) (int, bool) {
	if day, ok := names[value]; ok {
		return day, true
	}
	day, err := strconv.Atoi(value)
	return day, err == nil && day >= 0 && day <= 7
}

func sameHost(a, b string) bool {
	return b != "" && strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

func sameEndpointHost(host, endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && sameHost(host, u.Hostname())
}

// prepareServiceOverride permits only operational tuning beneath protection.
// Target, recovery kind, interruption authority, proof age, and staging
// identity remain project-level intent and cannot change by environment.
func prepareServiceOverride(path string, service Service, patch map[string]any) (map[string]any, error) {
	protectionValue, present := patch["protection"]
	if !present {
		return patch, nil
	}
	if service.Protection == nil {
		return nil, errf("override_not_permitted", path+".protection", "ob validate", "an environment cannot enable protection for a service that has no project-level policy")
	}
	protectionPatch, ok := protectionValue.(map[string]any)
	if !ok {
		return nil, errf("override_invalid", path+".protection", "ob validate", "a protection override must be a mapping")
	}
	allowed := map[string]bool{"schedule": true, "retention": true, "restore_drill": true}
	for _, key := range sortedKeys(protectionPatch) {
		if !allowed[key] {
			return nil, errf("override_not_permitted", path+".protection."+key, "ob validate", "%q may not be overridden per environment; protection overrides may tune only schedules and retention", key)
		}
	}

	base, err := toGeneric(*service.Protection)
	if err != nil {
		return nil, err
	}
	if value, ok := protectionPatch["schedule"]; ok {
		merged, err := mergeClosedOverride(path+".protection.schedule", base["schedule"], value, map[string]bool{"cron": true, "timezone": true})
		if err != nil {
			return nil, err
		}
		base["schedule"] = merged
	}
	if value, ok := protectionPatch["retention"]; ok {
		merged, err := mergeClosedOverride(path+".protection.retention", base["retention"], value, map[string]bool{"minimum_generations": true, "recovery_window": true})
		if err != nil {
			return nil, err
		}
		base["retention"] = merged
	}
	if value, ok := protectionPatch["restore_drill"]; ok {
		drillPatch, ok := value.(map[string]any)
		if !ok {
			return nil, errf("override_invalid", path+".protection.restore_drill", "ob validate", "a restore_drill override must be a mapping")
		}
		for _, key := range sortedKeys(drillPatch) {
			if key != "schedule" {
				return nil, errf("override_not_permitted", path+".protection.restore_drill."+key, "ob validate", "%q may not be overridden per environment; only the drill schedule may vary", key)
			}
		}
		drillBase, _ := base["restore_drill"].(map[string]any)
		if schedule, ok := drillPatch["schedule"]; ok {
			merged, err := mergeClosedOverride(path+".protection.restore_drill.schedule", drillBase["schedule"], schedule, map[string]bool{"cron": true, "timezone": true})
			if err != nil {
				return nil, err
			}
			drillBase["schedule"] = merged
		}
		base["restore_drill"] = drillBase
	}

	out := make(map[string]any, len(patch))
	for key, value := range patch {
		out[key] = value
	}
	out["protection"] = base
	return out, nil
}

func mergeClosedOverride(path string, baseValue, patchValue any, allowed map[string]bool) (map[string]any, error) {
	base, ok := baseValue.(map[string]any)
	if !ok {
		base = map[string]any{}
	}
	patch, ok := patchValue.(map[string]any)
	if !ok {
		return nil, errf("override_invalid", path, "ob validate", "an override at %s must be a mapping", path)
	}
	for _, key := range sortedKeys(patch) {
		if !allowed[key] {
			return nil, errf("override_not_permitted", path+"."+key, "ob validate", "%q may not be overridden here; permitted fields: %s", key, joinSorted(allowed))
		}
	}
	return mergeMapping(base, patch), nil
}
