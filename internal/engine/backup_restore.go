package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
)

// Recovery.
//
// One path serves both things that need it. A restore and a drill do exactly
// the same work — fetch the base backup, replay WAL to a point in time, start
// the cluster, prove it opens and answers — and differ only in what happens
// afterwards: a restore puts the recovered volume in front of the application,
// a drill throws it away and records that it worked.
//
// That is deliberate. A drill that exercised a different code path from a real
// restore would prove the drill works, which is not the claim anyone needs.

// A recovered cluster promotes on its own once replay reaches the target, but
// not instantly. Five minutes is generous for a replay that has already fetched
// everything it needs and is measured against a database that is not serving.
const (
	recoveryPromotionBudget = 5 * time.Minute
	recoveryPollInterval    = 2 * time.Second
)

// RestoreOutcome is what a recovery produced, whether or not it was kept.
type RestoreOutcome struct {
	Service                  string `json:"service"`
	Target                   string `json:"target"`
	Backup                   string `json:"backup"`
	RecoveredTo              string `json:"recovered_to"`
	Rows                     string `json:"sanity_check"`
	StagingVolume            string `json:"staging_volume,omitempty"`
	PreviousData             string `json:"previous_data_volume,omitempty"`
	Promoted                 bool   `json:"promoted"`
	DatabaseSystemIdentifier string `json:"database_system_identifier,omitempty"`
	// RetainStaging is set the moment promotion starts modifying the live
	// volume. From then on the staging volume is the only complete copy of the
	// recovered data and must survive a failure, so the operator has something
	// to recover from rather than two empty volumes.
	RetainStaging bool `json:"-"`
}

