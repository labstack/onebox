package app

// BackupEffectiveProjection is the policy and target a service is protected
// by. It is recorded at enablement and carried in the durable lifecycle state,
// so a service keeps archiving to the repository it was bound to even if the
// project's intent is later edited.
type BackupEffectiveProjection struct {
	Policy BackupPolicy `json:"policy"`
	Target BackupTarget `json:"target"`
}

// The projection a protected service is actually running under.
//
// This file used to also generate a set of twelve JSON descriptors — schedules,
// retention, provenance, restore templates — describing what backup *should*
// look like on the target, together with a digest comparison to detect drift.
// None of it ever had a caller, and the design it described no longer exists:
// the schedules are systemd units derived from the policy, retention is applied
// by the prune command from the same policy, and the provenance that matters is
// the wal-g checksum pinned in this binary and verified before the binary is
// ever placed on a host.
//
// Drift is now asked of the target directly rather than of a descriptor written
// beside it — see VerifyBackupRuntime. A second description of the truth is
// only somewhere for the two to disagree.

func (r *Resolved) effectiveBackupProjection(serviceName string, service Service) (BackupEffectiveProjection, string, error) {
	if state, ok := r.serviceRuntime[serviceName]; ok && state.BackupState == "disable-pending" {
		if state.LastEffective == nil {
			return BackupEffectiveProjection{}, "", errf("backup_image_revert_unsafe", "services."+serviceName, "ob backup disable --output ndjson", "disable-pending state has no durable last-effective backup projection")
		}
		return *state.LastEffective, "last-effective", nil
	}
	if service.Backup == nil {
		return BackupEffectiveProjection{}, "", errf("project_invalid", "services."+serviceName+".backup", "ob validate", "service has no backup intent or retained projection")
	}
	target, ok := r.BackupTargets[service.Backup.Target]
	if !ok {
		return BackupEffectiveProjection{}, "", errf("backup_target_unknown", "services."+serviceName+".backup.target", "ob validate", "backup target is not declared")
	}
	return BackupEffectiveProjection{Policy: *service.Backup, Target: target}, "project-intent", nil
}

// EffectiveBackupProjection is the repository a service is actually
// archiving to, which is not always the one the project currently names.
//
// The recorded projection wins whenever there is one. Enablement writes down
// exactly what it bound, and the server has been archiving there ever since, so
// editing `backup_targets` or `services.<n>.backup.target` afterwards must not
// silently redirect a restore at a repository the history is not in — nor at a
// credential file installed under the old target's name.
//
// Rendering, recovery, status and retention all resolve through here, so there
// is one rule rather than four that can drift. The project's intent takes
// effect at the next `ob backup enable`, which is where the change is made
// deliberately.
func (r *Resolved) EffectiveBackupProjection(serviceName string) (BackupEffectiveProjection, error) {
	service, ok := r.Services[serviceName]
	if !ok {
		return BackupEffectiveProjection{}, errf("project_invalid", "services."+serviceName, "ob validate", "service is not declared")
	}
	if state, observed := r.serviceRuntime[serviceName]; observed && state.LastEffective != nil {
		return *state.LastEffective, nil
	}
	projection, _, err := r.effectiveBackupProjection(serviceName, service)
	return projection, err
}
