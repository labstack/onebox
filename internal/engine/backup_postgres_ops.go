package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
)

// BackupGeneration is one recoverable base backup as the repository reports it.
type BackupGeneration struct {
	Label     string `json:"label"`
	Type      string `json:"type"`
	StartedAt int64  `json:"started_at"`
	StoppedAt int64  `json:"stopped_at"`
	WALStart  string `json:"wal_start,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// BackupStatus is what the repository can actually recover, read from the
// repository rather than from the project. The distinction is the whole point:
// a policy says what should be true, and only the repository says what is.
type BackupStatus struct {
	Service       string             `json:"service"`
	Repository    string             `json:"repository"`
	Generations   []BackupGeneration `json:"generations"`
	RuntimeIssues []string           `json:"runtime_issues,omitempty"`
	LatestBackup  *BackupGeneration  `json:"latest_backup,omitempty"`
	RecoverableTo string             `json:"recoverable_to,omitempty"`
}

// backedUpService resolves a service to its policy and driver, refusing every
// service this file cannot actually protect. It is the single gate: no caller
// below reaches wal-g without passing through it.
func (e *Engine) backedUpService(service string) (app.Service, *app.BackupPolicy, error) {
	declared, ok := e.Spec.Services[service]
	if !ok {
		return app.Service{}, nil, fmt.Errorf("service %s is not declared in this project", service)
	}
	driver := declared.Driver
	if driver == "" {
		driver = service
	}
	if driver != "postgres" {
		return app.Service{}, nil, fmt.Errorf(
			"service %s runs the %s driver; executable backup exists for postgres only today", service, driver)
	}
	if declared.Backup == nil {
		return app.Service{}, nil, fmt.Errorf(
			"service %s declares no backup policy; add services.%s.backup to the project first", service, service)
	}
	// Declared is not established. Without this the command reaches into a
	// container that mounts neither wal-g nor its credentials, and the operator
	// gets an OCI runtime error about a missing path instead of being told the
	// service is not protected.
	if !e.Spec.ServiceIsProtected(service) {
		return app.Service{}, nil, fmt.Errorf(
			"service %s declares backup but it has never been established, or it was disabled; run `ob backup enable %s` first",
			service, service)
	}
	return declared, declared.Backup, nil
}

// runWalg executes one wal-g operation inside the service container, as the
// server's own user, through the wrapper that puts its credentials in scope.
//
// It deliberately does not go through mutate: the repository is off-host, these
// operations are the ones the fence and the service lock already guard at a
// higher level, and routing them through the application's release machinery
// would tie a backup to a deploy that has nothing to do with it.
func (e *Engine) runWalg(ctx context.Context, service string, args ...string) (string, error) {
	return e.runWalgLocked(ctx, service, e.walgLockPrefix(ctx, service), args...)
}

// runWalgRead is for commands that only read the repository. It waits briefly
// on a shared lock instead of an hour on an exclusive one, so status answers
// while a backup is running rather than blocking behind it.
func (e *Engine) runWalgRead(ctx context.Context, service string, args ...string) (string, error) {
	return e.runWalgLocked(ctx, service, e.walgReadLockPrefix(ctx, service), args...)
}

func (e *Engine) runWalgLocked(ctx context.Context, service, lockPrefix string, args ...string) (string, error) {
	// Under the same flock the scheduled units take, so an operator running a
	// backup by hand and a timer firing cannot talk to the repository at once.
	//
	// Absent flock the command runs unwrapped, and that is not a silent
	// degradation: flock ships with util-linux on every host that can run
	// systemd, and a host that cannot run systemd has no timers, so there is
	// nothing for the lock to serialise against. SyncBackupSchedules
	// refuses such a host outright rather than leaving backups unscheduled.
	n := e.names()
	var command []string
	if lockPrefix != "" {
		command = append(command, strings.TrimSpace(lockPrefix))
	}
	command = append(command, "docker", "exec", "-u", "postgres", q(n.ServiceContainer(service)), app.WalgBinary)
	for _, arg := range args {
		command = append(command, q(arg))
	}
	res, err := e.T.Run(ctx, strings.Join(command, " "))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		// wal-g's own message is the useful one and it carries no secret: the
		// credentials reach it through the environment and it reports its
		// configuration by name.
		return "", fmt.Errorf("wal-g %s: %s", strings.Join(args, " "), lastLines(res.Stderr+res.Stdout, 6))
	}
	return res.Stdout, nil
}

// BackupService takes one base backup.
//
// wal-g has no incremental base backup for PostgreSQL in the sense pgBackRest
// does: every base backup is complete, and the space between them is covered by
// the WAL stream rather than by differential backups. So there is no type to
// choose, and retention counts backups directly.
func (e *Engine) BackupService(ctx context.Context, service string) error {
	if _, _, err := e.backedUpService(service); err != nil {
		return err
	}
	st := e.ui.Step("backup "+service, false)
	if _, err := e.runWalg(ctx, service, "backup-push", app.PgDataPath); err != nil {
		st(err)
		return err
	}
	st(nil)
	return nil
}

// PruneServiceBackups expires everything outside the declared retention.
//
// Retention has two bounds and they are not the same promise.
// keep says how many independently recoverable base backups to
// keep; window says how far back a point-in-time recovery must be able
// to reach. On a busy database the count is the binding one; on a quiet one the
// window is, because N backups might span an afternoon. Honouring only the
// count quietly shortens the window the policy promised, and nobody finds out
// until they try to recover to last Tuesday.
//
// wal-g has no working time bound — its `--after` flag is accepted and ignored
// — so the window is folded into the count before it gets here. See
// app.WalgRetainCount for that arithmetic and the measurements behind it.
//
// Separate from taking a backup because the order matters: expiring first would
// briefly hold one generation fewer than the policy promises.
func (e *Engine) PruneServiceBackups(ctx context.Context, service string) error {
	if _, _, err := e.backedUpService(service); err != nil {
		return err
	}
	projection, err := e.Spec.EffectiveBackupProjection(service)
	if err != nil {
		return err
	}
	policy := projection.Policy
	retain, err := app.WalgRetainCount(policy)
	if err != nil {
		return fmt.Errorf("service %s: %w", service, err)
	}
	label := fmt.Sprintf("prune %s (keep %d generations — %d declared, %s of history)",
		service, retain, policy.Retention.Keep, policy.Retention.Window)
	st := e.ui.Step(label, false)
	if _, err := e.runWalg(ctx, service, "delete", "retain", "FULL",
		fmt.Sprint(retain), "--confirm"); err != nil {
		st(err)
		return err
	}
	st(nil)
	return nil
}

// VerifyServiceArchive checks that the WAL segments in the repository form an
// unbroken chain.
//
// This is the check worth running, and it has no equivalent in "did the backup
// command exit zero". A base backup plus a gapped WAL stream recovers to the
// backup and no further, which is a nightly snapshot wearing the label of
// point-in-time recovery — and nothing else notices until a restore.
func (e *Engine) VerifyServiceArchive(ctx context.Context, service string) error {
	if _, _, err := e.backedUpService(service); err != nil {
		return err
	}
	st := e.ui.Step("verify "+service+" archive", false)
	if _, err := e.runWalg(ctx, service, "wal-verify", "integrity", "timeline"); err != nil {
		st(err)
		return err
	}
	st(nil)
	return nil
}

// BackupStatusFor reads what the repository holds. Everything it reports
// comes from wal-g, not from the project: the project's claim about retention
// and recovery window is exactly the claim this is here to check.
func (e *Engine) BackupStatusFor(ctx context.Context, service string) (BackupStatus, error) {
	if _, _, err := e.backedUpService(service); err != nil {
		return BackupStatus{}, err
	}
	// The recorded projection, so status reports the repository the service is
	// actually archiving to rather than one the project was edited to name.
	projection, err := e.Spec.EffectiveBackupProjection(service)
	if err != nil {
		return BackupStatus{}, err
	}
	status := BackupStatus{
		Service:    service,
		Repository: app.WalgPrefix(projection.Target, e.Spec.Spec.Name, service),
	}

	issues, err := e.VerifyBackupRuntime(ctx, service)
	if err != nil {
		return status, err
	}
	status.RuntimeIssues = issues

	out, err := e.runWalgRead(ctx, service, "backup-list", "--detail", "--json")
	if err != nil {
		return status, err
	}
	var report []struct {
		BackupName       string `json:"backup_name"`
		Time             string `json:"time"`
		StartTime        string `json:"start_time"`
		FinishTime       string `json:"finish_time"`
		WalFileName      string `json:"wal_file_name"`
		UncompressedSize int64  `json:"uncompressed_size"`
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" {
		return status, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &report); err != nil {
		return status, fmt.Errorf("service %s: wal-g backup-list is not readable JSON", service)
	}
	for _, entry := range report {
		generation := BackupGeneration{
			Label: entry.BackupName, Type: "full",
			WALStart: entry.WalFileName, SizeBytes: entry.UncompressedSize,
		}
		if started, err := time.Parse(time.RFC3339, entry.StartTime); err == nil {
			generation.StartedAt = started.Unix()
		}
		finish := entry.FinishTime
		if finish == "" {
			finish = entry.Time
		}
		stopped, err := time.Parse(time.RFC3339, finish)
		if err != nil {
			// Refused rather than defaulted. A zero timestamp renders as
			// 1970-01-01, so a report meant to say what is recoverable would
			// state a recoverable point off by half a century — and it is the
			// report an operator decides on.
			return status, fmt.Errorf(
				"service %s: wal-g reported backup %q with an unreadable completion time %q", service, entry.BackupName, finish)
		}
		generation.StoppedAt = stopped.Unix()
		status.Generations = append(status.Generations, generation)
	}
	if len(status.Generations) > 0 {
		// Sorted rather than assuming wal-g lists oldest first: "the newest
		// recoverable point" must come from the timestamps, not from the order
		// another tool happened to print.
		sort.Slice(status.Generations, func(i, j int) bool {
			return status.Generations[i].StoppedAt < status.Generations[j].StoppedAt
		})
		latest := status.Generations[len(status.Generations)-1]
		status.LatestBackup = &latest
		status.RecoverableTo = time.Unix(latest.StoppedAt, 0).UTC().Format(time.RFC3339)
	}
	return status, nil
}

func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}

// hasFlock probes the target once and remembers. Every wal-g invocation asks,
// and a round trip per backup command to learn something that cannot change
// mid-operation is waste.
func (e *Engine) hasFlock(ctx context.Context) bool {
	if e.flockProbed {
		return e.flockPresent
	}
	res, err := e.T.Run(ctx, "command -v flock >/dev/null 2>&1 && echo ok")
	e.flockProbed = true
	e.flockPresent = err == nil && strings.TrimSpace(res.Stdout) == "ok"
	return e.flockPresent
}

// walgLockPrefix is the flock every repository operation runs behind, as a
// command prefix so callers that build their own docker exec can use it too.
// Empty when the host has no flock — see hasFlock for why that is not a silent
// degradation.
//
// Read-only listing takes it too but must not wait an hour behind a running
// backup: `ob backup status` promises to answer while other work is in flight,
// and an operator who has to wait to find out whether they are recoverable is
// one who stops asking.
func (e *Engine) walgLockPrefix(ctx context.Context, service string) string {
	if !e.hasFlock(ctx) {
		return ""
	}
	return "flock -w 3600 " + q(e.names().BackupRunLock(service)) + " "
}

func (e *Engine) walgReadLockPrefix(ctx context.Context, service string) string {
	if !e.hasFlock(ctx) {
		return ""
	}
	return "flock -s -w 15 " + q(e.names().BackupRunLock(service)) + " "
}
