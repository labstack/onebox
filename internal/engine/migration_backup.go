package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/journal"
)

func (e *Engine) enforceMigrationBackup(ctx context.Context, jw *journal.Writer, done map[string]bool) error {
	if !e.migrationBackupRequired() || !e.hasPendingMigration(done) {
		return nil
	}
	e.progress("migration_backup", "started", "")
	evidence := e.Opts.MigrationBackup
	if err := validateMigrationBackupEvidence(evidence, e.Opts.Now().UTC()); err != nil {
		_ = jw.Append(ctx, journal.Record{
			Phase: "pre-release", SubStep: journal.MigrationBackupSubStep,
			Event: "result", Status: "fail", Detail: "migration backup authorization rejected",
			ErrorCode: "migration_backup_required",
		})
		e.progress("migration_backup", "failed", "migration backup evidence is required before migration")
		return err
	}
	if done[journal.MigrationBackupSubStep] {
		e.logf("migration backup evidence: already accepted (resume)")
		e.progress("migration_backup", "succeeded", "")
		return nil
	}
	detail := "backup evidence receipt accepted"
	if evidence.Mode == "override" {
		detail = "explicit migration backup override accepted"
		e.warnf("migration backup requirement OVERRIDDEN by %s: %s", evidence.OverrideOperator, evidence.OverrideReason)
	}
	if err := jw.Append(ctx, journal.Record{
		Phase: "pre-release", SubStep: journal.MigrationBackupSubStep,
		Event: "result", Status: "ok", Detail: detail,
	}); err != nil {
		e.progress("migration_backup", "failed", "could not persist migration backup evidence")
		return fmt.Errorf("journal migration backup evidence: %w", err)
	}
	e.progress("migration_backup", "succeeded", "")
	return nil
}

func (e *Engine) migrationBackupRequired() bool {
	if e.Opts.MigrationBackupWasRequired {
		return true
	}
	if strings.TrimSpace(e.Opts.Environment) == "" {
		return false
	}
	environment, ok := e.Cfg.Environments[e.Opts.Environment]
	return ok && environment.Policy.MigrationBackupRequired()
}

func (e *Engine) hasPendingMigration(done map[string]bool) bool {
	for _, job := range e.gateSteps() {
		if e.jobDataEffect(job) != "migration" {
			continue
		}
		if done["job:"+job] || done["migrate"] {
			continue
		}
		return true
	}
	return false
}

func validateMigrationBackupEvidence(evidence *journal.MigrationBackupEvidence, now time.Time) error {
	if evidence == nil {
		return errors.New("migration backup evidence is required by environment policy; supply a fresh plan-bound receipt or an explicit audited override")
	}
	if len(evidence.ProtectedResources) == 0 {
		return errors.New("migration backup evidence has no protected resources")
	}
	if !sort.StringsAreSorted(evidence.ProtectedResources) {
		return errors.New("migration backup protected resources are not sorted")
	}
	for i, resource := range evidence.ProtectedResources {
		if strings.TrimSpace(resource) == "" || i > 0 && resource == evidence.ProtectedResources[i-1] {
			return errors.New("migration backup protected resources must be non-empty and unique")
		}
	}
	validUntil, err := time.Parse(time.RFC3339Nano, evidence.ValidUntil)
	if err != nil {
		return errors.New("migration backup evidence has an invalid validity deadline")
	}
	if now.After(validUntil) {
		return fmt.Errorf("migration backup evidence expired at %s; supply fresh evidence or an explicit audited override", validUntil.UTC().Format(time.RFC3339))
	}
	switch evidence.Mode {
	case "receipt":
		if !validSHA256Digest(evidence.ReceiptDigest) {
			return errors.New("migration backup receipt digest is missing or invalid")
		}
		if evidence.OverrideDigest != "" || evidence.OverrideOperator != "" || evidence.OverrideReason != "" || evidence.OverrideCreatedAt != "" || evidence.OverrideSource != "" {
			return errors.New("migration backup receipt evidence contains override fields")
		}
		if strings.TrimSpace(evidence.RecordedBy) == "" {
			return errors.New("migration backup receipt recorder is missing")
		}
		if _, err := time.Parse(time.RFC3339Nano, evidence.RecordedAt); err != nil {
			return errors.New("migration backup receipt time is invalid")
		}
	case "override":
		if !validSHA256Digest(evidence.OverrideDigest) {
			return errors.New("migration backup override digest is missing or invalid")
		}
		if evidence.ReceiptDigest != "" {
			return errors.New("migration backup override evidence contains a receipt digest")
		}
		if strings.TrimSpace(evidence.OverrideOperator) == "" || strings.TrimSpace(evidence.OverrideReason) == "" || strings.TrimSpace(evidence.OverrideSource) == "" {
			return errors.New("migration backup override must include operator, reason, and source")
		}
		if _, err := time.Parse(time.RFC3339Nano, evidence.OverrideCreatedAt); err != nil {
			return errors.New("migration backup override time is invalid")
		}
	default:
		return fmt.Errorf("unknown migration backup evidence mode %q", evidence.Mode)
	}
	return nil
}

func validSHA256Digest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && value == strings.ToLower(value)
}
