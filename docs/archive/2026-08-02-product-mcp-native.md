# Onebox — LLM-first production operations for one box

> Status: product direction
>
> This document is not an implementation contract. See the
> [documentation authority map](README.md), the current
> [`onebox.run/v1` schema](schema-v1.md), the current [MCP guide](mcp.md), and
> active [OpenSpec changes](../openspec/changes/).

## Product statement

Onebox is the production-operations layer an LLM uses to operate an application
intentionally running on one server. The model supplies reasoning and dialogue;
Onebox supplies typed perception, bounded intent, durable state, exact approval,
and a fenced execution engine.

The normal customer experience is conversational:

```text
User → coding agent → Onebox MCP → canonical operations service → safety engine
```

Users are not expected to learn a Onebox command workflow or manually operate a
control panel. The CLI remains a deterministic adapter for development, CI,
support, and break-glass recovery. A trusted web interaction may be elicited for
approval or secret entry, but it is not a second operations product.

The compact product contract is:

> Bring an MCP-capable agent, an application repository, one Linux server,
> secrets, intent, and trusted approval. Get structured production observation,
> constrained change proposals, and evidence-backed execution within a declared
> safety envelope.

## What exists today

The repository currently provides:

- The stable `onebox.run/v1` project schema and Compose-based application model.
- SSH transport with host-key verification and no resident deployment agent.
- State-bound deployment plans, image pinning, exact approval grants, locks,
  fencing, append-only journals, resume/abort, migration gates, verification,
  versioned releases, and rollback.
- Explicit application, worker, job, PostgreSQL, MySQL, Redis, and generic
  service classifications.
- Generic accessory convergence as a separate maintenance action. These
  supporting services remain authored and owned by Compose.
- Specialized host-scoped Traefik management with an independent Compose
  project, conflict detection, atomic configuration publication, and health
  gating.
- A canonical Go operations service shared by adapters.
- Four read-only MCP tools: `onebox_observe`, `onebox_propose_deploy`,
  `onebox_read_memory`, and `onebox_propose_memory_change`.

The current MCP cannot execute production changes, mint approval, write
configuration, or manage PostgreSQL/Redis lifecycles. The current schema's
`protection` and `observability` sections express desired posture; they do not
prove that Onebox runs backups, restore drills, monitoring, or alerts.

## Product principles

### MCP is the product interface

Agent workflows receive compact, typed, secret-safe results. Large diffs,
evidence, and diagnostics are referenced as bounded resources instead of being
placed in model context by default. Tool schemas and annotations describe real
side effects, and read tools never converge state.

There is no generic shell tool in the standard MCP surface. Repository text,
logs, tool output, and model claims are untrusted input; none can create policy,
approval, or execution authority.

### The canonical service owns behavior

MCP and CLI adapters invoke one versioned operations service. Lifecycle logic,
planning, redaction, approval checks, and execution state do not live in an
adapter. A missed notification or disconnected model cannot lose an operation:
durable operation identifiers and polling are the product contract.

### Approval is not model intent

A statement such as “the user approved” is data, not authority. Consequential
execution requires a trusted capability bound to the exact sealed plan, actor,
target, observed state, expiry, and allowed attempt. Secrets enter through a
trusted local or encrypted flow and never through ordinary model-visible tool
arguments.

### Safety claims are bounded

Application rollback, data recovery, and reversal of an external side effect
are different operations. Onebox classifies risk and refuses when evidence or a
driver contract is insufficient. A force flag cannot transform an unsupported
operation into a safe one.

The customer may still bypass Onebox with root access. A compromised root host
may lie about its own evidence. Onebox does not claim universal reversibility,
high availability, or protection from an adversarial infrastructure provider.

## One-box scope

Onebox operates one application per environment, on one active production host.
That application may have as many workloads as it needs — a server, workers,
jobs, databases, caches, volumes, and a proxy — but the unit Onebox owns is the
application, and a host runs one of them.

Onebox is not:

- A cluster manager, Kubernetes replacement, PaaS, or hosting provider.
- A multi-host or multi-region orchestrator.
- A way to run several independent applications side by side on one host.
- A generic Docker dashboard, terminal, or remote shell.
- A universal infrastructure-as-code language.
- A guarantee of availability after losing the only host.
- A guarantee that arbitrary migrations or external effects can be undone.

Rolling deployment can avoid application interruption while the host remains
healthy. Recovery onto another host is a distinct, evidence-backed workflow,
not failover.

## Configuration and agent experience

`ob.yml` is a durable, reviewable operational contract even when an agent
authors it. The user should not need to memorize or manually edit it, but the
artifact must remain portable, diffable, and independent of model memory.
Compose remains the runtime contract for application-owned containers.

