package app

import (
	"errors"
	"strings"
	"testing"
)

const validBackupProject = `api_version: onebox.run/v1
app: shop
environments:
  production:
    server: deploy@app.example.net
workloads:
  web: {image: nginx:1}
backup_targets:
  offsite:
    kind: s3-compatible
    endpoint: https://objects.example.net
    bucket: onebox-backups
    prefix: production/shop
    failure_domain:
      identity: provider-b/region-2/account-7
      host: objects.example.net
    credentials:
      file: secrets/backup.env
      provider: sops
      access_key_entry: BACKUP_ACCESS_KEY_ID
      secret_key_entry: BACKUP_SECRET_ACCESS_KEY
    encryption:
      pitr: client-side
services:
  postgres:
    version: 17
    backup:
      target: offsite
      recovery_kind: pitr
      max_data_loss: 15m
`

func TestBackupIntentLoadsAndDefaultsToExactSchedules(t *testing.T) {
	p, err := LoadBytes([]byte(validBackupProject), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	policy := p.Services["postgres"].Backup
	if policy == nil {
		t.Fatal("backup policy was not decoded")
	}
	if policy.Schedule.Cron != "0 2 * * *" || policy.Schedule.Timezone != "UTC" {
		t.Fatalf("backup schedule = %#v, want exact daily UTC default", policy.Schedule)
	}
	if policy.Retention.Keep != 7 || policy.Retention.Window != "7d" {
		t.Fatalf("retention = %#v, want seven generations and seven days", policy.Retention)
	}
	if policy.Drill.Schedule.Cron != "0 3 * * 0,3" || policy.Drill.MaxAge != "7d" {
		t.Fatalf("restore drill = %#v, want exact twice-weekly schedule and seven-day proof age", policy.Drill)
	}
	if got := p.BackupTargets["offsite"]; got.TLS != "verify" || got.Credentials.Provider != "sops" {
		t.Fatalf("target defaults = %#v", got)
	}
}

// minio was accepted with a backup policy and then refused at
// `ob backup enable`, because postgres is the only driver whose contract runs.
// The refusal belongs at the point the policy is written, so this is now the
// same rejection every other unqualified driver gets.
func TestMinIOBackupIntentIsRefusedUntilItsContractRuns(t *testing.T) {
	project := `api_version: onebox.run/v1
app: shop
environments: {production: {server: deploy@app.example.net}}
workloads: {web: {image: nginx:1}}
backup_targets:
  offsite:
    kind: s3-compatible
    endpoint: https://objects.example.net
    bucket: onebox-backups
    failure_domain: {identity: provider-b/region-2, host: objects.example.net}
    credentials: {file: secrets/backup.env, provider: sops, access_key_entry: BACKUP_ACCESS, secret_key_entry: BACKUP_SECRET}
    encryption: {cold: client-side}
services:
  minio:
    version: RELEASE.2026-07-31T00-00-00Z
    backup: {target: offsite, recovery_kind: cold, max_data_loss: 24h, allow_downtime: true}
`
	_, err := LoadBytes([]byte(project), "ob.yml")
	if err == nil {
		t.Fatal("a minio backup policy was accepted, but no driver except postgres can establish one")
	}
	if !strings.Contains(err.Error(), "backup_driver_unsupported") {
		t.Fatalf("refusal is not the unqualified-driver one: %v", err)
	}
}

func TestReplicationIntentIsRejected(t *testing.T) {
	project := strings.ReplaceAll(validBackupProject, "kind: s3-compatible", "kind: minio-replication")
	if _, err := LoadBytes([]byte(project), "ob.yml"); err == nil {
		t.Fatal("removed replication target was accepted")
	}
}

func TestRunnableUnqualifiedDriverRejectsBackupWithoutFallback(t *testing.T) {
	if _, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: deploy@app.example.net}}
