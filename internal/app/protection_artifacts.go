package app

// ProtectionEffectiveProjection is the policy and target a service is protected
// by. It is recorded at enablement and carried in the durable lifecycle state,
// so a service keeps archiving to the repository it was bound to even if the
// project's intent is later edited.
type ProtectionEffectiveProjection struct {
	Policy BackupPolicy `json:"policy"`
	Target BackupTarget     `json:"target"`
}

// The projection a protected service is actually running under.
//
// This file used to also generate a set of twelve JSON descriptors — schedules,
// retention, provenance, restore templates — describing what protection *should*
// look like on the target, together with a digest comparison to detect drift.
// None of it ever had a caller, and the design it described no longer exists:
// the schedules are systemd units derived from the policy, retention is applied
// by the prune command from the same policy, and the provenance that matters is
// the wal-g checksum pinned in this binary and verified before the binary is
// ever placed on a host.
//
// Drift is now asked of the target directly rather than of a descriptor written
// beside it — see VerifyProtectionRuntime. A second description of the truth is
// only somewhere for the two to disagree.

func (r *Resolved) effectiveProtectionProjection(serviceName string, service Service) (ProtectionEffectiveProjection, string, error) {
	if state, ok := r.serviceRuntime[serviceName]; ok && state.ProtectionState == "disable-pending" {
		if state.LastEffective == nil {
			return ProtectionEffectiveProjection{}, "", errf("protection_image_revert_unsafe", "services."+serviceName, "ob protection disable --output ndjson", "disable-pending state has no durable last-effective protection projection")
		}
		return *state.LastEffective, "last-effective", nil
	}
	if service.Backup == nil {
		return ProtectionEffectiveProjection{}, "", errf("project_invalid", "services."+serviceName+".backup", "ob validate", "service has no protection intent or retained projection")
	}
	target, ok := r.BackupTargets[service.Backup.Target]
	if !ok {
		return ProtectionEffectiveProjection{}, "", errf("backup_target_unknown", "services."+serviceName+".backup.target", "ob validate", "protection target is not declared")
	}
	return ProtectionEffectiveProjection{Policy: *service.Backup, Target: target}, "project-intent", nil
}

// EffectiveProtectionProjection is the policy and target a service is actually
// protected by: its project intent normally, and the retained last-effective
// projection while disablement is pending. Callers outside this package need it
// to record what an enablement bound, so the record and the render can never
// describe different repositories.
func (r *Resolved) EffectiveProtectionProjection(serviceName string) (ProtectionEffectiveProjection, error) {
	service, ok := r.Services[serviceName]
	if !ok {
		return ProtectionEffectiveProjection{}, errf("project_invalid", "services."+serviceName, "ob validate", "service is not declared")
	}
	projection, _, err := r.effectiveProtectionProjection(serviceName, service)
	return projection, err
}
