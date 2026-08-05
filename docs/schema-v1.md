# The Onebox project file

> Status: the authoring contract the current binary accepts.
>
> Anything described only by an active OpenSpec change is not accepted yet. See
> the [documentation authority map](README.md).

`onebox.run/v1` is the contract for one application on one host. It goes in
`ob.yml` at the root of your repository.

You describe what your application *is*. Onebox derives what runs: the Compose
runtime, container and volume names, routing labels, health checks, the proxy,
supporting services and their credentials. You never write a Compose file for
Onebox to consume — Compose is an artifact Onebox generates, and `ob preview`
prints it before anything is deployed.

Start with the schema reference so your editor can help:

```yaml
# yaml-language-server: $schema=https://onebox.run/schema/onebox.run-v1.json
api_version: onebox.run/v1
```

`ob schema --to onebox.schema.json` writes a local copy, and `ob init` puts the
reference on the first line of a scaffolded project.

## The smallest project

```yaml
api_version: onebox.run/v1
app: shop
environments:
  production:
    server: root@203.0.113.10
image: ghcr.io/acme/shop:1.4.0
domain: shop.example.com
port: 3000
```

That is a complete project. Onebox derives a single application workload named
after the app, a container name, the Traefik router and service, TLS, the
release layout under `/var/lib/ob/shop`, and a retention policy. `ob canonical`
prints every one of those decisions with `# default` beside it.

It derives `strategy: recreate`, not rolling, because it declares no health
check: a rolling release stands the newcomer up and waits for it to report
healthy, and there is nothing here to wait for. Add `health:` and the default
becomes `rolling`. Asking for `strategy: rolling` without a health check is
refused rather than quietly downgraded.

## Shape

| Block | What it says |
|---|---|
| `app` | The application's name. Every derived name carries it. |
| `environments` | Where it runs, and the policy that governs deploying there. |
| `workloads` | The containers that are yours. |
| `services` | The supporting services Onebox runs for you. |
| `deployment` | Release order, retention, migration policy. |
| `proxy` | Who runs the proxy and what routes. |
| `runtime` | Environment files and preflight assertions. |
| `hooks` | Commands at lifecycle seams. |
| `verification` | What must be true for a release to activate. |
| `registries`, `secrets`, `notifications`, `observability` | Named maps. |

Any mapping also accepts `x-` keys. They are carried nowhere and never change
the generated runtime.

### Shorthand

A single-workload project may write the workload's own fields at the top level
instead of a `workloads` block: `build`, `image`, `compose`, `domain`, `port`,
`health`, `routes`. Mixing the two is refused, because it would be ambiguous
which workload the top-level fields describe.

Several fields accept a scalar where an object is also valid. Both forms are
permanent — a scalar form once accepted is accepted forever:

```yaml
image: nginx                     # image: {reference: nginx}
health: /healthz                 # health: {http: /healthz}
server: root@203.0.113.10        # server: {user: root, host: 203.0.113.10}
needs: [postgres]                # needs: [{name: postgres}]
services: {postgres: 17}         # services: {postgres: {version: 17}}
hooks: {post_deploy: "echo hi"}  # hooks: {post_deploy: {run: "echo hi"}}
```

## Workloads

A workload has a `role`, which decides how it is released:

| Role | Released as | Notes |
|---|---|---|
| `application` | rolling where it declares `health:`, otherwise recreate | Receives environment files. Usually routed. |
| `worker` | recreate by default | Receives environment files. Never routed unless it declares a route. |
| `daemon` | recreate | A long-running container you own — a database you still author, a sidecar. No environment files. |
| `job` | runs to completion | Declares `data_effect`. May declare `run` and `schedule`. |

Exactly one source per workload:

```yaml
workloads:
  web:
    role: application
    image: ghcr.io/acme/shop:1.4.0     # a registry reference
  api:
    role: application
    build: .                            # built elsewhere; deploy needs --image
  legacy:
    role: daemon
    compose: docker-compose.yml#legacy  # a service in a file you wrote
```

`build:` describes how to build for development. Production never builds: a
build-sourced workload needs its image resolved by whatever built it, passed as
`--image web=ghcr.io/acme/shop@sha256:…`.

`compose:` is the escape hatch. The named service is copied verbatim with
Onebox's overlay applied. It is refused if it uses `extends`, fixes
`container_name`, sets `network_mode` where a network must attach, or carries
labels in the `ob.` or `traefik.` namespaces — each of those would contradict
something Onebox owns.

### Health

```yaml
health: {http: /healthz, port: 8080, interval: 2s, start_period: 5s, within: 120s}
health: {tcp: true, port: 5432}
health: {exec: "pg_isready -U app"}     # runs through a shell
health: {exec: ["/app", "health"]}      # runs directly — the only form an
                                        # image with no shell can answer
```

Every shell-form check is drain-guarded: the rollout makes it fail before
stopping a container, so the proxy stops routing to it first. An exec-list check
cannot carry that guard, and the rollout says so rather than waiting for a flip
that cannot happen.

### Routing

`domain` and `port` together are shorthand for one route. For more than one, or
for anything non-HTTP, use `routes`:

```yaml
routes:
  - {domain: shop.example.com, path: /, port: 3000}
  - {domain: shop.example.com, path: /api, port: 3001}
  - {domain: grpc.example.com, port: 9000, entrypoint: grpc, scheme: h2c}
```

Two workloads claiming the same entrypoint, protocol, domain and path is
refused: the proxy would accept both and route to one, and nothing would say
which.

