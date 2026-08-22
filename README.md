<div align="center">

# onebox

**Plan-before-apply deploys. Zero downtime. One box.**

<!-- One badge per line renders as one badge per row: GitHub turns a single
     newline inside a paragraph into a line break. Same for the links below. -->
[![CI](https://img.shields.io/github/actions/workflow/status/labstack/onebox/ci.yml?branch=main&label=ci&color=4f9a3c&labelColor=22291f)](https://github.com/labstack/onebox/actions/workflows/ci.yml) [![Release](https://img.shields.io/github/v/release/labstack/onebox?label=release&color=4f9a3c&labelColor=22291f)](https://github.com/labstack/onebox/releases) [![Licence](https://img.shields.io/badge/licence-Apache--2.0-4f9a3c?labelColor=22291f)](LICENSE)

[Documentation](https://onebox.run) · [Your first deploy](https://onebox.run/start/first-deploy) · [What it refuses](https://onebox.run/explanation/what-onebox-refuses) · [Shipped vs proposed](https://onebox.run/status/capabilities)

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/media/deploy-dark.svg">
  <img src="docs/media/deploy-light.svg" width="760" alt="An example Onebox session: ob plan prints a sealed diff of two image changes, a new workload and a migration whose data effect is unknown, then ob deploy rolls the workloads, verifies, and finishes with release r-0042 serving.">
</picture>

<sub>A rendering of an example session, not a recording of one.</sub>

</div>

---

**Production operations for an application intentionally running on one
server.**

Onebox keeps the economic and cognitive simplicity of a single box while making
deployments inspectable, resumable, and recoverable. You describe what your
application *is* in `ob.yml`; Onebox derives the Compose runtime, the names, the
routing, and the supporting services. It connects over SSH — there is no
deployment agent to install on the host.

Nothing runs against production until you have read the exact change and
approved that exact plan.

## The smallest complete project

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/labstack/onebox/main/docs/onebox.run-v1.schema.json
api_version: onebox.run/v1
app: shop
environments:
  production:
    server: root@203.0.113.10
image: ghcr.io/acme/shop:1.4.0
domain: shop.example.com
port: 3000
```

That is a complete project. It derives one application workload, its container
name, the Traefik router and service, TLS, the release layout under
`/var/lib/ob/shop`, and a retention policy. `ob canonical` prints every one of
those decisions with `# default`, `# shorthand` or `# override` beside it,
because the difference between a value someone chose and one that appeared by
itself is what a person checking a production configuration needs to see.

A Compose file you wrote cannot be the contract. Compose is an artifact Onebox
generates from the declaration, digest-bound into the plan, printable with
`ob preview`, and permanently ejectable with `ob eject`. `ob init` scaffolds a
project from an existing Compose file, and individual services can be adopted
with `compose: docker-compose.yml#service`.

## Install

Released archives and Linux packages come from GitHub Releases, macOS installs
through Homebrew, and Windows through Scoop. Follow the verified steps in the
[installation guide](https://onebox.run/start/install), then confirm which
runner will execute plans:

```sh
ob version
ob doctor
```

Both commands also take `--output json`.

To build from a checkout:

```sh
just build      # lands in ./bin/ob, deliberately not on PATH
just install    # copies it to ~/.local/bin and prints what it will answer to
```

A checkout build that shadows an installed release makes `ob` mean the working
tree, and the difference only surfaces when someone is already confused about
which binary produced a result. `OB_BIN_DIR` changes where the build lands and
`OB_INSTALL_DIR` where the install goes; `just --list` shows the rest.

## The four commands

```sh
ob validate    # no side effects, no target contacted
ob plan --out ob-plan.json
ob approve --plan ob-plan.json --out ob-approval.json
ob deploy --plan ob-plan.json --approval ob-approval.json
```

The plan is a mode-`0600`, digest-protected executable envelope containing the
typed operation graph and exact config, Compose, host-state, image, rendered
Compose, and payload bindings. It expires after 15 minutes; any drift or local
payload change requires a new plan.

`ob approve` writes a mode-`0600`, digest-bound local confirmation covering the
plan, target, inputs, risk, operator label, and expiry. It is tamper-evident,
but it is not authenticated identity or an independently issued capability. When
approval policy is enabled, migrations and unknown data effects use the strong
ceremony, where the operator types the release ID.

`ob init` is a starting point, not permission to deploy. Review workload types,
persistence semantics, readiness, job data effects, and the environment server
before running a plan. Two rules catch most first drafts: a job with no explicit
`when` defaults to `manual`, so it never runs during a deploy — declare
`when: pre_release` or `when: post_release` for a migration; and a rolling
workload cannot publish a host port, because two replicas cannot hold the same
port during a roll.

## What it refuses

- `strategy: rolling` with no health check, at load.
- A cron expression whose meaning cannot be preserved, at load.
- A service driver it cannot run. Eleven are supported — postgres, mysql,
  mariadb, redis, valkey, mongodb, rabbitmq, minio, meilisearch, clickhouse,
  nats — and a twelfth name is refused rather than guessed at, because inventing
  an image from a name produces a container that starts and stores nothing
  durable.
- Top-level workload shorthand mixed with a `workloads` block
  (`shorthand_and_workloads`): it would be ambiguous which workload the
  top-level fields describe.
- A second application on a host another already owns (`host_owner_mismatch`).
  Ownership is recorded by `ob bootstrap` and checked by every mutating command.
- A backup policy on a driver that cannot honour one, and a major version change
  a driver cannot perform in place — rather than replacing the container and
  leaving the data intact and unreachable.

There is no generic `--force`. Each override grants exactly one capability and
is named for it:

| Flag | Grants |
| --- | --- |
| `--break-lock` | break a stale application or host lock after inspecting its holder |
| `--break-migration-gate` | abort past a closed migration gate you have judged safe |
| `--allow-destructive-mounts` | apply a service change that detaches or replaces a data volume |
| `--no-rollback` | leave a failed deploy in place instead of recovering it |
| `--redeploy` | replace workloads that a no-op deploy would otherwise leave alone |

## What it does not do today

**Backups cover managed services, not workload volumes.** A service declaring
`backup` — PostgreSQL today — is archived continuously to a repository you own
and can be recovered to a point in time. A workload's own volume is not, and
`ob doctor` says so for every workload holding durable data, because silence
there would read as approval.

**`mongodb` runs a standalone server, not a replica set.** An application
needing change streams or multi-document transactions will connect,
authenticate, and then fail — in its own logs, not in anything Onebox says.

**One host, and no failover.** Rolling deployment can avoid interruption while
the box is healthy; it cannot make a failed physical host available. Onebox is
not a cluster manager, Kubernetes replacement, PaaS, or hosting provider, not
multi-host or multi-region, and not a way to run several independent
applications side by side on one host.

[Shipped vs proposed](https://onebox.run/status/capabilities) is the full
account of what the binary does today versus what the schema merely accepts.

## Driven by people and agents alike

The CLI is the interface for both. It is deterministic, composable in CI, easy
to test, and it calls one canonical operations service that owns every lifecycle
decision. `--output` accepts `human`, `json`, or `ndjson`, and every finite
machine result uses one `onebox.run/cli/v1alpha1` envelope with
`schema_version`, `command`, `outcome`, and exactly one of `data` or `error`.
Outcomes are `success`, `no_op`, `cancelled`, or `error`; cancellation exits 2
and errors exit 1. Errors are typed — branch on the code, never on the sentence.

There is no MCP surface. A read-only tool list constrains nothing when every
mutation goes through the CLI anyway and the agent can run `ob deploy` in a
shell. Point an agent at the `ob` binary the way you would point it at `gh`.

## Documentation

**[onebox.run](https://onebox.run)** is the documentation, published from
[`site/`](site). Every page is also served as clean Markdown at `<path>.md`, and
[llms.txt](https://onebox.run/llms.txt) maps the whole site for agents. Build it
with `just site-build`, or serve it locally with `just site`.

- **Start here** — [your first deploy](https://onebox.run/start/first-deploy)
- **Reference** — the [project file](https://onebox.run/reference/project-file),
  every [field](https://onebox.run/reference/fields/top-level), every
  [CLI command](https://onebox.run/reference/cli), every
  [error code](https://onebox.run/reference/errors), and the
  [policies](https://onebox.run/reference/policies) that carry the output matrix
  and version gates. The field, CLI and error pages are generated from the
  binary by `cmd/ob-docgen`, so they cannot describe something the loader does
  not accept.
- **Operations** — [secrets](https://onebox.run/guides/handle-secrets),
  [migrations](https://onebox.run/guides/run-migrations),
  [backups](https://onebox.run/guides/back-up-a-database),
  [rollback](https://onebox.run/guides/roll-back), and
  [eject](https://onebox.run/guides/eject).
- **Why it works this way** — the
  [safety envelope](https://onebox.run/explanation/safety-envelope),
  [evidence, not declaration](https://onebox.run/explanation/evidence-not-declaration),
  and [the ownership boundary](https://onebox.run/explanation/ownership-boundary).
- **Direction** — [product direction](docs/product.md) gives the boundaries; the
  [documentation map](docs/README.md) says which documents are authoritative.

## Releases

Onebox uses `vYYYY.M.REVISION` — for example `v2026.8.0` for the first release
in August 2026. The year is four digits, months are unpadded, and each UTC
calendar month starts at revision zero. Checkout builds use Git-derived
provenance and remain visibly distinct from a release, and fail closed against
an environment policy that sets `min_onebox_version`.

Maintainers cut the next release with `just release`, which requires a clean,
checked, up-to-date `main` and atomically publishes a metadata-only
fast-forward release commit plus its tag.

## Development

```sh
go test ./...
go vet ./...
OB_E2E=1 go test ./e2e/   # opt-in Docker end-to-end suite
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the setup, the verification gate, and
the standard a change has to meet. Contributions require accepting the
[Contributor License Agreement](CLA.md). Security issues go to
[SECURITY.md](SECURITY.md), never to a public issue.

## License

Onebox is licensed under the [Apache License, Version 2.0](LICENSE).

Copyright 2026 LabStack LLC.
