package engine

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func recoveryConfigTestEngine(fake *transport.Fake) *Engine {
	spec := &app.Spec{
		Name:     "shop",
		BasePath: "/var/lib/ob",
		Services: map[string]app.Service{"database": {Driver: "postgres", Version: "18"}},
	}
	return New(&app.Resolved{Spec: spec, Env: "production"}, nil, fake,
		Options{Out: io.Discard, Sleep: func(time.Duration) {}})
}

// A base backup can carry the recovery settings of the restore that produced
// the cluster it was taken from. Recovery states its own target and clears
// every other kind first, so what the backup carried cannot decide where this
// recovery stops. Without the clearing, a drill asking for the newest point
// inherited a target in the past and the cluster died with "recovery ended
// before configured recovery target was reached".
func TestRecoveryClearsAnyTargetTheBaseBackupCarried(t *testing.T) {
	fake := &transport.Fake{}
	e := recoveryConfigTestEngine(fake)

	if err := e.replayRecovery(context.Background(), "shop-database-restore-1", "database", ""); err != nil {
		t.Fatalf("replaying to the newest recoverable point: %v", err)
	}

	var written string
	for _, cmd := range fake.Commands {
		if strings.Contains(cmd, "postgresql.conf") {
			written = cmd
		}
	}
	if written == "" {
		t.Fatal("no recovery configuration was written")
	}
	// Compared with the shell quoting removed: this is about which settings are
	// written, not how many layers of escaping carry them there.
	plain := unquoteShell(written)
	for _, cleared := range []string{
		"recovery_target = ", "recovery_target_time = ", "recovery_target_name = ",
		"recovery_target_xid = ", "recovery_target_lsn = ",
	} {
		if !strings.Contains(plain, cleared) {
			t.Fatalf("recovery configuration does not clear %s:\n%s", cleared, plain)
		}
	}
	if !strings.Contains(written, recoveryBlockStart) || !strings.Contains(written, recoveryBlockEnd) {
		t.Fatalf("recovery configuration is not fenced for removal:\n%s", written)
	}
}

// A stated target still reaches PostgreSQL, and does so after the clearing so
// that it is the assignment in effect.
func TestAStatedRecoveryTargetSurvivesTheClearing(t *testing.T) {
	fake := &transport.Fake{}
	e := recoveryConfigTestEngine(fake)

	if err := e.replayRecovery(context.Background(), "shop-database-restore-1", "database", "2026-08-20T13:58:00Z"); err != nil {
		t.Fatalf("replaying to a point in time: %v", err)
	}

	var written string
	for _, cmd := range fake.Commands {
		if strings.Contains(cmd, "postgresql.conf") {
			written = cmd
		}
	}
	plain := unquoteShell(written)
	target := "recovery_target_time = 2026-08-20 13:58:00+00:00"
	if !strings.Contains(plain, target) {
		t.Fatalf("stated target missing:\n%s", plain)
	}
	if strings.Index(plain, target) < strings.Index(plain, "recovery_target_name = ") {
		t.Fatalf("the clearing overrides the stated target:\n%s", plain)
	}
}

func TestRecoveryStartsWithDeclaredExtensionPreloads(t *testing.T) {
	fake := &transport.Fake{}
	e := recoveryConfigTestEngine(fake)
	service := e.Spec.Services["database"]
	service.Features = &app.ServiceFeatures{Extensions: map[string]app.ServiceExtension{
		"pg_cron":            {},
		"pg_stat_statements": {},
	}}
	e.Spec.Services["database"] = service

	if err := e.replayRecovery(context.Background(), "shop-database-restore-1", "database", ""); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(fake.Commands, "\n")
	for _, setting := range []string{
		"shared_preload_libraries=pg_cron,pg_stat_statements",
		"cron.database_name=shop",
		"cron.use_background_workers=on",
	} {
		if !strings.Contains(commands, setting) {
			t.Errorf("recovery server command is missing %q:\n%s", setting, commands)
		}
	}
}

// The cluster that goes into service must not keep the settings that recovered
// it: every base backup taken from it would carry them into the next recovery.
func TestPromotionRemovesTheRecoveryConfiguration(t *testing.T) {
	fake := &transport.Fake{}
	e := recoveryConfigTestEngine(fake)

	if err := e.stripRecoveryConfiguration(context.Background(), "shop-database-restore-1"); err != nil {
		t.Fatalf("stripping the recovery configuration: %v", err)
	}
	if len(fake.Commands) != 1 {
		t.Fatalf("expected one command, got %v", fake.Commands)
	}
	cmd := fake.Commands[0]
	if !strings.Contains(cmd, "sed -i") || !strings.Contains(cmd, "postgresql.conf") {
		t.Fatalf("recovery configuration is not removed: %s", cmd)
	}
	if !strings.Contains(cmd, "BEGIN onebox recovery") || !strings.Contains(cmd, "END onebox recovery") {
		t.Fatalf("removal is not bounded by the markers: %s", cmd)
	}
}

// unquoteShell drops the quoting the command carries so a test can read the
// settings it writes.
func unquoteShell(command string) string {
	return strings.NewReplacer("'", "", "\\", "").Replace(command)
}