### Prerequisites

```yaml
needs: [cache]
needs: [{name: postgres, condition: healthy}]
```

The condition resolves against what the dependency can actually do — `healthy`
where it has a health check, `started` where it does not. Asking to wait for
health from something that cannot report it is refused.

`needs` also decides release order when `deployment.order` is absent.

## Services

```yaml
services:
  postgres: 17
  cache: {driver: redis, version: "7.4"}
```

Onebox runs these: the image, a durable volume, a health check, a generated
credential, and the connection details your application reads. The service's
name is its driver unless `driver` says otherwise, so a second Postgres is
`events: {driver: postgres, version: 17}`.

Drivers: `postgres`, `mysql`, `mariadb`, `redis`, `valkey`, `mongodb`,
`rabbitmq`, `minio`, `meilisearch`, `clickhouse`, `nats`. Anything else is
refused — guessing an image from a name produces a container that starts and
stores nothing durable. Run it as a `daemon` workload instead and you own it.

Three properties hold:

- **A service outlives every release.** It runs in its own Compose project, so
  no deploy and no rollback can stop it or remove its volume.
- **Its credential is generated on the target, once.** It is not in your
  project, not in the generated runtime, and not in the digest. It is never
  rotated by a re-apply.
- **Its version binds into the release digest**, so a database upgrade under an
  untouched application cannot pass unnoticed.

Onebox does not take backups. `ob doctor` says so for every workload and service
holding durable data, because silence there would read as approval.

### Reading the connection

A workload that needs a service receives `<SERVICE>_URL` and its parts —
`_HOST`, `_PORT`, `_USER`, `_PASSWORD`, `_DATABASE`. Most applications want
their own names, so say which:

```yaml
workloads:
  n8n:
    role: application
    image: docker.n8n.io/n8nio/n8n:1.70.0
    needs:
      - name: postgres
        env:
          DB_POSTGRESDB_HOST: host
          DB_POSTGRESDB_USER: user
          DB_POSTGRESDB_PASSWORD: password
services:
  postgres: 16
```

The right-hand side is a connection part: `url`, `host`, `port`, `user`,
`password`, `database`. A part the driver does not have — a database on a cache
— is omitted rather than written empty.

## Jobs and schedules

```yaml
workloads:
  migrate:
    role: job
    image: ghcr.io/acme/shop:1.4.0
    command: ["./migrate"]
    data_effect: migration
    run: pre_release
  nightly-dump:
    role: job
    image: postgres:17
    command: ["sh", "-c", "pg_dump \"$POSTGRES_URL\" | gzip > /backups/$(date -u +%F).sql.gz"]
    data_effect: none
    needs: [postgres]
    volumes: [{name: backups, path: /backups}]
    schedule: {cron: "0 2 * * *", timezone: Europe/Berlin}
```

`data_effect` is required on a job and is what the rollback gate reads.

A `schedule` becomes a timer on the host, so it fires without any Onebox process
running and survives a reboot. The cron expression is translated exactly or
refused: a form whose meaning cannot be preserved — a day-of-month and a
day-of-week together, which cron treats as "either matches" — fails at load
rather than running on days nobody chose.

## Environments and overrides

```yaml
environments:
  production:
    server: root@203.0.113.10
    policy: {require_approval: true}
  staging:
    server: root@203.0.113.20
    base_path: /srv/ob
    overrides:
      workloads:
        web: {replicas: 1, resources: {memory: 512MB}}
```

An override may change how much of something runs. It may not change what runs
or what it does to data: `image`, `build`, `compose`, `command`, `data_effect`,
`volumes` and `persistence` are refused, because a staging environment that can
swap the image is a different application wearing the same name.

Permitted: `replicas`, `resources`, `env`, `strategy`, `routes` on a workload;
`resources` and `settings` on a service.

## What Onebox generates

- **Names.** `shop_web`, `shop_web_2`, `ob_shop_postgres`,
  `ob_shop_postgres_data`, `ob_shop` (the service network), `ob-ingress`. These
  are contract: once a volume exists its name cannot change without moving data.
- **Layout.** `/var/lib/ob/<app>/releases/<id>`, `current`, `journal`,
  `services`. Configurable per environment with `base_path`.
- **The proxy.** If anything is routed, Onebox runs Traefik and writes its
  static configuration. Declare `proxy.config` to own that configuration
  instead. `proxy.kind: none` means nothing routes, and a route declared under
  it is refused.

`ob preview` prints all of it. `ob eject` writes it into your repository and
hands it over permanently — the overlay is stripped so the file is ordinary
Compose, and the workloads are repointed at it with your comments intact.

## Failures

Every failure carries a typed code, the path that produced it, and where
possible the line and the command that resolves it:

```
✗ ob: "replicaz" is not a field of this contract (line 10); did you mean "replicas"?
  at: workloads.web.replicaz
  code: unknown_field
```

`ob canonical` is the other half: it prints what Onebox understood, with
`# default`, `# shorthand` or `# override` beside every value you did not write
where it appears.

## Evolution

`api_version: onebox.run/v1` is stable. Within it:

- A field is added, never repurposed.
- A scalar form once accepted is accepted permanently.
- A default may be added; an existing default's value does not change.
- A constraint is not tightened against a project that already loads.

The JSON Schema published by `ob schema` is generated from the same declarations
the loader enforces and is checked against the conformance corpus, so what your
editor tells you while you type is what `ob validate` tells you afterwards.