// RecoverService materialises the repository into a fresh volume at a point in
// time, proves the cluster opens, and optionally puts it in front of the
// application.
//
// The recovered cluster is always built beside the live one, never over it.
// Nothing touches the running service until the recovery has already started
// and answered a query — so a repository that cannot actually recover fails
// while the database it would have replaced is still serving.
//
// When promote is false this is a drill: the staging volume is removed and the
// live service is never touched at all.
func (e *Engine) RecoverService(ctx context.Context, service, targetTime string, promote bool) (RestoreOutcome, error) {
	_, _, err := e.backedUpService(service)
	if err != nil {
		return RestoreOutcome{}, err
	}
	if !e.Spec.ServiceIsProtected(service) {
		return RestoreOutcome{}, fmt.Errorf(
			"service %s is not running under established backup; there is no repository to recover from", service)
	}
	if targetTime != "" {
		if _, err := time.Parse(time.RFC3339, targetTime); err != nil {
			return RestoreOutcome{}, fmt.Errorf(
				"recovery target %q is not an RFC 3339 timestamp such as 2026-08-19T15:04:05Z", targetTime)
		}
	}
	// The recorded projection, not the project's current intent. Enablement
	// wrote down exactly which repository the server has been archiving to; if
	// somebody edits the target afterwards, recovery must still read the
	// repository the history is actually in. Rendering follows the same rule.
	projection, err := e.Spec.EffectiveBackupProjection(service)
	if err != nil {
		return RestoreOutcome{}, err
	}
	target, policyTarget := projection.Target, projection.Policy.Target

	n := e.names()
	staging := n.BackupRestoreVolume(service)
	container := n.BackupRestoreContainer(service)
	outcome := RestoreOutcome{Service: service, Target: targetTime, StagingVolume: staging}

	// Any leftover from an interrupted recovery goes first. Reusing a
	// half-populated staging volume is how a restore silently succeeds against
	// the wrong data.
	if err := e.discardRecoveryStaging(ctx, container, staging); err != nil {
		return outcome, err
	}
	defer func() {
		// The container is scratch either way. The volume survives only when it
		// has been promoted into service.
		_, _ = e.T.Run(context.WithoutCancel(ctx), "docker rm -f "+q(container)+" >/dev/null 2>&1 || true")
		// Kept only when promotion began and did not finish — then it is the one
		// complete copy of the recovered data and deleting it would leave the
		// operator with an empty live volume and nothing to restore from. On
		// success the live volume holds that data, so the staging copy goes.
		if outcome.Promoted || !outcome.RetainStaging {
			_, _ = e.T.Run(context.WithoutCancel(ctx), "docker volume rm -f "+q(staging)+" >/dev/null 2>&1 || true")
		}
	}()

	image, err := e.Spec.ServiceImageForRuntime(service)
	if err != nil {
		return outcome, err
	}
	repository, err := e.Spec.BackupRepository(service)
	if err != nil {
		return outcome, err
	}
	environment, err := app.WalgEnvironment(target, repository, e.Spec.Spec.Name, service)
	if err != nil {
		return outcome, err
	}
	environment["OB_S3_KEY_ENTRY"] = target.Credentials.AccessKeyEntry
	environment["OB_S3_SECRET_ENTRY"] = target.Credentials.SecretKeyEntry
	if target.Credentials.SessionTokenEntry != "" {
		environment["OB_S3_SESSION_TOKEN_ENTRY"] = target.Credentials.SessionTokenEntry
	}

	st := e.ui.Step("recovery: fetch base backup", false)
	if err := e.startRecoveryContainer(ctx, container, staging, image.Image, environment, service, policyTarget); err != nil {
		st(err)
		return outcome, err
	}
	backup, err := e.fetchRecoveryBase(ctx, container, service, targetTime)
	if err != nil {
		st(err)
		return outcome, err
	}
	outcome.Backup = backup
	st(nil)

	st = e.ui.Step("recovery: replay to "+recoveryTargetLabel(targetTime), false)
	if err := e.replayRecovery(ctx, container, targetTime); err != nil {
		st(err)
		return outcome, err
	}
	st(nil)

	// The proof. A cluster that starts is not the same as a cluster that holds
	// the data, and "the restore command exited zero" is exactly the assurance
	// this product exists to distrust.
	st = e.ui.Step("recovery: verify the recovered cluster answers", false)
	recoveredTo, rows, err := e.probeRecoveredCluster(ctx, container, targetTime)
	if err != nil {
		st(err)
		return outcome, err
	}
	outcome.RecoveredTo, outcome.Rows = recoveredTo, rows
	identifier, err := e.recoveredSystemIdentifier(ctx, container, service)
	if err != nil {
		st(err)
		return outcome, err
	}
	outcome.DatabaseSystemIdentifier = identifier
	if state, ok := e.Spec.ServiceRuntimeState(service); ok && state.DatabaseSystemIdentifier != "" && state.DatabaseSystemIdentifier != identifier {
		err := fmt.Errorf("recovered PostgreSQL cluster %s does not match selected repository generation %s", identifier, state.DatabaseSystemIdentifier)
		st(err)
		return outcome, err
	}
	st(nil)

	if !promote {
		return outcome, nil
	}
	if err := e.StageServiceCompose(ctx, service); err != nil {
		return outcome, fmt.Errorf("cannot stage the recovered cluster's repository binding: %w", err)
	}
	previous, err := e.promoteRecoveredVolume(ctx, service, container, staging, &outcome)
	outcome.PreviousData = previous
	if err != nil {
		return outcome, err
	}
	outcome.Promoted = true
	return outcome, nil
}

func (e *Engine) recoveredSystemIdentifier(ctx context.Context, container, service string) (string, error) {
	command := "docker exec -u postgres " + q(container) + " psql -U " + q(app.PgSuperuser) +
		" -d postgres -Atc " + q("select system_identifier::text from pg_control_system();")
	res, err := e.T.Run(ctx, command)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("service %s: cannot read the recovered PostgreSQL system identifier: %s", service, strings.TrimSpace(res.Stderr))
	}
	identifier := strings.TrimSpace(res.Stdout)
	if _, err := strconv.ParseUint(identifier, 10, 64); err != nil || identifier == "" {
		return "", fmt.Errorf("service %s: recovered PostgreSQL returned an invalid system identifier %q", service, identifier)
	}
	return identifier, nil
}

