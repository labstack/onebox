package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const stableV1Minimal = `
api_version: onebox.run/v1
app: demo
compose: compose.yaml
environments:
  production: { target: deploy@example.test }
components:
  web:
    type: application
    deployment: { strategy: recreate }
deployment: { order: [web] }
`

// stableV1AllOptions is the compatibility fixture for the complete v1
// authoring surface. New optional fields may be added to v1, but this fixture
// must continue to load, normalize, serialize, and reload without migration.
const stableV1AllOptions = `
api_version: onebox.run/v1
app: demo
compose: compose.yaml
environments:
  production:
    target: deploy@example.test
    policy:
      require_approval: true
      allow_agent_proposals: false
      require_migration_backup: true
      migration_backup_max_age: 24h
      require_migration_restore_test: true
      migration_backup_key_material: [runtime_environment, application_encryption_key]
components:
  web:
    type: application
    service: server
    deployment: { strategy: rolling, replicas: 2 }
    readiness:
      http: /healthz
      port: 8080
      interval: 1.5s
      start_period: 500us
      within: 14d
      retries: 2
    drain: { signal: TERM, wait: 1h30m0s, grace: 8s }
  worker:
    type: worker
    service: jobs
    deployment: { strategy: recreate }
    readiness: { exec: "test -f /tmp/ready", within: 30s }
  migrate:
    type: job
    service: schema
    data_effect: migration
    command: docker compose run --rm --no-deps schema
  database:
    type: postgres
    service: db
    persistence: { mode: durable, volumes: [db_data] }
    protection:
      backup:
        schedule: { cron: "0 2 * * *", timezone: UTC }
        retention_days: 14
      restore_drill:
        schedule: { cron: "0 4 * * 0", timezone: UTC }
  cache:
    type: redis
    persistence: { mode: ephemeral }
  mail:
    type: service
    service: mailcatcher
    persistence: { mode: external }
deployment:
  order: [web, worker]
  retain_releases: 9
  migration_policy: expand-only
runtime:
  env_files: [app.env]
  preflight:
    - { file: app.env, require: [DATABASE_URL], present: [OPTIONAL_TOKEN] }
hooks:
  bootstrap: docker version
  pre_release: { run: "./scripts/pre-release", local: true }
  post_release: docker compose ps
  post_deploy: { run: "./scripts/announce", local: true }
verification:
  - { component: web, http: /healthz, port: 8080 }
  - { component: worker, exec: "test -f /tmp/ready" }
  - url: "https://example.test/"
    contains: ready
    advisory: true
    status_codes: [200, 204]
    required_headers: { Content-Type: application/json }
    json_assertions:
      - { path: service.ready, equals: true }
  - migration_revisions:
      job: schema
      provider: atlas
      applied_revisions: ["202607010001", "202607130001"]
notifications:
  webhook: "https://notify.example.test/onebox"
  on: [failure, success]
  format: json
proxy:
  kind: traefik-docker
  managed: true
  image: traefik:v3
  config: infra/traefik
  network: onebox-ingress
secrets: { sops: secrets.enc.yaml }
registry:
  server: ghcr.io
  username: deploy
  password_env: GHCR_TOKEN
observability:
  logs: { enabled: true, retention_days: 7 }
  metrics: { enabled: true }
  alerts: { unhealthy_after: 1h30m0s }
`

func loadAndValidateV1(t *testing.T, source string) *Config {
	t.Helper()
	cfg, err := LoadBytes([]byte(source), "ob.yml")
	if err != nil {
		t.Fatalf("load stable v1 fixture: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate stable v1 fixture: %v", err)
	}
	return cfg
}

func requireV1Rejected(t *testing.T, source string) {
	t.Helper()
	cfg, err := LoadBytes([]byte(source), "ob.yml")
	if err == nil {
		err = cfg.Validate()
	}
	if err == nil {
		t.Fatal("invalid stable v1 config was accepted")
	}
}

