package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

// Protection schedules.
//
// A backup policy is a promise about time. Declaring `schedule: {cron: "0 2 *
// * *"}` and then only ever backing up when somebody types the command is not
// a weaker version of that promise — it is a different thing wearing its
// clothes, and it fails silently on exactly the night nobody was watching.
//
// Two timers per protected service, taken straight from the policy rather than
// invented:
//
//   - the backup schedule takes a base backup and then applies retention, in
//     that order, so the repository is never briefly below the number of
//     generations the policy promises;
//   - the restore-drill schedule verifies the archived WAL forms an unbroken
//     chain, which is the check a green backup does not imply.
//
// They run wal-g directly rather than through `ob`, because there is no `ob` on
// the target — Onebox is agentless, and the only thing it has already placed
// there is the verified binary these units invoke.
//
// That agentlessness is also why the drill schedule verifies rather than
// actually restoring. A real drill recovers into a throwaway volume and proves
// the cluster answers, and that orchestration lives in `ob`. Reimplementing it
// in a unit file would give a drill that exercises a different path from a real
// restore — which proves the drill works, not the backups. So the unattended
// half is the check that can be made honestly here, and `ob backup drill`
// remains the whole proof, to be run from CI or a workstation on the same
// cadence the policy declares.

// SyncProtectionSchedules installs a timer for every protected service and
// removes the timers of services that are no longer protected.
//
// Removal matters as much as installation. A timer left behind for a service
// whose protection was disabled would keep pushing backups to a repository the
// project no longer describes, and nothing in the project would explain why.
func (e *Engine) SyncProtectionSchedules(ctx context.Context) error {
	n := e.names()
	prefix := app.ProtectionUnitPrefix + e.Spec.Spec.Name + "-" + e.Opts.Environment + "-"
	// flock creates the lock file but not the directory holding it.
	if res, err := e.T.Run(ctx, "mkdir -p "+q(n.AppDir()+"/protection")); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("cannot create the protection directory: %s", strings.TrimSpace(res.Stderr))
	}

	res, err := e.T.Run(ctx, "systemctl list-unit-files --no-legend --type=timer 2>/dev/null | awk '{print $1}'")
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		unit := strings.TrimSpace(line)
		if strings.HasPrefix(unit, prefix) && strings.HasSuffix(unit, ".timer") {
			installed[strings.TrimSuffix(unit, ".timer")] = true
		}
	}

	if err := e.RequireProtectionScheduling(ctx, protectedServiceNames(e.Spec)); err != nil {
		return err
	}

	type wantedUnit struct {
		name     string
		calendar string
		cron     string
		body     string
	}
	var wanted []wantedUnit
	for _, service := range e.Spec.ServiceNames() {
		if !e.Spec.ServiceIsProtected(service) {
			continue
		}
		projection, err := e.Spec.EffectiveProtectionProjection(service)
		if err != nil {
			return err
		}
		container := n.ServiceContainer(service)
		prune, err := pruneExec(container, projection.Policy)
		if err != nil {
			return fmt.Errorf("service %s retention: %w", service, err)
		}
		for _, unit := range []struct {
			operation string
			schedule  app.Schedule
			commands  []string
		}{
			{"backup", projection.Policy.Schedule, []string{
				walgExec(container, "backup-push", app.PgDataPath),
				// Retention after the new generation exists, never before.
				prune,
			}},
			{"verify", projection.Policy.RestoreDrill.Schedule, []string{
				walgExec(container, "wal-verify", "integrity", "timeline"),
			}},
		} {
			calendar, err := app.CronToCalendar(unit.schedule.Cron)
			if err != nil {
				return fmt.Errorf("service %s %s schedule: %w", service, unit.operation, err)
			}
			expression := calendar
			if unit.schedule.Timezone != "" {
				expression += " " + unit.schedule.Timezone
			}
			check, err := e.T.Run(ctx, "systemd-analyze calendar "+q(expression)+" >/dev/null 2>&1 && echo ok")
			if err != nil {
				return err
			}
			if strings.TrimSpace(check.Stdout) != "ok" {
				return fmt.Errorf(
					"service %s: the host rejected the calendar expression %q derived from cron %q. "+
						"A timezone in OnCalendar needs systemd 252 or newer; on an older host, declare the schedule in UTC",
					service, expression, unit.schedule.Cron)
			}
			wanted = append(wanted, wantedUnit{
				name:     n.ProtectionUnitForEnvironment(e.Opts.Environment, service, unit.operation),
				calendar: expression,
				cron:     unit.schedule.Cron,
				body:     protectionServiceUnit(e.Spec.Spec.Name, service, unit.operation, n.ProtectionRunLock(service), unit.commands),
			})
		}
	}

	wantedNames := map[string]bool{}
	for _, unit := range wanted {
		wantedNames[unit.name] = true
		if err := e.writeServiceFile(ctx, "/etc/systemd/system/"+unit.name+".service", []byte(unit.body)); err != nil {
			return fmt.Errorf("cannot install %s: %w", unit.name, err)
		}
		if err := e.writeServiceFile(ctx, "/etc/systemd/system/"+unit.name+".timer",
			[]byte(protectionTimerUnit(unit.name, unit.calendar))); err != nil {
			return fmt.Errorf("cannot install %s timer: %w", unit.name, err)
		}
	}

	var stale []string
	for unit := range installed {
		if !wantedNames[unit] {
			stale = append(stale, unit)
		}
	}
	for _, unit := range sortedNames(setOf(stale)) {
		if err := e.mutateChecked(ctx, "remove protection schedule "+unit, fmt.Sprintf(
			"systemctl disable --now %s.timer >/dev/null 2>&1 && rm -f /etc/systemd/system/%s.timer /etc/systemd/system/%s.service",
			unit, unit, unit)); err != nil {
			return err
		}
		e.logf("protection schedule: removed %s (no longer protected)", unit)
	}

	if len(wanted) == 0 && len(stale) == 0 {
		return nil
	}
	if res, err := e.mutate(ctx, "systemctl daemon-reload"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(res.Stderr))
	}
	for _, unit := range wanted {
		if res, err := e.mutate(ctx, "systemctl enable --now "+unit.name+".timer"); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("cannot start %s: %s", unit.name, strings.TrimSpace(res.Stderr))
		}
		e.logf("protection schedule: %s at %s", unit.name, unit.cron)
	}
	return nil
}