func recoveryTargetLabel(targetTime string) string {
	if targetTime == "" {
		return "the newest recoverable point"
	}
	return targetTime
}

func (e *Engine) discardRecoveryStaging(ctx context.Context, container, staging string) error {
	// The removals are best-effort — neither may exist — so the *result* is
	// what gets checked, not their exit codes. The previous version ended the
	// command in `true`, which made its own exit-code guard unreachable: a
	// staging volume that could not be removed reported success and recovery
	// proceeded into a half-populated volume, which is exactly what this
	// function exists to prevent.
	if _, err := e.T.Run(ctx, "docker rm -f "+q(container)+" >/dev/null 2>&1; docker volume rm -f "+q(staging)+" >/dev/null 2>&1; true"); err != nil {
		return err
	}
	res, err := e.T.Run(ctx, "docker volume inspect "+q(staging)+" >/dev/null 2>&1 && echo present || echo absent")
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.Stdout) != "absent" {
		return fmt.Errorf(
			"the staging volume %s from a previous recovery could not be removed; recovering into it would mix two restores. Remove it and retry", staging)
	}
	return nil
}

// startRecoveryContainer runs the protected image with the staged wal-g mounted
// and the data directory empty, doing nothing. The server is started later, by
// hand, so recovery configuration is in place before it reads anything.
func (e *Engine) startRecoveryContainer(ctx context.Context, container, staging, image string, environment map[string]any, service, credentialTarget string) error {
	n := e.names()
	args := []string{
		"docker", "run", "-d", "--name", q(container),
		"--network", q(n.ServiceNetwork()),
		"--entrypoint", "sleep",
		"-v", q(staging + ":/var/lib/postgresql/data"),
		"-v", q(n.BackupRuntimeDir(service) + ":" + app.WalgMountPath + ":ro"),
		"--env-file", q(n.BackupCredentialFile(service, credentialTarget)),
	}
	for _, key := range sortedEnvKeys(environment) {
		args = append(args, "-e", q(fmt.Sprintf("%s=%v", key, environment[key])))
	}
	args = append(args, q(image), "infinity")
	res, err := e.T.Run(ctx, strings.Join(args, " "))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot start the recovery container: %s", lastLines(res.Stderr, 3))
	}
	prepare := "docker exec " + q(container) + " sh -c " +
		q("mkdir -p "+app.PgDataPath+" && chown -R postgres:postgres /var/lib/postgresql/data && chmod 700 "+app.PgDataPath)
	res, err = e.T.Run(ctx, prepare)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot prepare the recovery data directory: %s", lastLines(res.Stderr, 3))
	}
	return nil
}

func (e *Engine) fetchRecoveryBase(ctx context.Context, container, service, targetTime string) (string, error) {
	// Which base backup, decided here rather than left to wal-g's LATEST.
	//
	// Replay only moves forward. A base backup that finished after the
	// requested point can never reach it: PostgreSQL replays what WAL it has,
	// runs out before the target, and dies with "recovery ended before
	// configured recovery target was reached". Fetching LATEST unconditionally
	// therefore made every point older than the newest base backup
	// unrecoverable — with a daily backup and a seven-day window, six of those
	// days could not be reached, while `ob backup status` reported the whole
	// window as recoverable.
	selected := "LATEST"
	if targetTime != "" {
		chosen, err := e.baseBackupFor(ctx, container, service, targetTime)
		if err != nil {
			return "", err
		}
		selected = chosen
	}
	// Under the repository lock, like every other wal-g invocation. Without it a
	// backup timer firing mid-restore runs its retention pass and can expire the
	// very generation this is streaming out. The per-service backup lock
	// does not help: systemd units cannot take it, which is why the flock exists.
	fetch := e.walgLockPrefix(ctx, service) +
		"docker exec -u postgres " + q(container) + " " + q(app.WalgBinary) +
		" backup-fetch " + q(app.PgDataPath) + " " + q(selected)
	res, err := e.T.Run(ctx, fetch)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("cannot fetch the base backup: %s", lastLines(res.Stderr+res.Stdout, 4))
	}
	// wal-g names the backup it chose in its log. Trimmed of the punctuation
	// the surrounding sentence puts around it, so the label is a label.
	for _, line := range strings.Split(res.Stderr+res.Stdout, "\n") {
		if index := strings.Index(line, "base_"); index >= 0 {
			return strings.Trim(strings.Fields(line[index:])[0], `'".,;:`), nil
		}
	}
	return selected, nil
}

