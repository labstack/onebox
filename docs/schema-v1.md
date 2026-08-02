# Onebox project schema v1

> Status: implemented authoring contract for the current binary
>
> Fields described only by an active OpenSpec change are not accepted yet. See
> the [documentation authority map](README.md).

`onebox.run/v1` is the stable authoring contract for a single-host Onebox
project. Put it in `ob.yml` beside the Compose file it describes. Onebox also
accepts the same shape as `ob.cue`.

The project file records the operational facts that Compose cannot express:
which services are applications, workers, jobs, or data services; how they are
released; which state is durable; what verifies a release; and which actions an
agent may propose. Compose remains the container and networking contract.

Onebox loads services from every Compose profile so profile-gated migration and
maintenance jobs remain available when explicitly targeted. Every declared
service must therefore have a component classification. Use a production
Compose contract rather than leaving development-only profile services in the
file that Onebox operates.

## Canonical example

This example assumes `compose.yaml` contains services named `api`, `worker`,
`migrate`, `db`, and `cache`. Replace the target and service names with those
from your project.

```yaml
api_version: onebox.run/v1

app: ledger
compose: compose.yaml

environments:
  production:
    target: deploy@app.example.com
    policy:
      require_approval: true
      allow_agent_proposals: true
      minimum_onebox_version: v2026.08.1
      minimum_plan_schema: onebox.run/executable-deploy-plan/v1alpha2
      require_migration_backup: true
      migration_backup_max_age: 24h
      require_migration_restore_test: true
      migration_backup_key_material: [application-key]

components:
  web:
    type: application
    service: api
    deployment:
      strategy: rolling
      replicas: 2
    readiness:
      http: /healthz
      port: 8080
      interval: 2s
      within: 2m
      retries: 3
    drain:
      signal: TERM
      wait: 5s
      grace: 30s

  worker:
    type: worker
    service: worker
    deployment:
      strategy: recreate

  migrate:
    type: job
    service: migrate
    data_effect: migration

  database:
    type: postgres
    service: db
    persistence:
      mode: durable
      volumes: [postgres-data]
    protection:
      backup:
        schedule:
          cron: "0 */6 * * *"
          timezone: UTC
        retention_days: 14
      restore_drill:
        schedule:
          cron: "0 3 1 * *"
          timezone: UTC

  cache:
    type: redis
    service: cache
    persistence:
      mode: ephemeral

deployment:
  order: [web, worker]
  retain_releases: 5
  migration_policy: manual

runtime:
  env_files: [.env.production]
  preflight:
    - file: .env.production
      require: [DATABASE_URL, SECRET_KEY_BASE]

verification:
  - component: web
    http: /healthz
    port: 8080
  - url: https://app.example.com/healthz
    status_codes: [200]
    required_headers:
      Content-Type: application/json
    json_assertions:
      - path: service.ready
        equals: true
  - migration_revisions:
      job: migrate
      provider: atlas
      applied_revisions: ["202607130001", "202607130002"]

notifications:
  webhook: https://alerts.example.com/onebox
  on: [failure]

proxy:
  kind: none

secrets:
  sops: secrets.production.yaml

observability:
  logs:
    enabled: true
    retention_days: 14
  metrics:
    enabled: true
  alerts:
    unhealthy_after: 2m
```

Run these locally before touching production:

```sh
ob validate
ob config
ob plan --out ob-plan.json
```

`ob validate` checks the project schema, Compose service references, and rollout
constraints. `ob config` prints the normalized configuration. `ob plan` reads
the host and creates a mode-`0600`, digest-bound executable operation envelope;
it does not deploy. The envelope expires after 15 minutes and binds the v1
config, root Compose file, host state, image pins, rendered Compose, and staged
payload, so a changed input must be planned again.

The current executable artifact schema is
`onebox.run/executable-deploy-plan/v1alpha2`. Missing, schema-less, or other
unsupported schema versions are rejected before Onebox connects to the target;
regenerate them with the PATH-selected current binary rather than editing an
artifact.