func walgExec(container string, args ...string) string {
	parts := []string{"/usr/bin/docker", "exec", "-u", "postgres", container, app.WalgBinary}
	return strings.Join(append(parts, args...), " ")
}

// pruneExec applies retention, with the count derived from both declared
// bounds. See app.WalgRetainCount for why the window becomes a count rather
// than a timestamp.
func pruneExec(container string, policy app.BackupPolicy) (string, error) {
	retain, err := app.WalgRetainCount(policy)
	if err != nil {
		return "", err
	}
	return walgExec(container, "delete", "retain", "FULL", fmt.Sprint(retain), "--confirm"), nil
}

// protectionServiceUnit runs the operation's commands in order, under a lock.
//
// flock is what keeps a timer from running while an interactive `ob backup`
// command is already talking to the same repository. Onebox's own protection
// lock is a value written to a file and cannot be taken from a shell, so both
// sides take this one instead: the engine wraps every wal-g invocation in the
// same flock, which makes it the single mutex over actual repository work.
//
// -w rather than -n: a backup that waits for a running one is late, and a
// backup that gives up is missing.
func protectionServiceUnit(application, service, operation, lockPath string, commands []string) string {
	lines := []string{
		"[Unit]",
		"Description=Onebox protection " + operation + " for " + service + " (" + application + ")",
		"# Written by Onebox. Edits are overwritten on the next apply.",
		"After=docker.service",
		"Requires=docker.service",
		"",
		"[Service]",
		"Type=oneshot",
	}
	for _, command := range commands {
		lines = append(lines, "ExecStart=/usr/bin/flock -w 3600 "+lockPath+" "+command)
	}
	return strings.Join(append(lines, ""), "\n")
}

func protectionTimerUnit(unit, calendar string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Onebox protection schedule " + unit,
		"# Written by Onebox. Edits are overwritten on the next apply.",
		"",
		"[Timer]",
		// The timezone belongs in the expression. `Timezone=` is not a [Timer]
		// directive: systemd ignores it silently and evaluates the calendar in
		// the host's zone.
		"OnCalendar=" + calendar,
		// A box that was off at 2am still takes the backup when it comes back.
		// For a backup this is not a convenience — it is the difference between
		// a gap in the recovery window and a late entry in it.
		"Persistent=true",
		// Spread across a minute so several services on one box do not all
		// start pushing to the same repository on the same second.
		"RandomizedDelaySec=60",
		"",
		"[Install]",
		"WantedBy=timers.target",
		"",
	}, "\n")
}

func protectedServiceNames(resolved *app.Resolved) []string {
	var out []string
	for _, service := range resolved.ServiceNames() {
		if resolved.ServiceIsProtected(service) {
			out = append(out, service)
		}
	}
	return out
}

// RequireProtectionScheduling refuses a host that cannot run the schedules a
// protected service needs.
//
// Called by enablement before anything durable happens, as well as by the
// schedule sync itself. Discovering it only at the sync would mean finding out
// after the service had already been recorded as protected and restarted
// archiving — a half-applied enablement whose only symptom is a failed command.
func (e *Engine) RequireProtectionScheduling(ctx context.Context, protected []string) error {
	if len(protected) == 0 {
		return nil
	}
	// Refused rather than skipped. A host with no systemd can run a protected
	// database perfectly well and will never take a scheduled backup, and a
	// warning at the foot of an otherwise green apply is how that goes
	// unnoticed until it matters.
	probe, err := e.T.Run(ctx, "command -v systemctl >/dev/null 2>&1 && echo ok")
	if err != nil {
		return err
	}
	if strings.TrimSpace(probe.Stdout) != "ok" {
		return fmt.Errorf(
			"this host has no systemctl, so the backup schedules declared for %s cannot be installed "+
				"and no backup would ever run unattended.\n"+
				"Run them from elsewhere on the declared cadence (`ob backup create`, `ob backup prune`, "+
				"`ob backup verify`), or use a host with systemd",
			strings.Join(protected, ", "))
	}
	// The units serialise themselves with flock. A host that schedules but
	// cannot lock would run a backup and a retention pass over the same
	// repository at once.
	if !e.hasFlock(ctx) {
		return fmt.Errorf(
			"this host has systemd but no flock, so the scheduled backups for %s could run over each other. "+
				"Install util-linux",
			strings.Join(protected, ", "))
	}
	return nil
}