func TestStableV1MinimalRoundTrip(t *testing.T) {
	cfg := loadAndValidateV1(t, stableV1Minimal)
	resolved, err := cfg.YAML()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadBytes(resolved, "resolved-ob.yml")
	if err != nil {
		t.Fatalf("reload resolved minimal config: %v\n%s", err, resolved)
	}
	if err := reloaded.Validate(); err != nil {
		t.Fatalf("validate resolved minimal config: %v\n%s", err, resolved)
	}
	if reloaded.Retain != 5 || reloaded.Deployment.MigrationPolicy != "manual" {
		t.Fatalf("stable defaults lost after round trip: retain=%d migration_policy=%q", reloaded.Retain, reloaded.Deployment.MigrationPolicy)
	}
}

func TestStableV1AllOptionsRoundTripAndNormalizeIsIdempotent(t *testing.T) {
	cfg := loadAndValidateV1(t, stableV1AllOptions)
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("second Normalize must be safe: %v", err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("third Normalize must be safe: %v", err)
	}
	if got := time.Duration(cfg.Roles["web"].Ready.Within); got != 14*24*time.Hour {
		t.Fatalf("whole-day duration = %v, want 14d", got)
	}
	if got := cfg.Hooks["schema"]; got.Run == "" {
		t.Fatalf("job command was not normalized into the runtime hook map: %+v", got)
	}

	resolved, err := cfg.YAML()
	if err != nil {
		t.Fatal(err)
	}
	reloaded := loadAndValidateV1(t, string(resolved))
	if got := reloaded.Hooks["schema"]; got.Run == "" {
		t.Fatalf("job command was lost by snapshot reload: %+v\n%s", got, resolved)
	}

	var document map[string]any
	if err := yaml.Unmarshal(resolved, &document); err != nil {
		t.Fatalf("decode resolved YAML: %v", err)
	}
	hooks, ok := document["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("resolved lifecycle hooks missing or malformed: %#v", document["hooks"])
	}
	if _, duplicated := hooks["schema"]; duplicated {
		t.Fatalf("normalized job command leaked into authored lifecycle hooks: %#v", hooks)
	}
	components, ok := document["components"].(map[string]any)
	if !ok {
		t.Fatalf("resolved components missing or malformed: %#v", document["components"])
	}
	job, ok := components["migrate"].(map[string]any)
	if !ok || job["command"] == nil {
		t.Fatalf("job command must remain nested under components.migrate: %#v", components["migrate"])
	}
}

func TestStableV1DurationGrammarMatchesResolvedYAML(t *testing.T) {
	for _, duration := range []string{"1h30m0s", "1.5s", "500us", "500µs", "14d"} {
		t.Run(duration, func(t *testing.T) {
			source := strings.Replace(stableV1Minimal,
				"deployment: { strategy: recreate }",
				`deployment: { strategy: recreate }
    readiness: { exec: "true", within: `+duration+" }", 1)
			cfg := loadAndValidateV1(t, source)
			resolved, err := cfg.YAML()
			if err != nil {
				t.Fatal(err)
			}
			loadAndValidateV1(t, string(resolved))
		})
	}
	requireV1Rejected(t, strings.Replace(stableV1Minimal,
		"deployment: { strategy: recreate }",
		`deployment: { strategy: recreate }
    readiness: { exec: "true", within: 999999999999999999999d }`, 1))
}

func TestStableV1HooksAreClosedLifecycleHooks(t *testing.T) {
	loadAndValidateV1(t, stableV1Minimal+`hooks:
  bootstrap: docker version
  pre_release: echo pre
  post_release: echo released
  post_deploy: echo deployed
`)
	requireV1Rejected(t, stableV1Minimal+`hooks:
  post_deply: echo typo
`)
	requireV1Rejected(t, stableV1Minimal+`hooks:
  migrate: docker compose run --rm migrate
`)
	reservedJobService := strings.Replace(stableV1Minimal,
		"deployment: { order: [web] }",
		`  cleanup:
    type: job
    service: post_deploy
    data_effect: none
deployment: { order: [web] }`, 1)
	requireV1Rejected(t, reservedJobService)
}

