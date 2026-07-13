# onebox

**MCP-first production operations for applications intentionally running on one
server.**

Onebox keeps the economic and cognitive simplicity of a single box while
making deployments inspectable, resumable, and recoverable. It uses your
Docker Compose application and connects over SSH; there is no deployment agent
to install on the host.

The product contract is:

> Bring your coding agent, repository, one server, secrets, intent, and
> approval. Get typed production observation, constrained change proposals,
> and a proven deployment safety engine.

See the [product specification](docs/product.md), the stable
[`onebox.run/v1` project schema](docs/schema-v1.md), and the
[MCP quick start](docs/mcp.md).

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

The schema can already declare desired backup, restore-drill, log, metric, and
alert capabilities. The local engine does **not** manage those continuous
services yet, and reports them as declared rather than managed. The planned
dashboard/control plane will add approvals, evidence, policy, and recovery
assurance without becoming a generic Docker UI.

## Start using it

Build the binary:

```sh
mkdir -p ./bin
go build -o ./bin/ob ./cmd/ob
```

Add this repository's `bin` directory to `PATH`, or invoke the binary by its
absolute path.

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

Apply exactly that state-bound plan when you are ready:

```sh
ob deploy --plan ob-plan.json
```

The plan is a mode-`0600`, digest-protected executable envelope containing the
typed operation graph and exact config, Compose, host-state, image, rendered
Compose, and payload bindings. It expires after 15 minutes; any drift or local
payload change requires a new plan.

`ob init` is a starting point, not permission to deploy. Review component
types, persistence semantics, readiness, job data effects, and the environment
target before running a plan. The [schema guide](docs/schema-v1.md) contains a
complete example and the one-time mapping from the earlier alpha shape.

## MCP-first, not MCP-only

An MCP-capable agent should be the normal conversational interface. MCP earns
that role by returning typed, secret-safe state and immutable proposals rather
than asking a model to interpret arbitrary shell output.

The CLI remains useful because it is deterministic, composable in CI, easy to
test, and available if an MCP client is down. It now calls the same canonical
operations service used by MCP-facing reads and proposals; mutation remains
local-only until approval-bound MCP execution is implemented.

Connect Claude, Codex, or another MCP client using [docs/mcp.md](docs/mcp.md).

## Scope

Onebox supports several applications, workers, jobs, databases, caches,
volumes, and a proxy on one active production host per environment. It is not a
cluster manager, Kubernetes replacement, hosting provider, or high-availability
system. Rolling deployment can avoid application interruption while the box is
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

The deeper engine design and prior review history remain in
[docs/design.html](docs/design.html).