// baseBackupFor names the newest base backup that finished at or before the
// requested point, which is the only kind replay can carry forward to it.
//
// Read from the recovery container rather than the live service: it already has
// the staged wal-g and the repository credentials, and a recovery must not
// depend on the database it may be about to replace.
func (e *Engine) baseBackupFor(ctx context.Context, container, service, targetTime string) (string, error) {
	target, err := time.Parse(time.RFC3339, targetTime)
	if err != nil {
		return "", err
	}
	list := e.walgLockPrefix(ctx, service) +
		"docker exec -u postgres " + q(container) + " " + q(app.WalgBinary) +
		" backup-list --detail --json"
	res, err := e.T.Run(ctx, list)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("cannot list the base backups to recover from: %s", lastLines(res.Stderr+res.Stdout, 3))
	}
	entries, err := parseWalgBackupList(res.Stdout)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("the repository holds no base backup, so there is nothing to recover from")
	}
	var chosen walgBackupEntry
	var oldest time.Time
	for _, entry := range entries {
		if oldest.IsZero() || entry.finished.Before(oldest) {
			oldest = entry.finished
		}
		// At or before the target, and the latest such — the least WAL to
		// replay, and the only backups that can reach the point at all.
		if !entry.finished.After(target) && (chosen.name == "" || entry.finished.After(chosen.finished)) {
			chosen = entry
		}
	}
	if chosen.name == "" {
		return "", fmt.Errorf(
			"no base backup finished at or before %s, so that point cannot be recovered to; the oldest base backup in this repository finished at %s",
			target.UTC().Format(time.RFC3339), oldest.UTC().Format(time.RFC3339))
	}
	return chosen.name, nil
}

type walgBackupEntry struct {
	name     string
	finished time.Time
}

// parseWalgBackupList reads `backup-list --detail --json`. An entry whose
// completion time cannot be read is refused rather than defaulted: a zero time
// sorts before every real one, so a silent default would make an unreadable
// entry the answer to "what can reach this point".
func parseWalgBackupList(out string) ([]walgBackupEntry, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var report []struct {
		BackupName string `json:"backup_name"`
		Time       string `json:"time"`
		FinishTime string `json:"finish_time"`
	}
	if err := json.Unmarshal([]byte(trimmed), &report); err != nil {
		return nil, fmt.Errorf("wal-g backup-list is not readable JSON")
	}
	entries := make([]walgBackupEntry, 0, len(report))
	for _, entry := range report {
		finish := entry.FinishTime
		if finish == "" {
			finish = entry.Time
		}
		finished, err := time.Parse(time.RFC3339, finish)
		if err != nil {
			return nil, fmt.Errorf("wal-g reported backup %q with an unreadable completion time %q", entry.BackupName, finish)
		}
		entries = append(entries, walgBackupEntry{name: entry.BackupName, finished: finished})
	}
	return entries, nil
}

