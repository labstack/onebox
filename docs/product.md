# Onebox — production operations for one application on one server

> Status: product direction
>
> Not an implementation contract. See the [documentation authority
> map](README.md) for what is shipped, and the
> [capabilities page](../site/src/content/docs/status/capabilities.mdx) for what
> the binary does today versus what the schema merely accepts.

## Product statement

Onebox is the production-operations layer for an application intentionally
running on one server. It supplies typed perception, bounded intent, durable
state, exact approval, and a fenced execution engine, so that releasing,
recovering, and maintaining that application is something a person or an agent
can do without assembling it from shell scripts.

The compact contract is:

> Bring an application repository, one Linux server, secrets, intent, and
> approval. Get structured observation, constrained change proposals, and
> evidence-backed execution within a declared safety envelope.

## The ownership boundary

> The user owns the host, application, and data. Onebox owns the operational
> state and resources it explicitly manages.

The user supplies one Linux host. Onebox owns its release state, locks, journals,
generated runtime, and any host proxy or supporting services selected in the
project. It does not silently claim infrastructure or protection it has not
actually established.

Owned application containers have one visible grammar:
`<app>-<component>-<replica>`, with a one-based replica ordinal that is never
omitted. The managed host proxy is `onebox-proxy`. These names are generated
identity, not user configuration.

The broader managed-operations goal is direction, not an inventory. Owned today:
host bootstrap, the container runtime check, the proxy and its TLS, the host
ingress network, release
staging and retention, the supporting data services and their credentials, and
scheduled jobs. **Not owned today: backups, restore proof, and log rotation.**
Onebox says so rather than implying otherwise — `ob doctor` reports the absence
of backups for every workload and service holding durable data, because silence
there would read as approval.

The distinction matters more than it looks. A product direction that reads as a
capability list is how an operator ends up believing their database is backed
up by something that has never taken a backup.

This is the organising principle behind everything else. It is why the project
file declares intent rather than describing containers, why the Compose runtime
becomes a generated artifact, and why a database is something you select rather
than something you configure.

## Scope

Onebox operates **one application per environment, on one active production
host**. That application may have as many workloads as it needs — a server,
workers, jobs, databases, caches, and a proxy — but the unit Onebox owns is the
application, and a host runs one of them.

Onebox is not:

- A cluster manager, Kubernetes replacement, PaaS, or hosting provider.
- A multi-host or multi-region orchestrator.
- A way to run several independent applications side by side on one host.
- A generic Docker dashboard, terminal, or remote shell.
- A universal infrastructure-as-code language.
- A guarantee of availability after losing the only host.
- A guarantee that arbitrary migrations or external effects can be undone.

Rolling deployment can avoid interruption while the host is healthy. It cannot
make a failed host available. Recovery onto another host is a distinct,
evidence-backed workflow, not failover.

## The interface is the CLI

The CLI is the product interface, for people and for agents. It is not a
fallback or a transitional path.

A previous direction made MCP the interface and treated the CLI as a non-primary
adapter. That was withdrawn. The MCP surface was read-only, so every mutation
went through the CLI anyway; an agent that could run `ob deploy` in a shell was
never constrained by a read-only tool list. The boundary it appeared to provide
did not exist, and maintaining two surfaces cost more than it bought.

What replaces it is a CLI built to be operated by an agent:

- Every executable command has a closed output protocol; finite and streaming
  machine records are versioned, while help and completion remain shell-native.
- Errors are typed and distinguish diagnostic, workflow-next, and resolving
  commands.
- Results stay compact, with detail behind identifiers.
- Convergence is idempotent under retry: re-applying services or the proxy
  reaches the same state. Deploy retry is mediated by the journal and resume
  rather than replayed blindly, and an expired plan is refused rather than
  re-run. `exec` is explicitly an arbitrary, reasoned, digest-audited escape
  hatch and makes no idempotency claim at all.
- Scaffolding writes a project file an agent can read and validate. Writing
  operating instructions into the repository is intent, not shipped: `ob init`
  produces `ob.yml` and nothing else.

Lifecycle behaviour lives in one canonical service. The CLI is an adapter over
it, which is what would let an HTTP surface exist later without a second
implementation of anything that matters.

## Approval is not model intent

A statement that the user approved is data, not authenticated authority. The
shipped `ob approve` ceremony records a short-lived local human confirmation
bound to the exact sealed plan, operator label, target, observed state, inputs,
risk, and expiry. Its digest detects artifact modification; it does not prove
identity or that the actor requesting the change was unable to create it.

Independent approval would require a separate trust root, enrollment,
revocation, and recovery contract. That provider is product direction, not a
property of the current local artifact.

Secrets enter through a trusted local or encrypted flow and never through
ordinary model-visible arguments.

## Safety claims are bounded

Application rollback, data recovery, and reversal of an external side effect are
different operations with different guarantees. Onebox classifies risk and
refuses when evidence or a driver contract is insufficient. Exact overrides can
break a stale lock or authorize a named exceptional path; none can turn an
unsupported operation into a safe one.

A customer with root can still bypass Onebox, and a compromised host can lie
about its own evidence. Onebox does not claim universal reversibility, high
availability, or protection from an adversarial infrastructure provider.

## Supporting services

A supporting service — a database, a cache, a queue — is selected and tuned, not
configured. Ownership is explicit: a service is either user-authored or
Onebox-managed, never both.

Breadth and honesty are reconciled by making the guarantee level part of the
contract and visible everywhere the service appears:

- **Managed** — pinned, sized to the host, health-verified with driver-specific
  semantics, backed up, restore-drilled on a schedule, with a tested upgrade
  path.
- **Run** — pinned, persisted, health-gated, capped, observed, never recreated by
  an application rollback, and carrying no backup contract that it must state
  plainly.
- **External** — declared, connected to, and not owned.

A driver graduates from Run to Managed when its restore drill passes in CI. A
declaration is never presented as proof that protection is running.

This tier is direction, not current behaviour. The canonical facts carry the
field and validate it, but nothing assigns a tier yet, and no surface reports
one — see [Shipped vs proposed](../site/src/content/docs/status/capabilities.mdx).

## Delivery discipline

A capability is shipped only when its design is agreed, its tests pass, and the
generated reference and capabilities page are updated from proposed to
implemented.

Documentation says what is true today. Direction lives here, and it is not
presented as a capability.

## Positioning

The durable positioning is:

> Onebox operates one production server through typed, reviewable,
> evidence-backed capabilities — and tells the truth when an action falls outside
> those guarantees.
