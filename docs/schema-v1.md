# Onebox project schema v1

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
    contains: ok

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
the host and creates a state-bound artifact; it does not deploy.

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

The earlier unversioned/alpha shape is intentionally not accepted. Migrating
it is a one-time change:

| Earlier field | `onebox.run/v1` |
|---|---|
| `environments.<name>.hosts: [host]` | `environments.<name>.target: host` |
| `roles`, `accessories`, `jobs` | typed entries under `components` |
| role `mode` and `ready` | `deployment.strategy` and `readiness` |
| top-level `order`, `retain`, `migrations` | `deployment` |
| top-level `env_files`, `preflight` | `runtime` |
| `verify` and check `role` | `verification` and check `component` |
| `notify` | `notifications` |
| a job command under `hooks.<job>` | `components.<job>.command` |

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
```

Both policy values default to `true` when omitted:

- `require_approval` declares that consequential operations need human
  approval. The current MCP has no mutation tool, so this is also the safe
  current behavior.
- `allow_agent_proposals` controls whether the MCP may prepare deployment
  proposals for that environment. Set it to `false` for environments an agent
  may observe but not plan against.

These fields describe policy consistently across adapters. A future managed
control plane must enforce the same policy rather than creating a dashboard-only
rule.

Today this is declared control-plane policy, not a credential sandbox around
the local CLI. The CLI treats the local operator who invokes or confirms a
mutation as the authority; it does not verify a dashboard identity or signed
approval yet. Do not give an agent unrestricted shell access if it must be
unable to bypass MCP policy. Managed execution must enforce this declaration
before Onebox adds any MCP mutation tool.

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
adds no fixed wait.

### Jobs and data effects

Every job states its production data consequence:

- `none`: the job does not change durable production data.
- `migration`: the job may change an application data schema.
- `unknown`: Onebox must treat rollback safety as unknown.

`none` is an operator declaration that keeps application rollback open after a
successful job. `unknown` stays fail-closed unless that particular run writes
`changed=false` to `$OB_RESULT_FILE`.

`command` optionally overrides the default `docker compose run` behavior with
an explicit hook. A migration job should write the documented
`$OB_RESULT_FILE` result so the existing migration gate can determine whether
automatic application rollback remains safe.

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
    contains: ok
  - component: web
    http: /healthz
    port: 8080
  - component: worker
    exec: /app/bin/smoke
```

URL, component HTTP, and component exec forms cannot be combined in one entry.
On a URL check, `contains` can assert a response-body marker and
`advisory: true` reports failure without making the release fail. Keep checks
narrow, deterministic, and free of secret-bearing URLs.

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

Hooks and job commands are operator-authored shell and therefore have weaker
planability than typed operations. The MCP hides their bodies and can mark a
proposal as not ready for agent-only approval.

Notifications send a generic outcome webhook. `on` defaults to `[failure]` and
may include `success`; delivery is fail-open so an alerting outage does not
change the deployment result.

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
