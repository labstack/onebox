# onebox

**Production operations for an application intentionally running on one
server.**

Onebox keeps the economic and cognitive simplicity of a single box while
making deployments inspectable, resumable, and recoverable. You describe what
your application *is* in `ob.yml`; Onebox derives the Compose runtime, the
names, the routing, and the supporting services. It connects over SSH; there
is no deployment agent to install on the host.

The product contract is:

> Bring an application repository, one Linux server, secrets, intent, and
> approval. Get structured observation, constrained change proposals, and
> evidence-backed execution within a declared safety envelope.

## The project file is the authoring contract

A Compose file you wrote cannot be the contract. Compose is an artifact Onebox
generates from the declaration, digest-bound into the plan, printable with
`ob preview`, and permanently ejectable with `ob eject`. `ob init` scaffolds a
project from an existing Compose file to start from, and individual services can
be adopted with `compose: docker-compose.yml#service`.

## Documentation

**[onebox.run](https://onebox.run)** is the documentation, published from
[`site/`](site) in this repository. Every page is also served as clean Markdown
at `<path>.md`, and [llms.txt](https://onebox.run/llms.txt) maps the whole site
for agents.

Build it from a checkout with `just site-build`, or serve it locally with
`just site`.

- **Start here** — [your first deploy](site/src/content/docs/start/first-deploy.mdx)
- **Reference** — the [project file](site/src/content/docs/reference/project-file.mdx),
  every [field](site/src/content/docs/reference/fields/), every
  [CLI command](site/src/content/docs/reference/cli.mdx), and every
  [error code](site/src/content/docs/reference/errors.mdx). The field, CLI and
  error pages are generated from the binary by `cmd/ob-docgen`, so they cannot
  describe something the loader does not accept.
- **Shipped vs proposed** — [what the binary does today](site/src/content/docs/status/capabilities.mdx),
  and what the schema accepts but cannot yet execute.
- **Direction** — [product direction](docs/product.md) gives the boundaries;
  the [documentation map](docs/README.md) says which documents are authoritative.

## What exists today

- A stable, explicit `onebox.run/v1` project schema. Future v1 evolution is
  additive, so upgrading Onebox will not require rewriting a valid v1 project.
- Compose loading and validation, SSH transport with known-host checking,
  state-bound plans, image pinning, and rendered diffs.
- Health-gated rolling or recreate deployments, traffic drain, verification,
  versioned releases, retention, and rollback.
- Locks, fencing, append-only journals, resume, abort, migration gates, status,
  audit, SOPS secrets, supporting services, proxy operations, and notifications.
- A canonical, versioned operation graph and one shared Go service for
  observation, proposals, execution, structured events, and operational
  memory. The CLI is an adapter over that service; engine locks, fencing,
  journals, drift checks, and rollback gates remain the execution authority.
- The `ob` CLI as the current execution path and as a lasting adapter for local
  development, CI, support, and break-glass recovery.

Declaring `services: {postgres: 17}` makes Onebox run it: the image, a durable
volume, a health check, a credential generated on the server that never travels,
and the connection details the application reads. Eleven drivers are supported —
postgres, mysql, mariadb, redis, valkey, mongodb, rabbitmq, minio, meilisearch,
clickhouse, nats. Anything else is refused rather than guessed at, because
inventing an image from a name produces a container that starts and stores
nothing durable.

Onebox takes PostgreSQL backups: continuous WAL archiving to a repository you
own, point-in-time restore, and a drill that proves recovery without touching
the live service. Every other driver **refuses** a backup policy rather than
accepting one it cannot honour, and `ob doctor` says which is which for every
workload and service holding durable data. It also refuses a major version
change a driver cannot perform in place, rather than replacing the container
and leaving the data intact and unreachable.

The planned dashboard/control plane will add authenticated team approvals,
continuous evidence, shared policy, and recovery assurance without becoming a
generic Docker UI.

## Start using it

Released archives and Linux packages are installed from GitHub Releases,
macOS users can install through Homebrew, and Windows users can install through
Scoop. Follow the verified steps in the
[installation guide](https://onebox.run/start/install).
To build the binary from a checkout:

```sh
just build
```

It lands in `./bin/ob`, which is deliberately not on `PATH`: a checkout build
that shadows an installed release makes `ob` mean the working tree, and the
difference only surfaces when someone is already confused about which binary
produced a result.

`just install` is the deliberate step that copies it to `~/.local/bin` and
prints what it will answer to; `just clean` removes the build and `just
uninstall` the copy on PATH. `OB_BIN_DIR` changes
where the build lands and `OB_INSTALL_DIR` where the install goes. Run `just
--list` to see the available build, test, formatting, and check targets.

Onebox releases use `vYYYY.M.REVISION`, for example `v2026.8.0` for the first
release in August 2026. The year is four digits, months are unpadded, and each
UTC calendar month starts at revision zero. Checkout builds use Git-derived
provenance and remain visibly distinct from a release.
Maintainers create the next release with `just release`, which requires a
clean, checked, up-to-date `main` branch and atomically publishes a
metadata-only fast-forward release commit plus its tag to `origin`. The release
identity therefore needs permission to fast-forward `main`; a branch policy
that refuses the update aborts the atomic publication without leaving a tag.

Confirm which runner will execute plans and check the local safety setup:

```sh
ob version
ob doctor
```

Both commands also take `--output json`.

From a repository with a working Compose file, scaffold and inspect `ob.yml`:

```sh
ob init        # scaffold a project from the Compose file you have
ob validate    # no side effects, no target contacted
ob canonical   # what Onebox understood, with where each value came from
ob preview     # the Compose runtime it will generate
```

`ob canonical` marks every value you did not write with `# default`,
`# shorthand` or `# override`, because the difference between a value someone
chose and one that appeared by itself is what a person checking a production
configuration needs to see.

Review production without changing it:

```sh
ob plan --out ob-plan.json
```

Record a short-lived local confirmation for that exact plan, then deploy with both
artifacts:

```sh
ob approve --plan ob-plan.json --out ob-approval.json
ob deploy --plan ob-plan.json --approval ob-approval.json
```

The plan is a mode-`0600`, digest-protected executable envelope containing the
typed operation graph and exact config, Compose, host-state, image, rendered
Compose, and payload bindings. It expires after 15 minutes; any drift or local
payload change requires a new plan.

`ob init` is a starting point, not permission to deploy. Review workload
types, persistence semantics, readiness, job data effects, and the environment
server before running a plan.

Two rules catch most first drafts. A job with no explicit `when` defaults to
`manual`, so it never runs during a deploy — declare `when: pre_release` or
`when: post_release` for a migration. And a rolling workload cannot publish a
host port, because two replicas cannot hold the same port during a roll: use
`strategy: recreate`, or route through the proxy. The
[project file reference](site/src/content/docs/reference/project-file.mdx)
documents the accepted fields and representative examples.

## Execution contracts

Executable plans use
`onebox.run/executable-deploy-plan/v1alpha2` and include the planner's version,
source revision, build time, dirty state, and supported schemas. Schema-less
and unsupported plans are rejected. Environment policy can set
`min_onebox_version` using the exact CalVer release form and can set
`min_plan_schema`; `ob doctor` reports whether the runner selected by
`PATH` is compatible. When a minimum version is configured, commit-derived and
dirty checkout builds fail closed because they are not released runners.

`ob approve` writes a mode-`0600`, digest-bound local confirmation covering the
plan, target, inputs, risk, operator label, and expiry. A changed or expired plan
needs a new confirmation. The artifact is tamper-evident but is not authenticated
identity or an independently issued capability. When approval policy is enabled,
migrations and unknown data effects use the strong ceremony, where the operator
types the release ID.

For automation, `--output` accepts `human`, `json`, or `ndjson`:

```sh
ob plan --output json --out ob-plan.json
ob deploy --output ndjson --plan ob-plan.json --approval ob-approval.json
ob status --output json
```

Every finite machine result uses one `onebox.run/cli/v1alpha1` envelope with
`schema_version`, `command`, `outcome`, and exactly one of `data` or `error`.
Outcomes are `success`, `no_op`, `cancelled`, or `error`; cancellation exits 2
and errors exit 1. NDJSON adds monotonic sequences and exactly one terminal
record. Errors distinguish `diagnostic_command`, `next_command`, and
`resolving_command`; diagnostics stay on stderr.

Error codes come in three families, all published in the
[error code reference](site/src/content/docs/reference/errors.mdx): validation
codes raised while the project file is read, operation codes raised while a
command runs, and the lifecycle failure contract. Branch on the code, never on
the sentence.

There is no generic `--force`. Each override grants exactly one capability and
is named for it:

| Flag | Grants |
| --- | --- |
| `--break-lock` | break a stale application or host lock after inspecting its holder |
| `--break-migration-gate` | abort past a closed migration gate you have judged safe |
| `--allow-destructive-mounts` | apply a service change that detaches or replaces a data volume |
| `--no-rollback` | leave a failed deploy in place instead of recovering it |
| `--redeploy` | replace workloads that a no-op deploy would otherwise leave alone |

Logs and exec accept workloads and Onebox-run services through the same target
namespace. Follow mode is NDJSON-only; finite logs may be JSON. Their output is
operator-controlled passthrough and may contain secrets, so Onebox tags stdout
and stderr but does not claim to redact those bytes. `ob exec` requires a
bounded `--reason` and audits only safe invocation metadata plus the command
digest, never the command or output bytes.

Manual jobs use the same sealed-plan and local-confirmation boundary as deploys:

```sh
ob job plan maintenance --out ob-job-plan.json
ob approve --plan ob-job-plan.json --out ob-job-approval.json
ob job run --plan ob-job-plan.json --approval ob-job-approval.json --output ndjson
```

Each application release has a strict manifest state (`staged`, `verified`,
`serving`, `superseded`, `failed`, or `aborted`) and an explicit predecessor.
Rollback follows that predecessor; resume and abort reconcile an interrupted
checkpoint instead of inferring state from directory names.

`ob secrets push` rotates the complete declared secret graph as one opaque
generation. All affected workloads finish on the new generation or recovery
returns them all to the old one; declaration changes require a normal deploy.
Rotation is graph-wide: changing one encrypted file replaces every workload that
reads any declared secret, not only the ones whose values moved.

`ob secrets list` shows the declared graph and each entry's identifier.
`ob secrets edit <entry-id>` opens one encrypted source through SOPS; the
identifier is required whenever more than one editable source exists, because
guessing which one you meant is not a decision the tool should make.

A host belongs to exactly one application. `ob bootstrap` records the owner, and
every mutating command checks it before touching anything; a second application
is refused with `host_owner_mismatch`. The record is released once nothing of the
application remains — volumes removed, and the proxy too if it is managed — so a
partial destroy keeps it and `ob destroy` names the command that frees the host.
`ob preflight` reports the current owner and passes on an unclaimed host.

When environment policy sets `require_migration_backup: true`, the executable
plan binds protected resources, report age, restore-test requirements, and
key-material names. Ask planning to write the bound report template, have the
operator or backup tooling fill it from real observations, then bind that exact
report into the local confirmation and execution:

```sh
ob plan --out ob-plan.json --backup-report-out ob-backup-report.json
# Fill ob-backup-report.json from the backup system's actual results.
ob approve --plan ob-plan.json --backup-report ob-backup-report.json --out ob-approval.json
ob deploy --plan ob-plan.json --approval ob-approval.json --backup-report ob-backup-report.json
```

The report uses `onebox.run/backup-report/v1alpha1` and records artifact,
integrity, restore-test, and key-usability observations, never backup bytes or
secrets. It is execution input, not proof that Onebox created or independently
verified a backup. An audited override requires the exact plan's strong or
break-glass local confirmation:

```sh
ob deploy --plan ob-plan.json --approval ob-approval.json --override-migration-backup "incident reason"
```

Pre-release jobs can write JSON or `key=value` data to `$OB_RESULT_FILE` using
the `onebox.run/job-result/v1alpha1` protocol. Provider-aware evidence records
`changed`, `provider`, and ordered `before_revisions`/`after_revisions`; Atlas
results must extend history without rewriting it. A missing or invalid result
from a migration becomes `changed=unknown` and halts before workload
replacement unless a strong or break-glass local confirmation authorized that exact plan.

External URL verification can assert allowed status codes, exact response
headers, and scalar JSON values. Migration verification can bind the expected
provider and applied revisions to the captured job-result evidence:

```yaml
checks:
  url:
    - url: https://app.example.com/healthz
      status_codes: [200]
      required_headers:
        X-App-Ready: "yes"
      json_assertions:
        - path: service.ready
          equals: true
  migrations:
    - job: migrate
      provider: atlas
      applied_revisions: ["202607130001"]
```

## LLM-first

The CLI is the interface, for people and for agents alike. It is deterministic,
composable in CI, easy to test, and it calls one canonical operations service
that owns every lifecycle decision.

There is no MCP surface. A read-only tool list constrains nothing when every
mutation goes through the CLI anyway and the agent can run `ob deploy` in a
shell — a second protocol would earn no safety and cost a second contract to
keep honest. Point an agent at the `ob` binary the way you would point it at
`gh`.

## Scope

Onebox operates one application per environment, on one active production host.
That application may have as many workloads as it needs — a server, workers,
jobs, databases, caches, and a proxy — but a host runs one Onebox application,
not several. It is not a cluster manager, Kubernetes replacement, hosting
provider, or high-availability system. Rolling deployment can avoid application interruption while the box is
healthy; it cannot make a failed physical host available.

Compose is the generated runtime, not the contract you write. Onebox can
operate any containerized application inside that envelope when its health,
rollout, persistence, and job semantics are declared honestly. A container it
cannot describe can still be adopted verbatim through `compose:`, and `ob eject`
hands the whole runtime over permanently. It cannot make arbitrary external
side effects reversible.

## Development

```sh
go test ./...
go vet ./...
```

The opt-in Docker end-to-end suite is run with:

```sh
OB_E2E=1 go test ./e2e/
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the setup, the verification gate, and
the standard a change has to meet. Contributions require accepting the
[Contributor License Agreement](CLA.md).

Security issues go to [SECURITY.md](SECURITY.md), never to a public issue.

## License

Onebox is licensed under the [Apache License, Version 2.0](LICENSE).

Copyright 2026 LabStack LLC.