## Compatibility promise

`onebox.run/v1` evolves additively:

- A project valid under v1 continues to be accepted by newer Onebox releases.
- Existing fields keep their meaning and existing component types keep their
  behavior.
- New capabilities may appear as optional fields, sections, or component
  types. Existing projects do not need to add them.
- Removing a field, changing its meaning, or adding a new requirement needs a
  new API version and an explicit migration path.

An older Onebox binary can still reject a field introduced by a newer binary.
The promise is that upgrading Onebox will not force you to rewrite an existing
v1 project file.

## Top-level fields

| Field | Purpose |
|---|---|
| `api_version` | Required; exactly `onebox.run/v1` |
| `app` | Compose project and remote state name; defaults from the directory |
| `compose` | Compose file path; defaults to a conventional Compose filename |
| `environments` | Named single-host targets and agent policy |
| `components` | Explicit operational model of Compose services |
| `deployment` | Release order, retention, and migration compatibility |
| `runtime` | Shipped environment files and host preflight requirements |
| `hooks` | Explicit operator commands at supported lifecycle seams |
| `verification` | Container, HTTP, and external release checks |
| `notifications` | Outcome webhook settings |
| `proxy` | Existing or Onebox-managed proxy contract |
| `secrets` | SOPS-encrypted application secret source |
| `registry` | Private-registry login using a password environment variable |
| `observability` | Desired log, metric, and alert capabilities |

Unknown fields are rejected so spelling mistakes do not silently weaken an
operational policy.

## Environments and agent policy

Each environment has exactly one `target` in SSH form, such as
`deploy@example.com` or `deploy@example.com:2222`. DNS names and IPv4 addresses
may be written directly; IPv6 addresses are bracketed, for example
`deploy@[2001:db8::1]:2222`. Ports are numeric from 1 through 65535. Usernames
may contain letters, digits, dot, underscore, and hyphen and cannot begin with
dot or hyphen.

```yaml
environments:
  staging:
    target: deploy@staging.example.com
    policy:
      require_approval: true
      allow_agent_proposals: true
      minimum_onebox_version: v2026.08.1
      minimum_plan_schema: onebox.run/executable-deploy-plan/v1alpha2
      require_migration_backup: true
      migration_backup_max_age: 24h
      require_migration_restore_test: true
      migration_backup_key_material: [application-key]
```

The two agent/approval switches default to `true` when omitted:

- `require_approval` declares that consequential operations need human
  approval. CLI deploy execution enforces an exact, unexpired plan-bound grant.
- `allow_agent_proposals` controls whether the MCP may prepare deployment
  proposals for that environment. Set it to `false` for environments an agent
  may observe but not plan against.
- `minimum_onebox_version` uses the exact CalVer release form
  `vYEAR.MONTH.SEQUENCE`, for example `v2026.08.1`. Year, month, and the
  per-month sequence are compared numerically. A commit-derived or dirty
  development build is deliberately rejected when a minimum is configured.
- `minimum_plan_schema` rejects an older executable-plan contract during
  planning and execution. Use `ob version` and `ob doctor` to inspect the
  runner selected by `PATH`.
- `require_migration_backup` is opt-in. When true,
  `migration_backup_max_age` is required, restore-test evidence defaults to
  required, and `migration_backup_key_material` names any protected keys that
  need separate integrity and usability evidence. It also requires
  `require_approval: true` so an override cannot bypass the approval ceremony.

Create and apply a local approval grant with:

```sh
ob approve --plan ob-plan.json --out ob-approval.json
ob deploy --plan ob-plan.json --approval ob-approval.json
```

The mode-`0600` grant is digest-protected and binds the plan and operation
digests, application, environment, target, config, Compose, observed/live
state, payload, risk, approval class, operator, and expiry. A local grant proves
the explicit CLI ceremony; it is not an external identity-provider signature.
The MCP remains read-only and cannot mint or consume approval authority.