An agent should discover factual structure, expose only unresolved intent, and
propose a bounded patch. It must not silently choose persistence semantics,
data-loss tolerance, migration compatibility, or destructive behavior.

Typical interaction:

```text
User: Operate this application with Onebox.

Agent: I found an application, worker, PostgreSQL container, Redis container,
and Traefik. PostgreSQL has a named volume. Redis persistence intent is
ambiguous. Before proposing a change, is Redis disposable cache or durable
state?
```

The resulting configuration change is an inspectable artifact. Production
mutation remains a separate sealed and approved operation.

## Managed supporting services

The active
[`managed-service-operation-contract`](../openspec/changes/managed-service-operation-contract/)
is the only normative source for the proposed generic managed-service design.
It is not implemented.

The intended boundary is:

- Ownership is explicit: a service is either Compose-owned or Onebox-managed,
  never both.
- Managed services run in stable, independent projects so application deploy or
  rollback cannot recreate or remove them.
- A declaration selects a versioned built-in driver, immutable profile, and
  explicit image. Floating images such as `latest` are rejected; planning pins
  the resolved digest.
- Driver-owned settings are typed. Effective values report whether they came
  from a protected invariant, user input, an immutable profile, or the
  upstream image default.
- A bounded native-parameter map provides a deliberate escape hatch without
  permitting protected runtime overrides. Full native control remains
  available by keeping the service Compose-owned.
- Secrets are projected per declared slot. Plaintext secrets do not enter
  plans, MCP results, operation records, logs, or public errors.
- Observation separates desired, last-applied, and actual state and reports
  evidence completeness.
- Every mutation uses a sealed, expiring, state-bound plan, out-of-band
  approval, durable idempotent execution, fencing, health verification, and
  redaction-safe evidence.
- Application operations treat managed services as prerequisites and cannot
  mutate them as a deploy side effect.

Traefik supplies useful precedent for independent identity and convergence, but
data services require stronger plan, persistence, upgrade, secret, and recovery
contracts. The generic framework therefore ships provider-disabled first.
PostgreSQL and Redis production enablement each require their own OpenSpec
change and qualification suite. Adoption, detach/destroy, backup/restore, and
major upgrades are also separate contracts.

## Target MCP shape

The active managed-service OpenSpec proposes these agent-oriented tools:

- `onebox_service_catalog`: discover drivers, profiles, settings, constraints,
  defaults, origins, and apply effects.
- `onebox_observe`: inspect compact desired/applied/actual state.
- `onebox_propose_service_change`: submit closed structured intent and receive
  a sealed proposal.
- `onebox_apply_project_change`: persist the exact revision-bound local project
  change without accepting arbitrary YAML or touching production.
- `onebox_get_operation`: poll durable proposal or execution state.
- `onebox_execute_approved_operation`: consume a trusted approval capability.
- `onebox_cancel_operation`: request bounded cancellation at a safe checkpoint.

These names describe proposed behavior and are not available in the current
binary. They will not be documented as current until implementation and MCP
conformance tests pass.

## Architecture direction

```text
coding agent
    │ MCP: typed reads, proposals, operation polling
    ▼
Onebox MCP adapter
    │
    ▼
canonical operations service ─── durable proposal/operation repository
    │                                      │
    │ sealed plan + trusted approval       │ ordered evidence
    ▼                                      ▼
safety engine ── SSH ── one production host
```

A future hosted trust plane may provide authenticated approval, policy, team
history, and evidence synchronization. The runtime remains customer-owned and
must continue operating when that service is unavailable. The trust plane must
not expose a cloud-controlled arbitrary shell.

Continuous backups, alerts, and drills require narrowly scoped resident
components because a local stdio MCP process is ephemeral. Those components
need their own specifications and standing policy; their existence must never
be inferred from desired configuration alone.

## Delivery discipline

Roadmaps are represented by OpenSpec changes, not duplicated planning prose in
this document. A capability is shipped only when:

1. Its requirements and design are approved.
2. Its task checklist is implemented.
3. Unit, schema, race, redaction, fault-recovery, and relevant Docker-backed
   tests pass.
4. Current documentation is updated from “proposed” to “implemented.”
5. The OpenSpec change strict-validates and is archived.

The managed-service framework remains disabled for production providers until
the generic safety gates pass. PostgreSQL and Redis then graduate independently
against explicit data, persistence, recovery, and upgrade guarantees.

## Product and commercial boundary

The open local engine and MCP integration are the adoption surface. Potential
paid value is the trust and assurance layer: team approval, durable evidence,
continuous protection, tested recovery, policy, and support. Pricing and hosted
service scope are hypotheses, not repository contracts, and require customer
validation before they drive architecture.

The durable positioning is:

> Onebox lets an LLM operate a single production server through typed,
> reviewable, evidence-backed capabilities—and tells the truth when an action
> falls outside those guarantees.
