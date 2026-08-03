# onebox

**LLM-first production operations for an application intentionally running on one
server.**

Onebox keeps the economic and cognitive simplicity of a single box while
making deployments inspectable, resumable, and recoverable. It uses your
Docker Compose application and connects over SSH; there is no deployment agent
to install on the host.

The product contract is:

> Bring your coding agent, repository, one server, secrets, intent, and
> approval. Get typed production observation, constrained change proposals,
> and a proven deployment safety engine.

Start with the [documentation map](docs/README.md). It distinguishes current
behavior from active OpenSpec proposals. See the [product
direction](docs/product.md), stable [`onebox.run/v1` project
schema](docs/schema-v1.md), and current [MCP quick start](docs/mcp.md).

## What exists today

- A stable, explicit `onebox.run/v1` project schema. Future v1 evolution is
  additive, so upgrading Onebox will not require rewriting a valid v1 project.
- Compose loading and validation, SSH transport with known-host checking,
  state-bound plans, image pinning, and rendered diffs.
- Health-gated rolling or recreate deployments, traffic drain, verification,
  versioned releases, retention, and rollback.
- Locks, fencing, append-only journals, resume, abort, migration gates, status,
  audit, SOPS secrets, accessories, proxy operations, and notifications.
- A canonical, versioned operation graph and one shared Go service for
  observation, proposals, execution, structured events, and operational
  memory. The CLI is an adapter over that service; engine locks, fencing,
  journals, drift checks, and rollback gates remain the execution authority.
- `ob mcp` with redaction-safe observation, deployment-proposal, memory-read,
  and memory-change-proposal tools. All are read-only; there is no MCP
  production-mutation tool yet.
- The `ob` CLI as the current execution path and as a lasting adapter for local
  development, CI, support, and break-glass recovery.

PostgreSQL, MySQL, Redis, and generic service component types currently classify
Compose-owned accessories; selecting a type does not make Onebox install,
configure, upgrade, back up, or own that service. Traefik is the only specialized
managed runtime today.

The schema can already declare desired backup, restore-drill, log, metric, and
alert capabilities. The local engine does **not** manage those continuous
services yet, and reports them as declared rather than managed. The planned
dashboard/control plane will add authenticated team approvals, continuous
evidence, shared policy, and recovery assurance without becoming a generic
Docker UI.

The production-disabled managed-service framework, including version selection,
typed settings, visible defaults, durable operations, and the proposed MCP
surface, is specified in the active
[`managed-service-operation-contract`](openspec/changes/managed-service-operation-contract/).
That OpenSpec change is proposed behavior, not a shipped capability.

## Start using it

Build or install the binary into `~/.local/bin`:

```sh
just build
```

`just install` is an alias for the same target. Ensure `~/.local/bin` is on
`PATH`; set `OB_BIN_DIR` to use another destination. Run `just --list` to see
the available build, test, formatting, and check targets.

Onebox releases use `vYEAR.MONTH.SEQUENCE`, for example `v2026.08.1`. The
sequence increases for each release in a UTC calendar month. Checkout builds
use Git-derived provenance and remain visibly distinct from a release.
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

Both commands also support `--json`.

From a repository with a working Compose file, scaffold and inspect `ob.yml`:

```sh
ob init
ob validate
ob config
```

Review production without changing it:

```sh
ob plan --out ob-plan.json
```

Create a short-lived approval for that exact plan, then deploy with both
artifacts:

```sh
ob approve --plan ob-plan.json --out ob-approval.json
ob deploy --plan ob-plan.json --approval ob-approval.json
```

The plan is a mode-`0600`, digest-protected executable envelope containing the
typed operation graph and exact config, Compose, host-state, image, rendered
Compose, and payload bindings. It expires after 15 minutes; any drift or local
payload change requires a new plan.

`ob init` is a starting point, not permission to deploy. Review component
types, persistence semantics, readiness, job data effects, and the environment
target before running a plan. The [schema guide](docs/schema-v1.md) contains a
complete example.

## Execution contracts