func TestStableV1VerificationIsDiscriminated(t *testing.T) {
	valid := []string{
		"{ component: web, http: /healthz, port: 8080 }",
		`{ component: web, exec: "true" }`,
		`{ url: "https://example.test/", contains: ready, advisory: true }`,
	}
	for _, verification := range valid {
		loadAndValidateV1(t, stableV1Minimal+"verification:\n  - "+verification+"\n")
	}

	invalid := []string{
		"{}",
		"{ component: web }",
		"{ http: /healthz }",
		"{ component: web, http: /healthz }",
		`{ exec: "true" }`,
		"{ component: missing, http: /healthz, port: 8080 }",
		`{ component: web, http: /healthz, exec: "true" }`,
		`{ component: web, http: /healthz, url: "https://example.test/" }`,
		`{ component: web, url: "https://example.test/" }`,
	}
	for _, verification := range invalid {
		t.Run(verification, func(t *testing.T) {
			requireV1Rejected(t, stableV1Minimal+"verification:\n  - "+verification+"\n")
		})
	}
}

func TestStableV1RejectsRemovedSingletonAndDuplicateOrder(t *testing.T) {
	singleton := strings.Replace(stableV1Minimal,
		"strategy: recreate", "strategy: recreate, singleton: true", 1)
	requireV1Rejected(t, singleton)

	duplicate := strings.Replace(stableV1Minimal, "order: [web]", "order: [web, web]", 1)
	cfg, err := LoadBytes([]byte(duplicate), "ob.yml")
	if err == nil {
		err = cfg.Validate()
	}
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		t.Fatalf("duplicate deployment order must be rejected explicitly, got %v", err)
	}
}

func TestStableV1RejectsZeroAlertDuration(t *testing.T) {
	requireV1Rejected(t, stableV1Minimal+`observability:
  alerts: { unhealthy_after: 0s }
`)
}

func TestStableV1IdentifiersStartAlphanumeric(t *testing.T) {
	invalid := []string{
		strings.Replace(stableV1Minimal, "production:", "-production:", 1),
		strings.Replace(stableV1Minimal, "  web:", "  -web:", 1),
		strings.Replace(stableV1Minimal, "deployment: { strategy: recreate }", "service: -server\n    deployment: { strategy: recreate }", 1),
		strings.Replace(stableV1Minimal, "deployment: { order: [web] }", "  database:\n    type: postgres\n    persistence: { mode: durable, volumes: [-data] }\ndeployment: { order: [web] }", 1),
	}
	for _, source := range invalid {
		requireV1Rejected(t, source)
	}
}

func TestStableV1RejectsScaffoldTargetPlaceholder(t *testing.T) {
	requireV1Rejected(t, strings.Replace(stableV1Minimal,
		"deploy@example.test", "deploy@CHANGE-ME", 1))
	loadAndValidateV1(t, strings.Replace(stableV1Minimal,
		"deploy@example.test", "deploy@change-me.example.test:2222", 1))
	loadAndValidateV1(t, strings.Replace(stableV1Minimal,
		"deploy@example.test", `"deploy@[2001:db8::1]:2222"`, 1))
	for _, invalid := range []string{
		"deploy@example.test:ssh",
		"deploy@example.test:0",
		"deploy@2001:db8::1",
		"bad/user@example.test",
		"deploy@example_test",
	} {
		requireV1Rejected(t, strings.Replace(stableV1Minimal,
			"deploy@example.test", invalid, 1))
	}
}

func TestStableV1ProtectionSchedulesAreStructured(t *testing.T) {
	loadAndValidateV1(t, stableV1AllOptions)
	unsupportedScalar := strings.Replace(stableV1AllOptions,
		`schedule: { cron: "0 2 * * *", timezone: UTC }`,
		`schedule: "0 2 * * *"`, 1)
	requireV1Rejected(t, unsupportedScalar)
	invalidCron := strings.Replace(stableV1AllOptions,
		`cron: "0 2 * * *"`, `cron: "60 2 * * *"`, 1)
	requireV1Rejected(t, invalidCron)
	invalidTimezone := strings.Replace(stableV1AllOptions,
		`timezone: UTC`, `timezone: Mars/Olympus`, 1)
	requireV1Rejected(t, invalidTimezone)
}