## Components

The map key is the stable logical component name shown to users and agents.
`service` names the Compose service and defaults to the component name.

| Type | Required capability | Meaning today |
|---|---|---|
| `application` | `deployment` | Request-serving or general application workload |
| `worker` | `deployment` | Background workload using the same release engine |
| `job` | `data_effect` | Run-to-completion Compose service, including migrations |
| `postgres` | `persistence` | PostgreSQL data service |
| `mysql` | `persistence` | MySQL data service |
| `redis` | `persistence` | Redis cache or durable data service |
| `service` | none | Generic long-running supporting service |

### Supporting-service ownership today

`postgres`, `mysql`, `redis`, and `service` are operational classifications of
services authored in the Compose project. They let Onebox validate persistence,
keep supporting services out of application release choreography, check them as
deployment prerequisites, and converge them through the explicit accessory
maintenance path. They do not select an image, install a service, expose
provider settings, create backups, perform upgrades, or transfer lifecycle
ownership to Onebox.

Choose the image version and all native configuration in Compose today. A
current component does not accept a `managed` field; the closed schema rejects
it as unknown.

The active
[`managed-service-operation-contract`](../openspec/changes/managed-service-operation-contract/)
proposes an additive, explicitly owned managed-service envelope with a
versioned driver, immutable profile, explicit image, typed settings, bounded
native parameters, resource controls, and per-slot secrets. That example is
non-executable until the change ships. Compose-owned services remain the full
control escape hatch after managed services are introduced.

### Workload deployment

`application` and `worker` components choose an explicit strategy:

```yaml
deployment:
  strategy: rolling # or recreate
  replicas: 2       # default: 1
```

`rolling` requires two copies to coexist and a readiness signal, either in
`readiness` or in the Compose health check. Fixed container names, published
host ports, and other exclusive resources can make a service non-rollable;
`ob validate` reports the exact conflict. Use `recreate` when overlap is not
safe.

`readiness` is exactly one of:

```yaml
readiness:
  http: /healthz
  port: 8080
  within: 2m
```

```yaml
readiness:
  exec: /app/bin/ready
  within: 2m
```

Omit `readiness` to adopt the Compose service's health check. An HTTP probe
always includes its container port; HTTP and exec cannot be combined. `drain`
describes how the old instance leaves traffic before it stops. Durations use
Go-style units (`ns`, `us`/`µs`, `ms`, `s`, `m`, `h`) or whole days, and may
be compound or fractional, such as `500ms`, `1.5s`, `1h30m`, or `14d`.

For an explicitly declared readiness probe, omitted timing values resolve to a
2-second interval, 5-second start period, 120-second gate timeout, and three
retries. An adopted Compose health check retains its interval, start period,
and retry tuning. `drain.grace` defaults to 30 seconds; an omitted `drain.wait`
adds no fixed wait. Recreate workloads receive the declared drain signal before
`drain.wait`, then Compose replacement uses `drain.grace` as its TERM-to-KILL
timeout. Rolling workloads first leave traffic, optionally receive a declared
non-TERM bleed signal, wait `drain.wait`, and finally receive TERM from
`docker stop` with `drain.grace`, one retiring replica at a time.

### Jobs and data effects

Every job states its production data consequence:

- `none`: the job does not change durable production data.
- `migration`: the job may change an application data schema.
- `unknown`: Onebox must treat rollback safety as unknown.

`none` is an operator declaration that keeps application rollback open after a
successful job. A missing or invalid result for a migration becomes
`changed=unknown`: automatic rollback is unavailable, and workload replacement
halts unless the exact plan carries strong or break-glass approval for that
transition.

Jobs run before workload release. With rolling workloads, the previous code is
still serving while a migration runs and old/new replicas coexist during the
roll. Database changes must therefore be backward-compatible with both versions
regardless of migration policy. `manual` controls rollback behavior; it does
not create a maintenance window or make an incompatible migration safe.