workloads: {web: {image: nginx:1}}
services: {redis: 7}
`), "ob.yml"); err != nil {
		t.Fatalf("unqualified driver must remain runnable: %v", err)
	}

	protected := strings.ReplaceAll(validBackupProject, "postgres", "redis")
	protected = strings.ReplaceAll(protected, "pitr", "snapshot")
	_, err := LoadBytes([]byte(protected), "ob.yml")
	assertAppErrorCode(t, err, "backup_driver_unsupported")
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Next != "ob validate" {
		t.Fatalf("unsupported driver failure lacks safe resolution: %#v", appErr)
	}
}

func TestEveryRuntimeDriverHasAnExplicitLifecycleRecordAndNoDefault(t *testing.T) {
	if len(lifecycleCapabilities) != len(drivers) {
		t.Fatalf("lifecycle records = %d, runtime drivers = %d", len(lifecycleCapabilities), len(drivers))
	}
	for _, name := range DriverNames() {
		capability, ok := lifecycleCapabilityFor(name)
		if !ok || capability.driver != name {
			t.Errorf("driver %q has no matching lifecycle capability record", name)
		}
	}
	if _, ok := lifecycleCapabilityFor("custom-database"); ok {
		t.Fatal("unknown driver inherited a default lifecycle capability")
	}
}

func TestBackupIntentRefusals(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "inline storage secret",
			yaml: strings.Replace(validBackupProject, "      secret_key_entry: BACKUP_SECRET_ACCESS_KEY\n", "      secret_key_entry: BACKUP_SECRET_ACCESS_KEY\n      secret_key: plaintext-must-not-enter-the-model\n", 1),
			code: "unknown_field",
		},
		{
			name: "target shares protected host",
			yaml: strings.Replace(validBackupProject, "      host: objects.example.net", "      host: app.example.net", 1),
			code: "backup_target_not_independent",
		},
		{
			name: "author selects backup tool",
			yaml: strings.Replace(validBackupProject, "      target: offsite\n", "      target: offsite\n      tool: some-backup-tool\n", 1),
			code: "unknown_field",
		},
		{
			name: "recurring policy tries to authorize enablement restart",
			yaml: strings.Replace(validBackupProject, "      max_data_loss: 15m\n", "      max_data_loss: 15m\n      allow_enablement_restart: true\n", 1),
			code: "unknown_field",
		},
		{
			name: "restore drill too sparse",
			yaml: strings.Replace(validBackupProject, "      max_data_loss: 15m\n", "      max_data_loss: 15m\n      drill:\n        schedule: {cron: '0 3 1 * *', timezone: UTC}\n        max_age: 7d\n", 1),
			code: "drill_schedule_too_sparse",
		},
		{
			name: "stepped weekday drill too sparse",
			yaml: strings.Replace(validBackupProject, "      max_data_loss: 15m\n", "      max_data_loss: 15m\n      drill:\n        schedule: {cron: '0 3 * * */2', timezone: UTC}\n        max_age: 36h\n", 1),
			code: "drill_schedule_too_sparse",
		},
		{
			name: "sub-minute replay objective",
			yaml: strings.Replace(validBackupProject, "max_data_loss: 15m", "max_data_loss: 30s", 1),
			code: "recovery_objective_unsupported",
		},
		{
			name: "unsupported retention",
			yaml: strings.Replace(validBackupProject, "      max_data_loss: 15m\n", "      max_data_loss: 15m\n      retention: {keep: 0, window: 7d}\n", 1),
			code: "backup_retention_unsupported",
		},
		{
			name: "unsupported objective",
			yaml: strings.Replace(validBackupProject, "recovery_kind: pitr", "recovery_kind: snapshot", 1),
			code: "recovery_objective_unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml), "ob.yml")
			assertAppErrorCode(t, err, tc.code)
			if strings.Contains(errorString(err), "plaintext-must-not-enter-the-model") {
				t.Fatal("failure reflected an inline credential value")
			}
		})
	}
}

func TestBackupEnvironmentOverridesTuneOnlySchedulesAndRetention(t *testing.T) {
	valid := strings.Replace(validBackupProject, "    server: deploy@app.example.net\n", `    server: deploy@app.example.net
    overrides:
      services:
        postgres:
          backup:
            schedule: {cron: '0 4 * * *'}
            retention: {keep: 10}
            drill: {schedule: {cron: '0 5 * * 1,4'}}
`, 1)
	p, err := LoadBytes([]byte(valid), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := p.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	policy := resolved.Services["postgres"].Backup
	if policy.Target != "offsite" || policy.Schedule.Cron != "0 4 * * *" || policy.Retention.Keep != 10 || policy.Retention.Window != "7d" || policy.Drill.Schedule.Cron != "0 5 * * 1,4" {
		t.Fatalf("resolved safe override = %#v", policy)
	}

	unsafe := strings.Replace(validBackupProject, "    server: deploy@app.example.net\n", `    server: deploy@app.example.net
    overrides:
      services:
        postgres:
          backup: {target: another-repository}
`, 1)
	p, err = LoadBytes([]byte(unsafe), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Resolve("production")
	assertAppErrorCode(t, err, "override_not_permitted")
}

func assertAppErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	var appErr *Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected typed app error %s, got %T: %v", code, err, err)
	}
	if appErr.Code != code {
		t.Fatalf("error code = %q, want %q: %v", appErr.Code, code, err)
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
