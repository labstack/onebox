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
	WALStop   string `json:"wal_stop,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// ProtectionStatus is what the repository can actually recover, read from the
// repository rather than from the project. The distinction is the whole point:
// a policy says what should be true, and only the repository says what is.
type ProtectionStatus struct {
	Service       string             `json:"service"`
	Stanza        string             `json:"stanza"`
	State         string             `json:"state"`
	RepositoryOK  bool               `json:"repository_ok"`
	Generations   []BackupGeneration `json:"generations"`
	ArchiveMin    string             `json:"archive_min,omitempty"`
	ArchiveMax    string             `json:"archive_max,omitempty"`
	LatestBackup  *BackupGeneration  `json:"latest_backup,omitempty"`
	RecoverableTo string             `json:"recoverable_to,omitempty"`
}

// protectedService resolves a service to its policy and driver, refusing every
// service this file cannot actually protect. It is the single gate: no caller
// below reaches pgBackRest without passing through it.
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
	return declared, declared.Protection, nil
}

// runPgBackRest executes one pgBackRest operation inside the service container,
// as the server's own user.
//
// It deliberately does not go through mutate: the repository is off-host, the
// operations here are the ones the fence and the service lock already guard at
// a higher level, and routing them through the application's release machinery
// would tie a backup to a deploy that has nothing to do with it.
func (e *Engine) runPgBackRest(ctx context.Context, service string, args ...string) (string, error) {
	n := e.names()
	command := []string{"docker", "exec", "-u", "postgres", q(n.ServiceContainer(service)), app.PgBackRestBinary}
	for _, arg := range args {
		command = append(command, q(arg))
	}
	res, err := e.T.Run(ctx, strings.Join(command, " "))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		// pgBackRest's own message is the useful one and it carries no secret:
		// credentials reach it through the environment, and it reports options
		// by name. Trimmed to the last lines because the console log repeats
		// the whole configuration before it gets to the failure.
		return "", fmt.Errorf("pgbackrest %s: %s", strings.Join(args, " "), lastLines(res.Stderr+res.Stdout, 6))
	}
	return res.Stdout, nil
}

// BackupService takes one base backup. `type` is pgBackRest's own vocabulary —
// full, diff, or incr — and the caller chooses because retention is expressed
// in full generations and only a full backup starts a new one.
func (e *Engine) BackupService(ctx context.Context, service, backupType string) error {
	if _, _, err := e.protectedService(service); err != nil {
		return err
	}
	switch backupType {
	case "full", "diff", "incr":
	default:
		return fmt.Errorf("backup type %q is not one of full, diff, incr", backupType)
	}
	stanza := app.PgBackRestStanza(e.Spec.Spec.Name, service)
	st := e.ui.Step("backup "+service+" ("+backupType+")", false)
	if _, err := e.runPgBackRest(ctx, service, "--stanza="+stanza, "--type="+backupType, "backup"); err != nil {
		st(err)
		return err
	}
	st(nil)
	return nil
}

// ProtectionStatusFor reads what the repository holds. Everything it reports
// comes from pgBackRest, not from the project: the project's claim about
// retention and recovery window is exactly the claim this is here to check.
func (e *Engine) ProtectionStatusFor(ctx context.Context, service string) (ProtectionStatus, error) {
	if _, _, err := e.protectedService(service); err != nil {
		return ProtectionStatus{}, err
	}
	stanza := app.PgBackRestStanza(e.Spec.Spec.Name, service)
	status := ProtectionStatus{Service: service, Stanza: stanza, State: "unknown"}

	out, err := e.runPgBackRest(ctx, service, "--stanza="+stanza, "--output=json", "info")
	if err != nil {
		return status, err
	}
	var report []struct {
		Name   string `json:"name"`
		Status struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
		Archive []struct {
			Min string `json:"min"`
			Max string `json:"max"`
		} `json:"archive"`
		Backup []struct {
			Label     string `json:"label"`
			Type      string `json:"type"`
			Timestamp struct {
				Start int64 `json:"start"`
				Stop  int64 `json:"stop"`
			} `json:"timestamp"`
			Archive struct {
				Start string `json:"start"`
				Stop  string `json:"stop"`
			} `json:"archive"`
			Info struct {
				Size int64 `json:"size"`
			} `json:"info"`
		} `json:"backup"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return status, fmt.Errorf("service %s: pgbackrest info is not readable JSON", service)
	}
	for _, entry := range report {
		if entry.Name != stanza {
			continue
		}
		status.State = entry.Status.Message
		status.RepositoryOK = entry.Status.Code == 0
		for _, archive := range entry.Archive {
			if archive.Min != "" {
				status.ArchiveMin = archive.Min
			}
			if archive.Max != "" {
				status.ArchiveMax = archive.Max
			}
		}
		for _, backup := range entry.Backup {
			status.Generations = append(status.Generations, BackupGeneration{
				Label: backup.Label, Type: backup.Type,
				StartedAt: backup.Timestamp.Start, StoppedAt: backup.Timestamp.Stop,
				WALStart: backup.Archive.Start, WALStop: backup.Archive.Stop,
				SizeBytes: backup.Info.Size,
			})
		}
	}
	if len(status.Generations) > 0 {
		latest := status.Generations[len(status.Generations)-1]
		status.LatestBackup = &latest
		// The newest recoverable point is the end of the archive, not the end
		// of the last backup: WAL kept arriving after it, and that continuing
		// stream is what makes this point-in-time recovery rather than a
		// nightly snapshot. Reporting the backup's own stop time would
		// understate the window by up to a full backup interval.
		status.RecoverableTo = time.Unix(latest.StoppedAt, 0).UTC().Format(time.RFC3339)
	}
	return status, nil
}

func (e *Engine) verifyProtectionCredentials(ctx context.Context, path string, required []string) error {
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
			"the protection credential file %s does not define %s; stage it with `ob secrets push` before enabling protection",
			path, strings.Join(missing, ", "))
	}
	return nil
}

func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}

func trimLine(text string) string { return strings.TrimSpace(text) }

func containsDigest(reference string) bool { return strings.Contains(reference, "@sha256:") }

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
			"the protection credential file %s does not define %s; stage it through the trusted secret flow before enabling protection",
			path, strings.Join(missing, ", "))
	}
	return nil
}

// CreateProtectionStanza initialises the repository for a service and proves the
// whole path works. The check is not optional decoration: stanza-create succeeds
// against a repository the server cannot actually archive to, and finding that
// out at the first WAL switch means finding it out from a full data volume.
func (e *Engine) CreateProtectionStanza(ctx context.Context, service string) error {
	stanza := app.PgBackRestStanza(e.Spec.Spec.Name, service)
	for _, step := range []struct {
		label string
		args  []string
	}{
		{"protection stanza " + stanza, []string{"--stanza=" + stanza, "stanza-create"}},
		{"protection check " + stanza, []string{"--stanza=" + stanza, "check"}},
	} {
		st := e.ui.Step(step.label, false)
		if _, err := e.runPgBackRest(ctx, service, step.args...); err != nil {
			st(err)
			return err
		}
		st(nil)
	}
	return nil
}