`command` optionally overrides the default `docker compose run` behavior with
an explicit hook. Onebox mounts `$OB_RESULT_FILE` into containerized jobs. The
job should write either strict key/value data or
`onebox.run/job-result/v1alpha1` JSON. A generic result needs `changed`; a
provider-aware result also needs ordered `before_revisions` and
`after_revisions`:

```json
{
  "schema_version": "onebox.run/job-result/v1alpha1",
  "changed": true,
  "provider": "atlas",
  "before_revisions": ["202607130001"],
  "after_revisions": ["202607130001", "202607130002"]
}
```

Revision identifiers must be unique, `changed` must agree with the lists, and
Atlas history must extend the before-list without rewriting it. Only normalized
revision identifiers and their evidence digest enter journals; command output
and connection details do not.

`deployment.migration_policy: expand-only` is an operator assertion that
`migration`-labeled jobs stay compatible with the previous release. It never
covers `unknown` jobs or untyped `pre_release`/`post_release` hooks. Use
`manual` unless that contract is genuinely enforced by the application team.

### Persistence and protection

Persistence has one of three meanings:

- `durable`: the state must survive container and release replacement.
- `ephemeral`: the component is safe to recreate empty.
- `external`: state is owned outside the Compose project.

Named `volumes` make the intended durable inventory explicit and must match
named-volume mounts on that component's Compose service; typos are rejected.
PostgreSQL, MySQL, and Redis require a persistence classification; application
and generic service components may declare one when they own state.

`protection` declares the desired backup schedule, retention, and restore-drill
cadence. Every schedule has an explicit five-field numeric cron expression and
IANA timezone. Fields support numeric values, ranges, lists, wildcards, and
steps:

```yaml
protection:
  backup:
    schedule:
      cron: "0 2 * * *"
      timezone: America/Los_Angeles
    retention_days: 14
  restore_drill:
    schedule:
      cron: "0 3 1 * *"
      timezone: UTC
```

Omit `backup` or `restore_drill` when that capability is not requested. The
structured schedule avoids host-local timezone ambiguity and gives future
planners a stable value to validate and display. It is intentionally part of
v1 now so projects will not need another shape migration when managed
protection arrives. **The current local engine does not schedule backups or
restore drills and does not report declared protection as managed.** Continue
operating and testing your existing backup system until Onebox implements and
verifies that capability.

Migration backup evidence is a separate execution gate. When the environment
policy requires it, the executable plan binds every durable/external resource,
freshness, restore-test requirement, and named key material. Onebox does not run
a backup: an external process supplies validation facts, which the CLI seals
into a plan-bound receipt:

```sh
ob backup-evidence create \
  --plan ob-plan.json \
  --manifest backup-facts.json \
  --out ob-backup-evidence.json

ob deploy --plan ob-plan.json --approval ob-approval.json \
  --backup-evidence ob-backup-evidence.json
```

The facts manifest uses
`onebox.run/migration-backup-facts/v1alpha1` and records artifact identifiers,
creation times, integrity digests/methods, restore-test facts, and key-material
usability facts—never backup bytes, keys, or credentials. The receipt is
freshness-checked and bound to the exact plan, target, resources, and key names.
For audited break-glass use, `--override-migration-backup "reason"` replaces
`--backup-evidence`; the two flags are mutually exclusive, and the override
requires the exact plan's strong approval grant.

## Deployment and runtime

`deployment.order` lists every `application` and `worker` component in
dependency order; do not repeat entries. Data services and jobs do not belong
in this list.
`retain_releases` defaults to 5. `migration_policy` defaults to `manual`.

`runtime.env_files` participate in Compose interpolation and are shipped with
the release. They must stay inside the Compose project directory. Values are
not printed by safe plan and MCP surfaces. `runtime.preflight` asserts that a
file exists and contains required or merely reported keys before deployment.

