package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

const protectedPostgresProject = `api_version: onebox.run/v1
app: shop
environments:
  production: {server: deploy@example.net}
workloads:
  web: {image: nginx:1}
backup_targets:
  offsite:
    kind: s3-compatible
    endpoint: https://objects.example.net
    bucket: backups
    failure_domain: {identity: remote, host: objects.example.net}
    credentials:
      file: backup.env
      provider: sops
      access_key_entry: ACCESS_KEY
      secret_key_entry: SECRET_KEY
    encryption: {pitr: client-side}
services:
  postgres:
    version: 17
    backup: {target: offsite, recovery_kind: pitr, max_data_loss: 15m}
`

func protectedPostgresResolved(t *testing.T, identifier string) *app.Resolved {
	t.Helper()
	spec, err := app.LoadBytes([]byte(protectedPostgresProject), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	projection, err := resolved.DeclaredBackupProjection("postgres")
	if err != nil {
		t.Fatal(err)
	}
	return mustRuntimeState(t, resolved, app.ServiceRuntimeState{
		BackupState: "enabled", ServiceImage: "postgres@sha256:" + strings.Repeat("a", 64),
		PublicationVerified: true, DigestAvailable: true, LastEffective: &projection,
		DatabaseSystemIdentifier: identifier, BackupRepositoryGeneration: identifier,
	})
}

func mustRuntimeState(t *testing.T, resolved *app.Resolved, state app.ServiceRuntimeState) *app.Resolved {
	t.Helper()
	bound, err := resolved.WithServiceRuntimeStates(map[string]app.ServiceRuntimeState{"postgres": state})
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestProtectedDatabaseIdentityRejectsMissingOrReplacedVolume(t *testing.T) {
	const recorded = "7513211627332151223"
	for _, tt := range []struct {
		name, actual, want string
		missing            bool
	}{
		{name: "missing", missing: true, want: "data volume ob_shop_postgres_data is missing"},
		{name: "replaced", actual: "7513211627332151224", want: "belongs to PostgreSQL cluster 7513211627332151224"},
		{name: "same", actual: recorded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				switch {
				case strings.Contains(command, "docker volume inspect"):
					if tt.missing {
						return transport.Result{ExitCode: 1, Stderr: "not found"}, true
					}
					return transport.Result{}, true
				case strings.Contains(command, "pg_controldata"):
					return transport.Result{Stdout: "Database system identifier: " + tt.actual + "\n"}, true
				}
				return transport.Result{}, false
			}}
			e := New(protectedPostgresResolved(t, recorded), testProject(t), fake, Options{Environment: "production"})
			err := e.ValidateProtectedDatabaseIdentities(context.Background())
			if tt.want == "" && err != nil {
				t.Fatalf("validate: %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestQuiesceArchiverReturnsPollingFailureImmediately(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "pg_switch_wal") {
			return transport.Result{Stdout: "0/1"}, true
		}
		if strings.Contains(command, "archive_status") {
			return transport.Result{ExitCode: 2, Stderr: "connection lost"}, true
		}
		return transport.Result{}, false
	}}
	sleeps := 0
	e := New(testConfig(), testProject(t), fake, Options{Sleep: func(_ time.Duration) { sleeps++ }, Environment: "production"})
	err := e.QuiesceArchiver(context.Background(), "postgres")
	if err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("error = %v", err)
	}
	if sleeps != 0 {
		t.Fatalf("poll failure slept %d times", sleeps)
	}
}

func TestRepositoryGenerationDiscovery(t *testing.T) {
	listing := "dir 0 0001-01-01 00:00:00 +0000 UTC clusters/\n" +
		"dir 0 0001-01-01 00:00:00 +0000 UTC 7513211627332151224/\n" +
		"dir 0 0001-01-01 00:00:00 +0000 UTC 7513211627332151223/\n" +
		"dir 0 0001-01-01 00:00:00 +0000 UTC basebackups_005/\n"
	got := strings.Join(parseRepositoryGenerations(listing), ",")
	if want := "7513211627332151223,7513211627332151224,legacy"; got != want {
		t.Fatalf("generations = %q, want %q", got, want)
	}
}
