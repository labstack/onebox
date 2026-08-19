package engine

import (
	"context"
	"fmt"
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
	Service       string `json:"service"`
	Target        string `json:"target"`
	Backup        string `json:"backup"`
	RecoveredTo   string `json:"recovered_to"`
	Rows          string `json:"sanity_check"`
	StagingVolume string `json:"staging_volume,omitempty"`
	PreviousData  string `json:"previous_data_volume,omitempty"`
	Promoted      bool   `json:"promoted"`
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
	_, policy, err := e.protectedService(service)
	if err != nil {
		return RestoreOutcome{}, err
	}
	if !e.Spec.ServiceIsProtected(service) {
		return RestoreOutcome{}, fmt.Errorf(
			"service %s is not running under established protection; there is no repository to recover from", service)
	}
	if targetTime != "" {
		if _, err := time.Parse(time.RFC3339, targetTime); err != nil {
			return RestoreOutcome{}, fmt.Errorf(
				"recovery target %q is not an RFC 3339 timestamp such as 2026-08-19T15:04:05Z", targetTime)
		}
	}
	target, ok := e.Spec.BackupTargets[policy.Target]
	if !ok {
		return RestoreOutcome{}, fmt.Errorf("service %s names backup target %q, which is not declared", service, policy.Target)
	}

	n := e.names()
	staging := n.ProtectionRestoreVolume(service)
	container := n.ProtectionRestoreContainer(service)
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
		if !outcome.Promoted {
			_, _ = e.T.Run(context.WithoutCancel(ctx), "docker volume rm -f "+q(staging)+" >/dev/null 2>&1 || true")
		}
	}()

	image, err := e.Spec.ServiceImageForRuntime(service)
	if err != nil {
		return outcome, err
	}
	environment, err := app.WalgEnvironment(target, e.Spec.Spec.Name, service)
	if err != nil {
		return outcome, err
	}
	environment["OB_S3_KEY_ENTRY"] = target.Credentials.AccessKeyEntry
	environment["OB_S3_SECRET_ENTRY"] = target.Credentials.SecretKeyEntry
	if target.Credentials.SessionTokenEntry != "" {
		environment["OB_S3_SESSION_TOKEN_ENTRY"] = target.Credentials.SessionTokenEntry
	}

	st := e.ui.Step("recovery: fetch base backup", false)
	if err := e.startRecoveryContainer(ctx, container, staging, image.Image, environment, service); err != nil {
		st(err)
		return outcome, err
	}
	backup, err := e.fetchRecoveryBase(ctx, container)
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
	recoveredTo, rows, err := e.probeRecoveredCluster(ctx, container)
	if err != nil {
		st(err)
		return outcome, err
	}
	outcome.RecoveredTo, outcome.Rows = recoveredTo, rows
	st(nil)

	if !promote {
		return outcome, nil
	}
	previous, err := e.promoteRecoveredVolume(ctx, service, container, staging)
	if err != nil {
		return outcome, err
	}
	outcome.Promoted, outcome.PreviousData = true, previous
	return outcome, nil
}

func recoveryTargetLabel(targetTime string) string {
	if targetTime == "" {
		return "the newest recoverable point"
	}
	return targetTime
}

func (e *Engine) discardRecoveryStaging(ctx context.Context, container, staging string) error {
	res, err := e.T.Run(ctx, "docker rm -f "+q(container)+" >/dev/null 2>&1; docker volume rm -f "+q(staging)+" >/dev/null 2>&1; true")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot clear previous recovery staging: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// startRecoveryContainer runs the protected image with the staged wal-g mounted
// and the data directory empty, doing nothing. The server is started later, by
// hand, so recovery configuration is in place before it reads anything.
func (e *Engine) startRecoveryContainer(ctx context.Context, container, staging, image string, environment map[string]any, service string) error {
	n := e.names()
	args := []string{
		"docker", "run", "-d", "--name", q(container),
		"--network", q(n.ServiceNetwork()),
		"--entrypoint", "sleep",
		"-v", q(staging + ":/var/lib/postgresql/data"),
		"-v", q(n.ProtectionRuntimeDir(service) + ":" + app.WalgMountPath + ":ro"),
		"--env-file", q(n.ProtectionCredentialFile(service, e.Spec.Services[service].Protection.Target)),
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

func (e *Engine) fetchRecoveryBase(ctx context.Context, container string) (string, error) {
	fetch := "docker exec -u postgres " + q(container) + " " + q(app.WalgBinary) + " backup-fetch " + q(app.PgDataPath) + " LATEST"
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
	return "LATEST", nil
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
func (e *Engine) probeRecoveredCluster(ctx context.Context, container string) (string, string, error) {
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
	when, err := query("SELECT to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"');")
	if err != nil {
		return "", "", err
	}
	return when, tables + " tables in public schema", nil
}

// promoteRecoveredVolume puts the recovered data in front of the application.
//
// The previous volume is renamed, never deleted. A restore is the operation
// people run when they are already having a bad day, and the one thing it must
// not do is make the bad day unrecoverable — if the recovery turns out to be to
// the wrong second, the original is still there under a dated name.
func (e *Engine) promoteRecoveredVolume(ctx context.Context, service, container, staging string) (string, error) {
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

	st = e.ui.Step("recovery: put the recovered data in service", false)
	swap := strings.Join([]string{
		"docker compose -p " + q(n.ServiceProject(service)) + " -f " + q(n.ServiceFile(service)) + " down",
		"docker volume create " + q(kept),
		// Copied rather than renamed: Docker has no rename, and a copy leaves
		// the original intact until the very last step.
		"docker run --rm -v " + q(live+":/from") + " -v " + q(kept+":/to") + " alpine sh -c 'cp -a /from/. /to/'",
		"docker volume rm -f " + q(live),
		"docker volume create " + q(live),
		"docker run --rm -v " + q(staging+":/from") + " -v " + q(live+":/to") + " alpine sh -c 'cp -a /from/. /to/'",
		"docker compose -p " + q(n.ServiceProject(service)) + " -f " + q(n.ServiceFile(service)) + " up -d",
	}, " && ")
	res, err := e.mutate(ctx, swap)
	if err != nil {
		st(err)
		return "", err
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("cannot put the recovered data in service: %s", lastLines(res.Stderr, 4))
		st(err)
		return "", err
	}
	st(nil)

	healthy, last, err := e.serviceIsHealthy(ctx, service)
	if err != nil {
		return kept, err
	}
	if !healthy {
		return kept, fmt.Errorf(
			"the restored service did not become healthy (last: %s); the data it replaced is kept in volume %s", last, kept)
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
	if !outcome.Promoted {
		e.ui.Successf("drill passed: %s recovered from %s and answered (%s). Nothing was changed.",
			outcome.Service, outcome.Backup, outcome.Rows)
		return
	}
	e.ui.Successf("%s restored from %s (%s).", outcome.Service, outcome.Backup, outcome.Rows)
	e.ui.Infof("the data it replaced is kept in volume %s — remove it once you are satisfied", outcome.PreviousData)
}