// replayRecovery writes the recovery configuration and starts the server, which
// replays WAL until it reaches the target and then promotes.
//
// recovery_target_time is omitted entirely when no target was asked for, which
// means "replay everything available" — the newest recoverable point rather
// than an arbitrary one.
func (e *Engine) replayRecovery(ctx context.Context, container, targetTime string) error {
	settings := []string{
		"restore_command = '" + app.WalgBinary + " wal-fetch %f %p'",
		"recovery_target_action = 'promote'",
		// Every recovery target is cleared before this run states its own,
		// because the base backup may carry one. A promoted cluster keeps the
		// settings that recovered it in postgresql.conf, so every base backup
		// taken after a point-in-time restore contains that restore's target —
		// and a later recovery that asks for the newest point inherits a target
		// in the past and dies with "recovery ended before configured recovery
		// target was reached". Found by drilling a repository that had been
		// restored from once; the drill had passed before the restore.
		//
		// Last assignment wins in postgresql.conf, so clearing here and setting
		// below makes this run's target the only one in effect whatever the
		// backup carried.
		"recovery_target = ''",
		"recovery_target_time = ''",
		"recovery_target_name = ''",
		"recovery_target_xid = ''",
		"recovery_target_lsn = ''",
	}
	if targetTime != "" {
		// PostgreSQL wants a timestamptz literal here, not RFC 3339: it refuses
		// the `T` separator and the `Z` zone outright and the whole
		// configuration file fails to parse. RFC 3339 stays the input format —
		// it is the unambiguous one, and an operator typing a recovery point
		// should not have to know PostgreSQL's spelling — so it is converted
		// here instead.
		parsed, err := time.Parse(time.RFC3339, targetTime)
		if err != nil {
			return err
		}
		settings = append(settings, "recovery_target_time = '"+parsed.UTC().Format("2006-01-02 15:04:05.999999-07:00")+"'")
	}
	// Fenced by markers so promotion can take it back out again: see
	// stripRecoveryConfiguration.
	settings = append([]string{recoveryBlockStart}, settings...)
	settings = append(settings, recoveryBlockEnd)
	write := "docker exec -u postgres " + q(container) + " sh -c " +
		q("printf '%s\\n' "+shellQuoteAll(settings)+" >> "+app.PgDataPath+"/postgresql.conf && touch "+app.PgDataPath+"/recovery.signal")
	res, err := e.T.Run(ctx, write)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot write the recovery configuration: %s", lastLines(res.Stderr, 3))
	}
	start := "docker exec -u postgres " + q(container) +
		" pg_ctl -D " + q(app.PgDataPath) + " -l /tmp/ob-recovery.log -w -t 300 start"
	res, err = e.T.Run(ctx, start)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		log, _ := e.T.Run(ctx, "docker exec "+q(container)+" tail -20 /tmp/ob-recovery.log")
		return fmt.Errorf("the recovered cluster did not start: %s", lastLines(log.Stdout, 8))
	}
	return nil
}

// probeRecoveredCluster asks the recovered database whether it will actually
// answer, and reports what it holds.
//
// This is the difference between "the restore command exited zero" and "the
// data is there", which is the whole distinction this product exists to make.
func (e *Engine) probeRecoveredCluster(ctx context.Context, container, targetTime string) (string, string, error) {
	query := func(sql string) (string, error) {
		command := "docker exec -u postgres " + q(container) +
			" psql -U " + q(app.PgSuperuser) + " -d " + q(e.Spec.Spec.Name) + " -tAc " + q(sql)
		res, err := e.T.Run(ctx, command)
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("the recovered cluster refused a query: %s", lastLines(res.Stderr, 3))
		}
		return strings.TrimSpace(res.Stdout), nil
	}
	// Replay is asynchronous. `pg_ctl -w` returns as soon as the server accepts
	// connections, which happens while it is still read-only and still
	// replaying — so asking immediately would report every recovery as
	// unfinished. Wait for the promotion the recovery configuration asked for.
	promoted := false
	for waited := time.Duration(0); waited < recoveryPromotionBudget; waited += recoveryPollInterval {
		inRecovery, err := query("SELECT pg_is_in_recovery();")
		if err != nil {
			return "", "", err
		}
		if inRecovery == "f" {
			promoted = true
			break
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(recoveryPollInterval):
		}
	}
	if !promoted {
		return "", "", fmt.Errorf(
			"the recovered cluster is still replaying after %s, so it never reached the requested point; "+
				"the WAL needed to reach it may not be archived", recoveryPromotionBudget)
	}
	tables, err := query("SELECT count(*) FROM information_schema.tables WHERE table_schema='public';")
	if err != nil {
		return "", "", err
	}
	// What the cluster actually replayed to, not what time it is now. Reporting
	// wall-clock here made a drill say it had recovered to today when it had
	// been asked for a point last month — evidence that describes the wrong
	// thing is worse than no evidence.
	//
	// pg_last_xact_replay_timestamp() is empty once a cluster has been promoted
	// out of recovery, so the requested target is the authority and this is only
	// a fallback for a recovery that had none.
	recovered := targetTime
	if recovered == "" {
		replayed, err := query("SELECT coalesce(to_char(pg_last_xact_replay_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'), '');")
		if err != nil {
			return "", "", err
		}
		recovered = replayed
	}
	return recovered, tables + " tables in public schema", nil
}