## Verification, hooks, and notifications

Each verification entry is exactly one external URL check or one component
HTTP/exec check:

```yaml
verification:
  - url: https://app.example.com/healthz
    status_codes: [200, 204]
    required_headers:
      Content-Type: application/json
    json_assertions:
      - path: service.ready
        equals: true
  - component: web
    http: /healthz
    port: 8080
  - component: worker
    exec: /app/bin/smoke
  - migration_revisions:
      job: migrate
      provider: atlas
      applied_revisions: ["202607130001", "202607130002"]
```

URL, component HTTP, and component exec forms cannot be combined in one entry.
On a URL check, `status_codes` defaults to any 2xx response,
`required_headers` uses exact values, `contains` asserts a body marker, and
`json_assertions` compares scalar values at dotted object/array paths.
`advisory: true` reports failure without making the release fail. A
`migration_revisions` entry is exclusive of HTTP/exec/URL forms and compares
the complete expected revision list with provider evidence captured from the
named job's `$OB_RESULT_FILE`. Keep checks narrow, deterministic, and free of
secret-bearing URLs.

Top-level `hooks` is closed to `bootstrap`, `pre_release`, `post_release`, and
`post_deploy`. A job-specific command belongs on that job component:

```yaml
components:
  migrate:
    type: job
    service: migrate
    data_effect: migration
    command: docker compose run --rm --no-deps migrate

hooks:
  post_deploy: scripts/announce-release.sh
```

A map-form hook with `local: true` runs on the operator machine. It receives
`OB_TARGET` as an OpenSSH destination plus `OB_SSH_USER`, `OB_HOST`, and
`OB_SSH_PORT` separately. For example, use
`ssh -p "$OB_SSH_PORT" "$OB_TARGET" ...`. DNS names and IPv4 can also use
`"$OB_TARGET:/path"` with rsync and pass the port through its remote shell.
IPv6 remote-spec parsing differs between GNU rsync and macOS openrsync, so
hooks should construct the form accepted by their installed rsync from the
separate user and host values. The port is never appended to `OB_TARGET`,
because OpenSSH treats `user@host:port` as a hostname rather than a port.

Hooks and job commands are operator-authored shell and therefore have weaker
planability than typed operations. The MCP hides their bodies and can mark a
proposal as not ready for agent-only approval.

Notifications send a generic outcome webhook. `on` defaults to `[failure]` and
may include `success`; delivery is fail-open so an alerting outage does not
change the deployment result.

For agents and CI, the root `--output` flag accepts `human` (default), `json`,
or `ndjson`. Structured `plan` emits the executable plan, `status` emits a
`onebox.run/status/v1alpha1` envelope, and mutation commands emit ordered
redaction-safe events plus their result/error. JSON uses one
`onebox.run/cli-operation/v1alpha1` envelope; NDJSON streams
`onebox.run/cli-record/v1alpha1` event records followed by a result or error.
Structured deploy requires `--plan`, keeping automation on the same immutable
reviewed artifact as human execution.

## Observability status

`observability.logs`, `observability.metrics`, and `observability.alerts`
declare desired product capabilities. They let agents and the future control
plane reason about the requested operating posture without inventing policy
from Compose labels.

They do not currently install a log backend, metrics database, collector, or
alert scheduler. The existing CLI can fetch container logs and current status,
and notifications can push operation outcomes, but continuous observability is
not yet a managed Onebox service. MCP responses distinguish declared settings
from managed capability.

## Secrets and private registries

`secrets.sops` points to a SOPS-encrypted flat YAML map. The execution path
decrypts it runner-side and ships a mode-600 environment file. Secret values
must not appear in `ob.yml`, plans, MCP responses, or command arguments.

Private registry login takes its password from the named local environment
variable:

```yaml
registry:
  server: ghcr.io
  username: acme-deploy
  password_env: GHCR_TOKEN
```

The value travels through stdin and is never stored in the project schema.