Executable plans use
`onebox.run/executable-deploy-plan/v1alpha2` and include the planner's version,
source revision, build time, dirty state, and supported schemas. Schema-less
and unsupported plans are rejected. Environment policy can set
`minimum_onebox_version` using the exact CalVer release form and can set
`minimum_plan_schema`; `ob doctor` reports whether the runner selected by
`PATH` is compatible. When a minimum version is configured, commit-derived and
dirty checkout builds fail closed because they are not released runners.

`ob approve` writes a mode-`0600`, digest-bound grant covering the plan,
target, inputs, risk, operator, and expiry. A changed or expired plan needs a
new grant. When approval policy is enabled, migrations and unknown data
effects use the strong ceremony, where the operator types the release ID.

For automation, `--output` accepts `human`, `json`, or `ndjson`:

```sh
ob plan --output json --out ob-plan.json
ob deploy --output ndjson --plan ob-plan.json --approval ob-approval.json
ob status --output json
```

Plans and status produce versioned documents. A JSON deploy buffers ordered
operation events and its result into one envelope; NDJSON streams event records
and a terminal result/error record. Diagnostics stay on stderr.

When environment policy sets `require_migration_backup: true`, the executable
plan binds protected resources, evidence age, restore-test requirements, and
key-material names. Seal externally validated, secret-free facts into a
plan-bound receipt and apply it with the plan:

```sh
ob backup-evidence create --plan ob-plan.json --manifest backup-facts.json --out ob-backup-evidence.json
ob deploy --plan ob-plan.json --approval ob-approval.json --backup-evidence ob-backup-evidence.json
```

The facts manifest uses `onebox.run/migration-backup-facts/v1alpha1` and records
artifact, integrity, restore-test, and key-usability facts, never backup bytes
or secrets. An audited override requires the exact plan's strong or
break-glass grant:

```sh
ob deploy --plan ob-plan.json --approval ob-approval.json --override-migration-backup "incident reason"
```

Pre-release jobs can write JSON or `key=value` data to `$OB_RESULT_FILE` using
the `onebox.run/job-result/v1alpha1` protocol. Provider-aware evidence records
`changed`, `provider`, and ordered `before_revisions`/`after_revisions`; Atlas
results must extend history without rewriting it. A missing or invalid result
from a migration becomes `changed=unknown` and halts before workload
replacement unless a strong or break-glass grant authorized that exact plan.

External URL verification can assert allowed status codes, exact response
headers, and scalar JSON values. Migration verification can bind the expected
provider and applied revisions to the captured job-result evidence:

```yaml
verification:
  - url: https://app.example.com/healthz
    status_codes: [200]
    required_headers:
      X-App-Ready: "yes"
    json_assertions:
      - path: service.ready
        equals: true
  - migration_revisions:
      job: migrate
      provider: atlas
      applied_revisions: ["202607130001"]
```

## LLM-first

The CLI is the interface, for people and for agents alike. It is deterministic,
composable in CI, easy to test, and it calls one canonical operations service
that owns every lifecycle decision.

`ob mcp` still ships and is described in [docs/mcp.md](docs/mcp.md), but MCP is
no longer the intended product interface: the surface is read-only, so every
mutation already goes through the CLI, and an agent able to run `ob deploy` in a
shell was never constrained by a read-only tool list. Its withdrawal is
[product direction](docs/product.md), not yet a shipped change.

The historical framing below described MCP as the primary surface;
it is not the target product experience.

Connect Claude, Codex, or another MCP client using [docs/mcp.md](docs/mcp.md).

## Scope

Onebox operates one application per environment, on one active production host.
That application may have as many workloads as it needs — a server, workers,
jobs, databases, caches, and a proxy — but a host runs one Onebox application,
not several. It is not a cluster manager, Kubernetes replacement, hosting
provider, or high-availability system. Rolling deployment can avoid application interruption while the box is
healthy; it cannot make a failed physical host available.

Compose is the runtime contract. Onebox can operate any containerized
application inside that supported envelope when its health, rollout,
persistence, and job semantics are declared honestly. It cannot make arbitrary
external side effects universally reversible.

## Development

```sh
go test ./...
go vet ./...
```

The opt-in Docker end-to-end suite is run with:

```sh
OB_E2E=1 go test ./e2e/
```