// promoteRecoveredVolume puts the recovered data in front of the application.
//
// The previous volume is renamed, never deleted. A restore is the operation
// people run when they are already having a bad day, and the one thing it must
// not do is make the bad day unrecoverable — if the recovery turns out to be to
// the wrong second, the original is still there under a dated name.
func (e *Engine) promoteRecoveredVolume(ctx context.Context, service, container, staging string, outcome *RestoreOutcome) (string, error) {
	n := e.names()
	live := n.ServiceVolume(service, app.DataVolumeFor(e.Spec.Services[service]))
	kept := live + "-before-restore-" + time.Now().UTC().Format("20060102T150405Z")

	st := e.ui.Step("recovery: stop the recovered cluster cleanly", false)
	stop := "docker exec -u postgres " + q(container) + " pg_ctl -D " + q(app.PgDataPath) + " -w -t 120 -m fast stop"
	if res, err := e.T.Run(ctx, stop); err != nil {
		st(err)
		return "", err
	} else if res.ExitCode != 0 {
		err := fmt.Errorf("the recovered cluster did not stop cleanly: %s", lastLines(res.Stderr, 3))
		st(err)
		return "", err
	}
	st(nil)

	if err := e.stripRecoveryConfiguration(ctx, container); err != nil {
		return "", err
	}

	// The live volume is copied aside while it is still intact. Only after that
	// copy exists does anything destructive happen.
	st = e.ui.Step("recovery: copy the data being replaced aside", false)
	preserve := strings.Join([]string{
		"docker compose -p " + q(n.ServiceProject(service)) + " -f " + q(n.ServiceFile(service)) + " down",
		"docker volume create --label " + q("com.docker.compose.project="+n.ServiceProject(service)) +
			" --label " + q("ob.app="+e.Spec.Name) + " --label " + q("ob.service="+service) + " " + q(kept),
		"docker run --rm -v " + q(live+":/from") + " -v " + q(kept+":/to") + " alpine sh -c 'cp -a /from/. /to/'",
	}, " && ")
	res, err := e.mutate(ctx, preserve)
	if err != nil {
		st(err)
		return "", err
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("cannot copy the data being replaced aside, so nothing was changed: %s", lastLines(res.Stderr, 4))
		st(err)
		return "", err
	}
	st(nil)

	// Past this point the live volume is being overwritten, so the staging
	// volume is the only complete copy of the recovered data. It must survive a
	// failure here — deleting it would leave an empty live volume and nothing to
	// recover from.
	outcome.RetainStaging = true

	st = e.ui.Step("recovery: put the recovered data in service", false)
	// Emptied and refilled in one container rather than removed and recreated:
	// a `docker volume rm` that succeeds followed by a `create` that does not
	// leaves the service with no volume at all.
	swap := strings.Join([]string{
		"docker run --rm -v " + q(live+":/to") + " -v " + q(staging+":/from") +
			" alpine sh -c 'find /to -mindepth 1 -maxdepth 1 -exec rm -rf {} + && cp -a /from/. /to/'",
		"docker compose -p " + q(n.ServiceProject(service)) + " -f " + q(n.ServiceFile(service)) + " up -d",
	}, " && ")
	res, err = e.mutate(ctx, swap)
	if err != nil {
		st(err)
		return kept, recoveryPromotionFailure(err, kept, staging)
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("%s", lastLines(res.Stderr, 4))
		st(err)
		return kept, recoveryPromotionFailure(err, kept, staging)
	}
	st(nil)

	healthy, last, err := e.serviceIsHealthy(ctx, service)
	if err != nil {
		return kept, err
	}
	if !healthy {
		return kept, fmt.Errorf(
			"the restored service did not become healthy (last: %s).\nThe data it replaced is in volume %s, and the recovered data is in %s — neither was deleted",
			last, kept, staging)
	}
	return kept, nil
}

func sortedEnvKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func shellQuoteAll(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, q(value))
	}
	return strings.Join(quoted, " ")
}

// ReportRecovery prints what a recovery produced. It is on the engine because
// the engine owns the one UI instance a command shares.
func (e *Engine) ReportRecovery(outcome RestoreOutcome) {
	// The point recovered to is the whole claim. A drill that says only "it
	// worked" has not said what it proved, and the operator cannot tell a
	// recovery to last Tuesday from one to five minutes ago.
	point := outcome.RecoveredTo
	if point == "" {
		point = "the newest recoverable point"
	}
	if !outcome.Promoted {
		e.ui.Successf("drill passed: %s recovered to %s from %s and answered (%s). Nothing was changed.",
			outcome.Service, point, outcome.Backup, outcome.Rows)
		return
	}
	e.ui.Successf("%s restored to %s from %s (%s).", outcome.Service, point, outcome.Backup, outcome.Rows)
	e.ui.Infof("the data it replaced is kept in volume %s — remove it once you are satisfied", outcome.PreviousData)
}

// recoveryPromotionFailure names both volumes, because a failure here is the one
// moment an operator has two copies and no running database. An error that said
// only "cannot put the recovered data in service" would leave them looking for
// data they still have.
func recoveryPromotionFailure(err error, kept, staging string) error {
	return fmt.Errorf(
		"cannot put the recovered data in service: %w.\n"+
			"Nothing was lost: the data being replaced is in volume %s and the recovered data is in %s. "+
			"Restore either into the service volume by hand, or re-run the restore once the cause is fixed",
		err, kept, staging)
}

const (
	recoveryBlockStart = "# BEGIN onebox recovery — removed when the cluster is promoted"
	recoveryBlockEnd   = "# END onebox recovery"
)

// stripRecoveryConfiguration takes onebox's recovery settings back out of the
// cluster that is about to go into service.
//
// A promoted cluster that keeps them is not broken today — PostgreSQL ignores
// recovery settings without a recovery.signal — but every base backup taken
// from it carries them, so the next recovery from those backups inherits this
// restore's target and refuses to start. That is a failure the operator meets
// during a real recovery, caused by the previous one.
func (e *Engine) stripRecoveryConfiguration(ctx context.Context, container string) error {
	conf := app.PgDataPath + "/postgresql.conf"
	strip := "docker exec -u postgres " + q(container) + " sh -c " +
		q("sed -i '/^"+sedLiteral(recoveryBlockStart)+"$/,/^"+sedLiteral(recoveryBlockEnd)+"$/d' "+conf)
	res, err := e.T.Run(ctx, strip)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot remove the recovery configuration from the promoted cluster: %s", lastLines(res.Stderr, 3))
	}
	return nil
}

// sedLiteral escapes the characters sed reads as syntax inside an address. The
// markers are fixed strings in this file, so this is a guard against editing
// them into something sed would misread rather than against hostile input.
func sedLiteral(text string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "/", "\\/", ".", "\\.", "*", "\\*", "[", "\\[", "]", "\\]", "^", "\\^", "$", "\\$")
	return replacer.Replace(text)
}
