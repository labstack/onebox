package engine

import (
	"context"
	"encoding/json"
	"fmt"
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

// ProtectionStatus is what the repository can actually recover, read from the
// repository rather than from the project. The distinction is the whole point:
// a policy says what should be true, and only the repository says what is.
type ProtectionStatus struct {
	Service       string             `json:"service"`
	Repository    string             `json:"repository"`
	Generations   []BackupGeneration `json:"generations"`
	RuntimeIssues []string           `json:"runtime_issues,omitempty"`
	LatestBackup  *BackupGeneration  `json:"latest_backup,omitempty"`
	RecoverableTo string             `json:"recoverable_to,omitempty"`
}

// protectedService resolves a service to its policy and driver, refusing every
// service this file cannot actually protect. It is the single gate: no caller
// below reaches wal-g without passing through it.
func (e *Engine) protectedService(service string) (app.Service, *app.ProtectionPolicy, error) {
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
			"service %s runs the %s driver; executable protection exists for postgres only today", service, driver)
	}
	if declared.Protection == nil {
		return app.Service{}, nil, fmt.Errorf(
			"service %s declares no protection policy; add services.%s.protection to the project first", service, service)
	}
	// Declared is not established. Without this the command reaches into a
	// container that mounts neither wal-g nor its credentials, and the operator
	// gets an OCI runtime error about a missing path instead of being told the
	// service is not protected.
	if !e.Spec.ServiceIsProtected(service) {
		return app.Service{}, nil, fmt.Errorf(
			"service %s declares protection but it has never been established, or it was disabled; run `ob backup enable %s` first",
			service, service)
	}
	return declared, declared.Protection, nil
}

// runWalg executes one wal-g operation inside the service container, as the
// server's own user, through the wrapper that puts its credentials in scope.
//
// It deliberately does not go through mutate: the repository is off-host, these
// operations are the ones the fence and the service lock already guard at a
// higher level, and routing them through the application's release machinery
// would tie a backup to a deploy that has nothing to do with it.
func (e *Engine) runWalg(ctx context.Context, service string, args ...string) (string, error) {
	// Under the same flock the scheduled units take, so an operator running a
	// backup by hand and a timer firing cannot talk to the repository at once.
	//
	// Absent flock the command runs unwrapped, and that is not a silent
	// degradation: flock ships with util-linux on every host that can run
	// systemd, and a host that cannot run systemd has no timers, so there is
	// nothing for the lock to serialise against. SyncProtectionSchedules
	// refuses such a host outright rather than leaving backups unscheduled.
	n := e.names()
	var command []string
	if e.hasFlock(ctx) {
		command = append(command, "flock", "-w", "3600", q(n.ProtectionRunLock(service)))
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

// VerifyProtectionCredentials refuses enablement before it starts if the
// operator's credential file does not define what the repository needs. Naming
// every missing entry at once is the difference between one fix and four round
// trips, and finding out here is much cheaper than finding out from a database
// that has already restarted with archiving on.
func (e *Engine) VerifyProtectionCredentials(ctx context.Context, service, target string, required []string) error {
	path := e.names().ProtectionCredentialFile(service, target)
	res, err := e.T.Run(ctx, "cat "+q(path)+" 2>/dev/null || true")
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, _, ok := strings.Cut(line, "="); ok {
			present[strings.TrimSpace(name)] = true
		}
	}
	var missing []string
	for _, entry := range required {
		if !present[entry] {
			missing = append(missing, entry)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"the protection credential file %s does not define %s; stage it through the trusted secret flow before enabling protection.\n"+
				"%s must be 64 hex characters — generate one with `openssl rand -hex 32`",
			path, strings.Join(missing, ", "), app.WalgRepositoryKeyEntry)
	}
	return nil
}

// BackupService takes one base backup.
//
// wal-g has no incremental base backup for PostgreSQL in the sense pgBackRest
// does: every base backup is complete, and the space between them is covered by
// the WAL stream rather than by differential backups. So there is no type to
// choose, and retention counts backups directly.
func (e *Engine) BackupService(ctx context.Context, service string) error {
	if _, _, err := e.protectedService(service); err != nil {
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
// minimum_generations says how many independently recoverable base backups to
// keep; recovery_window says how far back a point-in-time recovery must be able
// to reach. On a busy database the count is the binding one; on a quiet one the
// window is, because N backups might span an afternoon. Honouring only the
// count quietly shortens the window the policy promised, and nobody finds out
// until they try to recover to last Tuesday.
//
// wal-g expresses both in one command: `retain FULL n --after t` keeps the n
// most recent full backups *and* everything taken since t. Letting it apply
// both is better than computing the cut here — the tool that owns the
// repository layout decides what a backup depends on.
//
// Separate from taking a backup because the order matters: expiring first would
// briefly hold one generation fewer than the policy promises.
func (e *Engine) PruneServiceBackups(ctx context.Context, service string) error {
	_, policy, err := e.protectedService(service)
	if err != nil {
		return err
	}
	retain, err := app.WalgRetainCount(*policy)
	if err != nil {
		return fmt.Errorf("service %s: %w", service, err)
	}
	label := fmt.Sprintf("prune %s (keep %d generations — %d declared, %s of history)",
		service, retain, policy.Retention.MinimumGenerations, policy.Retention.RecoveryWindow)
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
	if _, _, err := e.protectedService(service); err != nil {
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

// ProtectionStatusFor reads what the repository holds. Everything it reports
// comes from wal-g, not from the project: the project's claim about retention
// and recovery window is exactly the claim this is here to check.
func (e *Engine) ProtectionStatusFor(ctx context.Context, service string) (ProtectionStatus, error) {
	declared, policy, err := e.protectedService(service)
	if err != nil {
		return ProtectionStatus{}, err
	}
	_ = declared
	target, ok := e.Spec.BackupTargets[policy.Target]
	if !ok {
		return ProtectionStatus{}, fmt.Errorf("service %s names backup target %q, which is not declared", service, policy.Target)
	}
	status := ProtectionStatus{
		Service:    service,
		Repository: app.WalgPrefix(target, e.Spec.Spec.Name, service),
	}

	issues, err := e.VerifyProtectionRuntime(ctx, service)
	if err != nil {
		return status, err
	}
	status.RuntimeIssues = issues

	out, err := e.runWalg(ctx, service, "backup-list", "--detail", "--json")
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
		if stopped, err := time.Parse(time.RFC3339, finish); err == nil {
			generation.StoppedAt = stopped.Unix()
		}
		status.Generations = append(status.Generations, generation)
	}
	if len(status.Generations) > 0 {
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
