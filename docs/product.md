# onebox — MCP-first production operations for one box

> Status: working product specification
>
> Date: 2026-07-12
>
> Product direction: proposed canonical framing

This document consolidates the product decisions, customer contract, user
experience, architecture, safety model, operational scope, business model, and
delivery plan for the next shape of onebox. The existing `ob` deployment engine
is the implementation foundation; it is no longer the intended primary user
interface.

## Executive summary

**Onebox is an MCP-operated production layer for applications intentionally
running on one server, with a dashboard for approval, visibility, and recovery
assurance.**

The customer talks to their coding agent. The agent uses Onebox's MCP tools to
observe production, propose changes, and execute approved plans. A dashboard
shows the current state, captures human consent, preserves operational memory,
and proves that backups and recovery still work. The existing engine supplies
the hard safety mechanisms: plan binding, health-gated releases, locking,
fencing, journaling, resumability, migration gates, verification, and rollback.

The product contract is:

> **Bring your agent, repository, and one server. Get a production system that
> can be safely changed and demonstrably recovered.**

The topology is deliberately one production box. Onebox is not a cluster
manager, PaaS, hosting provider, or high-availability system. It preserves the
economic and cognitive simplicity of one server while addressing the
operational risks that appear once the application and its data matter.

The interaction model is:

> **The MCP is where work happens. The dashboard is where trust happens. The
> engine is where safety is enforced.**

## Implementation status

This document describes both the implemented foundation and the intended paid
product. The first MCP-facing vertical slice now exists, but the approval and
managed-service layers remain product direction rather than shipped claims.

The repository already contains the substantial safety kernel:

- Compose loading, validation, inference, rendering, and payload staging.
- SSH transport with known-host verification.
- State-bound plan artifacts and image pinning.
- Health-gated rolling and recreate deployments.
- Versioned releases, verification, activation, retention, and rollback.
- Locking, epochs, host-side fencing, heartbeats, and append-only journals.
- Resume, abort, migration gates, status, audit, secrets, accessories, managed
  proxy operations, and notifications.
- Unit and opt-in end-to-end coverage for the critical deployment lifecycle.

The repository now also contains the first MCP product slice:

- `ob mcp`, an official-SDK stdio server in the simply named `internal/mcp`
  package.
- The stable, explicit `onebox.run/v1` project contract described in
  [`schema-v1.md`](schema-v1.md). New v1 releases preserve existing files and
  add only optional capabilities.
- A typed `internal/onebox` service shared independently of CLI presentation.
- A canonical, digest-bound executable operation graph with typed steps, risk,
  reversibility, approval class, target/state bindings, and expiry.
- A single service execution boundary used by CLI mutations, preserving the
  existing engine's locks, fencing, journals, drift checks, and rollback gates.
- A read-only production observation tool with structured health, drift,
  incomplete-deployment, accessory, proxy, and certificate state.
- A read-only deployment-proposal tool with immutable image resolution,
  process-scoped environment authority, status blockers, opaque structural
  diffs, explicit risk, and keyed content-bound proposal identities.
- A model-facing secret boundary: arbitrary remote errors, Compose scalar
  values, verification endpoints, and operator hook bodies are not forwarded.
- Deterministic operational-memory reads and immutable, revision-bound memory
  change proposals. These are configuration-derived in this milestone; durable
  encrypted cloud memory remains future work.
- No MCP production mutation tool. Existing CLI workflows remain the execution
  path until approval-bound MCP execution is built.

The following product layers are proposed and not yet implemented:

- MCP execution tools and dashboard-issued approval capabilities.
- Durable encrypted operational memory and evidence synchronization.
- Credential or capability isolation suitable for a shell-capable agent.
- Onebox Cloud accounts, device pairing, policy, approvals, and evidence.
- Dashboard overview, approval, timeline, and recovery surfaces.
- Narrow on-box observer and continuous protection loop.
- PostgreSQL and file-volume backup and clean-target recovery drills.
- Baseline managed monitoring, alerts, and service capability drivers.

The v1 schema deliberately includes `protection` and `observability` desired
state before those managed layers ship. Their presence does not claim that a
backup, restore drill, log pipeline, metrics database, or alert loop is running.
Current observations distinguish declared configuration from managed
capability.

## Product decisions

| Question | Decision |
|---|---|
| Primary interface | Local MCP used by the customer's coding agent |
| Human interface | Web dashboard for state, approvals, evidence, policy, and recovery |
| Production topology | Exactly one active production host per application environment |
| Execution | Local MCP calls the shared Go operations service and connects over SSH |
| Continuous work | A narrowly scoped on-box observer performs backups, heartbeats, and read-only observation |
| Production credentials | Scoped credential owned by the Onebox execution boundary, not exposed in model context |
| Application contract | A working Compose contract plus a stable `onebox.run/v1` project file; `ob init` scaffolds it today and agent-assisted generation is the intended onboarding flow |
| Data custody | Customer-owned storage by default; managed storage may come later |
| Paid value | Continuous protection, approvals, durable memory, evidence, alerts, and team governance |
| CLI | Retained as a thin technical adapter for tests, CI, support, and break-glass use |
| Availability promise | Safe changes and measured recovery, not instant host failover |
| Autonomy in v1 | Observation and pre-approved protection may run automatically; production mutations require human approval |

## Stable project contract

Projects author one explicit, versioned operational model in `ob.yml` (or the
same shape in `ob.cue`):

```yaml
api_version: onebox.run/v1

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
    readiness:
      http: /healthz
      port: 8080

deployment:
  order: [web]
  migration_policy: manual
```

Compose describes containers, networks, mounts, and commands. The Onebox file
adds the operational facts that cannot be derived safely: component identity,
rollout strategy, persistence, job data effects, verification, and environment
policy.

The contract avoids ambiguous unions: readiness is exactly HTTP plus port or
exec (or it is omitted to adopt the Compose health check); verification is
exactly an external URL or a component HTTP/exec check; top-level hooks are
closed to the four lifecycle seams; and job commands live on their job
components. Backup and restore-drill schedules use a five-field numeric cron
expression plus an explicit IANA timezone. These constraints make policy display and
future managed execution deterministic without another shape migration.

The compatibility promise is additive from this point forward. New Onebox
releases keep accepting valid `onebox.run/v1` files; existing field meanings do
not change; and new functionality arrives through optional fields or component
types. A breaking requirement or semantic change needs a new API version and an
explicit migration path. This does not make new fields readable by old
binaries—it prevents a Onebox upgrade from forcing an existing v1 project to be
rewritten.

The earlier unversioned shape is intentionally rejected. Users make one
migration now from `hosts`, `roles`, `accessories`, `jobs`, `verify`, and
`notify` to `target`, typed `components`, `verification`, and `notifications`.
The complete contract, deployable example, and mapping are in
[`schema-v1.md`](schema-v1.md).

## Why this product exists

Developers choose one server because it is inexpensive, understandable, and
usually sufficient. They do not want to become infrastructure specialists or
adopt Kubernetes merely to operate a modest SaaS application responsibly.

The simplicity of one box nevertheless concentrates risk:

- `docker compose up` does not explain the full production consequence before
  acting.
- A routine deployment can recreate containers and interrupt requests.
- Mutable images and configuration make rollback unreliable.
- An SSH disconnect can leave production halfway through a change.
- A migration can make application rollback unsafe.
- The recorded desired state can diverge from what is actually running.
- Two operators or agents can race each other.
- The only person who understands the box becomes an operational dependency.
- Backups can report success for months without ever being restorable.
- Losing the host can mean reconstructing production from memory under pressure.
- Giving a coding agent unrestricted SSH is powerful but difficult to trust.

Onebox resolves those problems without changing the topology the customer
intentionally chose.

### Pain map

| Moment | Developer's pain | Onebox's answer |
|---|---|---|
| Before a change | "What exactly will this do?" | Rendered, state-bound plan with risk and verification |
| During deployment | "Will requests fail?" | Health-gated scale, traffic drain, and rolling replacement |
| Failed deployment | "Can I get the old version back?" | Versioned release with its own configuration and pinned images |
| Lost connection | "Which steps actually completed?" | Durable journal, fencing, resume, and abort |
| Database migration | "Is rollback still safe?" | Migration gate and honest halt when compatibility is unknown |
| Normal operation | "Is reality different from desired state?" | Structured recorded-versus-actual observation |
| Multiple operators | "Can two changes collide?" | Lock epochs and host-side stale-runner fencing |
| Day-two operations | "How do I safely change secrets, proxy, or data services?" | One plan, approval, mutation, and evidence regime |
| Staff continuity | "Only one person knows how this works." | Durable operational memory and tested procedures |
| Host loss | "Can this be rebuilt with its data?" | Off-box protection and clean-host recovery drills |
| Agent access | "I want help without handing over raw production SSH." | Typed, policy-gated production capabilities |

## Target customer

The initial customer is a technical founder or CTO at a small, revenue-producing
software company:

- Two to ten people.
- One to three independent production applications, each intentionally running
  on one VPS or physical server.
- Docker Compose, PostgreSQL, and common supporting services.
- No dedicated SRE or platform engineer.
- Production is valuable enough that data loss or a weekend rebuild is costly.
- Comfortable using coding agents, but unwilling to grant one unrestricted
  production access.

Strong buying triggers include a recent outage, an untested backup, an overdue
database or operating-system upgrade, a customer security questionnaire, or the
departure of the only person who understands production.

Hobbyist self-hosters are a useful community but a weak initial paid segment.
Larger companies have more budget but will require multi-host topology,
enterprise identity, compliance, and support before trusting the product.

## Customer contract

### What users bring

Users provide facts, authority, intent, and approval—not operational scripts.

#### Their coding agent

Claude, Codex, or another MCP-capable agent. Onebox does not sell inference or
require the customer to adopt a new chatbot.

#### Their application repository

The first supported product requires working Docker and Compose artifacts.
Onebox uses them as the execution contract. An agent may help create missing
artifacts before enrollment, but arbitrary repository-to-container generation is
a separate onboarding problem and is not part of the initial production-safety
or recovery guarantee. Today `ob init` scaffolds the small, stable
`onebox.run/v1` project file and the customer reviews its operational judgments.
The intended MCP onboarding flow generates and explains that file so the
customer does not need to learn a second platform language before getting
value.

#### One production server

A Linux VPS or physical server under the customer's control, with scoped SSH
authorization. There is no permanently running second production server.

#### External identities required by the application

Examples include a domain and DNS account, private registry credentials, a
cloud-provider account, an alert destination, and optional customer-owned
object storage.

#### Secret values

Application passwords, API keys, database credentials, and encryption keys are
entered through a secure local or encrypted flow. They are never placed in agent
conversation, command arguments, dashboard logs, or plan descriptions.

#### Decisions only the owner can make

Onebox discovers facts and asks only about intent:

- Which data is valuable or reconstructible?
- What proves the application is healthy?
- What amount of data loss is acceptable?
- How quickly should recovery complete?
- What are the worker drain and job completion semantics?
- Can a migration preserve compatibility with the previous release?
- During which windows may disruptive maintenance happen?

#### Approval

The owner approves consequential production plans. Routine reads, monitoring,
backups, and isolated drills may run under a standing policy.

### What users get

#### A production setup created and maintained for them

Onebox and the customer's agent produce and maintain the operational artifacts:

- Container and Compose definitions.
- Proxy and HTTPS configuration.
- Service roles, dependency order, and rollout behavior.
- Health and verification checks.
- Secret wiring.
- Backup and retention policies.
- Deployment, maintenance, and recovery procedures.

#### Conversational operations

The normal interface is intent expressed to the agent:

```text
Deploy the latest version.
Why is the worker unhealthy?
Protect the uploads volume.
Rotate the Stripe key.
Upgrade PostgreSQL safely.
What changed in production yesterday?
Can this application recover from losing the server?
Run a recovery drill.
```

#### Safe production changes

Every consequential proposal explains:

- The exact operations and artifacts.
- The host and repository state against which it was calculated.
- Risk and reversibility classification.
- Expected interruption, if any.
- Application and data consequences.
- Verification and rollback procedure.

#### Continuous protection

The paid product performs useful work when the customer is not talking to an
agent:

- Encrypted off-box backups.
- Backup-freshness and protection-gap monitoring.
- Scheduled restore and recovery drills.
- Application-level verification after restore.
- Health, drift, certificate, and job monitoring.
- Alerts when protection or production degrades.
- Measured recovery point and recovery time.

#### A durable trust plane

The dashboard supplies the current operational picture, pending approvals,
human-and-agent history, policies, recovery evidence, and durable application
memory.

#### Continued ownership

The customer keeps their server, repository, domain, secrets, backups, and
configuration. The application keeps running if Onebox Cloud is unavailable.
Cancellation must leave exportable recovery artifacts and no runtime dependency
on the subscription.

### What users should not have to bring

Users should not need to write or maintain:

- Deployment, backup, or restore shell scripts.
- A disaster-recovery runbook.
- Kubernetes, Terraform, or Ansible solely for Onebox.
- Health-drill automation.
- Audit-report generation.
- A custom prompt that reteaches each new agent how production works.
- A second permanently running production host.

## Scope and honest boundaries

### Single-box means one active production host

Onebox can prevent application-level interruption during a deployment while the
host remains healthy. It cannot make one physical host highly available.

Onebox can reduce recovery time by rebuilding onto a replacement host. It does
not promise instant failover without a second active system. A temporary drill
machine or post-failure replacement does not change the production topology.

### Recovery is not universal undo

Application rollback, data recovery, and reversal of an external side effect
are different operations.

- A versioned application release can often roll back safely.
- A database may require point-in-time recovery and may lose writes after the
  chosen recovery point.
- DNS changes, emails, payment actions, leaked secrets, and arbitrary external
  API calls cannot be made universally reversible by snapshotting the box.

Onebox must state those differences and refuse to describe an operation as
protected when it cannot prove the protection.

### Supported complexity

Application complexity may grow within one host: several services, workers,
jobs, databases, caches, volumes, and a proxy are valid. Topology is the hard
boundary: clusters, multi-region systems, Kubernetes, serverless infrastructure,
and coordinated multi-host applications are out of scope.

### Data-size boundary

Backup and recovery techniques that are honest for a modest database may be
inappropriate for a multi-terabyte dataset. Drivers declare supported size and
consistency boundaries. Onebox refuses or escalates outside them rather than
offering a misleading green status.

## Product experience

### Onboarding

The user installs or enables Onebox's local MCP integration, opens their
repository in an MCP-capable coding agent, and asks:

```text
Protect and operate this application with Onebox.
```

The agent calls Onebox to inspect the repository and host. Onebox returns
structured findings rather than requiring the model to parse arbitrary SSH
output:

```text
Onebox discovered:

  web          application         healthy
  worker       background worker   healthy
  postgres     database            persistent
  redis        datastore           intent unknown
  uploads      file volume         persistent
  traefik      proxy               healthy
  nightly      scheduled job       last success 19h ago
```

The agent asks only unresolved questions:

```text
1. Is Redis a disposable cache or durable application state?
2. Can the uploads volume be reconstructed from another source?
3. What maximum data loss is acceptable for PostgreSQL?
4. Which request proves the restored application works?
```

Onebox then creates a state-bound setup and protection proposal. The dashboard
shows the exact plan, and the user approves it. There are no Onebox commands in
the normal workflow.

### Routine deployment

```text
User: Deploy the latest version.

Agent: Onebox found changes to the web and worker images. The database
       migration declares expand-only compatibility. No configuration or
       persistent-volume changes were found. Review the production plan:
       https://app.onebox.run/approvals/plan_82af
```

After approval, the MCP executes the same immutable plan, streams structured
progress, verifies production, and records the result.

### Diagnosis

```text
User: Why is production slow?
```

The agent uses structured observations covering host resources, container
health, recent deployment events, PostgreSQL signals, Redis pressure, queue
depth where supported, and recent logs. It proposes a change only after it has
reported the evidence.

### Recovery drill

```text
User: Prove that we can recover this application.
```

Onebox restores the latest protected state onto a disposable blank target,
starts the application in isolation, runs service and application verification,
measures recovery time, records the recovery point, and tears the target down
only under the approved drill policy.

The result is explicit:

```text
RECOVERY PROVEN

potential data loss     3m 14s
time to healthy         7m 42s
database rows           1,284,902
application checks      12/12 passed
release                 20260712-a82f41
unprotected state       none
```

Failure is equally explicit:

```text
RECOVERY NOT PROVEN

uploads volume has no backup policy
application image is no longer available from the registry

Production was not modified.
```

## MCP interface

The MCP is the primary product interface. It should expose a small set of
high-level capabilities rather than mirroring every CLI flag or providing a
generic production shell.

### `observe`

Returns typed, read-only state for the repository, host, services, releases,
health, drift, protection, logs, metrics, jobs, and operational history.

An observation is a permission-scoped snapshot, not timeless ground truth. Each
result carries target identity, capture time, provenance, completeness and
partial-error information, plus a state digest suitable for plan preconditions.

Observation data is evidence, not instruction. Repository text, application
logs, database content, and remote output are marked as untrusted so prompt-like
content cannot silently become an operation.

### `propose`

Accepts an intended outcome and returns an immutable plan:

- Exact operation graph.
- Rendered artifacts and content digests.
- Host-state and repository-state preconditions.
- Risk and reversibility class.
- Required approval policy.
- Verification and rollback or recovery behavior.
- Expiration and plan digest.

The plan is the canonical executable representation. Human-readable commands
are a rendering of that representation, not a separate implementation of the
change.

### `execute`

Accepts a plan identifier and approval token. It refuses when:

- The plan has changed.
- The repository or relevant host state has drifted.
- The approval does not sign the exact plan digest and environment.
- The approval expired or came from an unauthorized identity.
- A policy changed after planning.
- A newer operation fenced this runner.

Execution streams typed phase and service events and ends with verification
evidence.

### `memory`

Reads and proposes changes to durable application knowledge:

- Service roles and dependencies.
- Persistent versus disposable state.
- Health and readiness semantics.
- Worker drain and job completion behavior.
- Migration compatibility policy.
- Recovery objectives and retention.
- Previous operator decisions and application-specific procedures.

Memory changes are versioned. A model cannot silently rewrite safety policy or
historical evidence. Facts inferred from repositories, logs, remote output, or
application data cannot silently become durable policy; they remain attributed
proposals until reviewed.

### No generic safe-shell claim

A low-level escape hatch may exist for break-glass support, but it is never
described as protected or automatically reversible. It receives a visibly
weaker risk classification and separate approval policy. The standard MCP
surface does not expose arbitrary production shell.

## CLI relationship

The CLI is not the intended user experience. It remains valuable as a thin
adapter because it provides:

- A fast development and test surface for the engine.
- CI and end-to-end automation.
- Debugging without a particular MCP client.
- Break-glass recovery when an agent client or dashboard is unavailable.
- A portable open-source interface independent of Onebox Cloud.

The CLI, MCP, and any future API must call the same operations service. Business
and safety logic must not live in Cobra handlers or MCP handlers.

For a project enrolled in the managed control plane, the CLI must enforce the
same signed-plan policy or enter an explicitly labeled, separately audited
break-glass mode. It must not silently bypass the approval regime.

Conceptually:

```text
                         Onebox Dashboard
                    approval, policy, evidence
                                │
                                │ signed plan
                                ▼
Agent ─────► Onebox MCP ─────► Operations service ─────► Engine ─────► SSH
                                      ▲
                                      │
                         thin CLI adapter
                      tests, CI, and break-glass
```

If MCP merely maps a tool to `ob deploy`, it has not earned its complexity. MCP
is valuable when it supplies typed perception, durable memory, credential
isolation, and policy-enforced execution.

## Dashboard control plane

The dashboard is not a Docker administration UI, terminal, file manager, or
generic observability product. It is the place for consent, policy, evidence,
and recovery confidence.

### Production overview

```text
Production                        HEALTHY

release                  20260712-a82f41
host                     connected 18s ago
configuration drift      none
latest backup            4m 12s ago
last recovery drill      passed — 7m 42s
potential data loss      <= 5 minutes
unprotected state        none
```

### Approval

An approval card shows:

- The intended outcome and initiating agent.
- Exact service, image, configuration, data, and host changes.
- Current-state preconditions.
- Expected interruption.
- Application rollback availability.
- Data recovery consequences.
- Verification and refusal conditions.

Approval signs the immutable plan once. The agent cannot alter an approved plan.

### Timeline and evidence

One ordered journal covers human and agent operations:

```text
14:32  deploy proposed by Codex
14:34  approved by v@example.com
14:34  lock acquired, epoch 42
14:35  migration completed: changed=false
14:36  web healthy
14:37  verification passed
14:37  deployment complete
```

### Recovery

The recovery surface shows:

- Latest recoverable point.
- Measured and targeted RPO/RTO.
- Backup and drill history.
- Protected and unprotected state.
- Missing images, secrets, data, or bootstrap dependencies.
- Recovery reports and evidence bundles.
- A path to propose a manual drill or emergency recovery.

### Policy

Initial policy is deliberately small:

- Who may approve production mutations.
- Which read-only and protection actions may run automatically.
- Maintenance windows.
- Recovery and retention objectives.
- Which operation classes require stronger approval.

## System architecture

```text
Developer workstation
  coding agent
      │
      ▼
  local Onebox MCP ───────── plans, memory, evidence ─────► Onebox Cloud
      │                                                        │
      │ scoped SSH                                             │ approvals
      ▼                                                        │ policy
Production box                                                 │ alerts
  application services                                         │ reports
  Onebox observer ───── heartbeat and proof metadata ──────────┘
      │
      └──────── encrypted backups ─────────► Customer object storage

Disposable customer-owned drill target
      ▲
      └──────── recovery bundle and backups
```

### Local MCP

The local MCP is launched automatically by the agent client. It has access to
the repository and a Onebox-scoped production capability. Sensitive credentials
are never returned to the model or embedded in tool results.

The product goal is to keep raw production credentials outside model context
and normal shell use. This reduces accidental and prompt-injection-driven
bypass; it is not a claim that Onebox can defend against a malicious local
administrator or a fully compromised workstation. A same-OS-user stdio MCP
cannot by itself prove credential isolation from a shell-capable agent; v1 must
either introduce a separate privileged identity or capability broker, or narrow
its guarantee to procedural enforcement and state that assumption visibly.

### Shared operations service and engine

The Go operations service owns:

- Discovery and desired-state resolution.
- Typed planning and risk classification.
- Plan binding and approval validation.
- Deployment, maintenance, protection, and recovery orchestration.
- Locking, fencing, journaling, verification, rollback, and resume.

The current engine is progressively refactored behind this service. There is one
implementation of each operation regardless of which adapter invoked it.

### Onebox Cloud

The first control plane should be one small application, not a collection of
microservices. Its capabilities are:

- Accounts, organizations, projects, and device pairing.
- Approval issuance and policy evaluation.
- Encrypted durable memory and recovery-manifest metadata.
- Evidence, drill, backup, and operation timelines.
- Heartbeat and protection-gap detection.
- Email and webhook delivery.
- Subscription and protected-stack entitlement.

The control plane is not in the application's request path and cannot make an
unapproved production mutation.

### On-box observer

A narrowly scoped component may run continuously to perform:

- Backup schedules.
- Read-only host and container observation.
- Health and job heartbeats.
- Journal and evidence forwarding.
- Protection-gap detection.

It communicates outbound only. It does not expose a cloud-controlled arbitrary
shell and does not autonomously remediate production in v1.

### Customer object storage

Encrypted application data is stored in the customer's S3-compatible storage
by default. Onebox Cloud keeps status, hashes, policy, and evidence—not plaintext
production backups. Managed encrypted storage is a later option after the
security and compliance implications are justified by demand.

The protection policy includes encryption-key separation, retention,
immutability or deletion protection where the provider supports it, and tested
restore access. An encrypted object without a recoverable key is not protected.

### Drill and replacement targets

V1 uses a user-provided disposable blank host. Later, Onebox may create an
ephemeral target in the customer's cloud account with a scoped provider token,
or offer managed drill compute. A drill target is never treated as a second
production node.

A scheduled clean-target drill requires a positively identified runner and a
previously approved provisioning capability. Until that exists, clean-host
drills are manual or concierge operations; the system must not imply that they
happen automatically while the workstation is closed.

## Operational model for common services

If Onebox claims to operate the box, it must understand the common services on
it. It should manage their operational lifecycle and unified experience without
reinventing PostgreSQL, Redis, Prometheus, or a log analytics engine.

This section is the target managed-service contract, not the current local
implementation. Today the engine deploys generic Compose services, fetches
current status and logs, and sends operation notifications. The schema records
persistence, protection, and observability intent, but Onebox does not yet run
continuous backups, restore drills, log collection, metrics storage, or alert
scheduling. Those capabilities become managed only when the implementation can
produce evidence for them.

### Capability drivers

Each supported service driver can implement the following capabilities:

| Capability | Meaning |
|---|---|
| Discover | Identify the service and infer factual configuration |
| Observe | Return typed health, state, capacity, and drift |
| Plan | Produce typed, risk-classified changes |
| Protect | Capture the service's recoverable state |
| Restore | Reconstruct that state in isolation or recovery |
| Verify | Prove service and application behavior after a change or restore |
| Upgrade | Plan and perform a supported lifecycle change |

Every Compose service receives a generic baseline: deployment, container health,
resource observation, recent logs, restart history, and image drift. First-class
drivers add data-aware protection and lifecycle behavior.

### Initial operational set

| Area | Onebox provides |
|---|---|
| Linux host | CPU, memory, disk, load, Docker health, updates, and certificate signals |
| Containers | Status, health, restarts, resource use, image drift, and recent logs |
| HTTP application | External uptime, internal readiness, and deployment verification |
| PostgreSQL | Health, encrypted backups, restore drills, migration gates, and safe supported upgrades |
| Redis | Health, memory and eviction observation, plus persistence based on declared semantics |
| File volumes | Inventory, durable/disposable classification, backup, restore, and verification |
| Proxy and TLS | Routing status, certificate health, and safe configuration convergence |
| Workers and queues | Worker health, supported queue depth, and stalled-worker signals |
| Scheduled jobs | Last run, duration, outcome, and missed-job alerts |
| Logs | Recent MCP access, filtering, redaction, and optional external shipping |
| Monitoring | Onebox baseline signals and integrations for deeper observability |
| Alerts | Actionable email and webhooks for production or protection failures |

### PostgreSQL

PostgreSQL is the first deeply supported data service:

- Health and connectivity checks.
- Connection and storage observations.
- Encrypted off-box backups.
- Backup-freshness alerts.
- Restore into an isolated PostgreSQL instance.
- Schema, table, row, and application-level sanity checks.
- Measured recovery point and restore time.
- Migration-aware releases.
- Supported version-upgrade plans.
- WAL archiving and point-in-time recovery after the simpler path is proven.

Backup success alone is not green. Protection becomes green only after a recent
backup has been restored and verified under policy.

### Redis

Redis requires an explicit semantic classification.

For a disposable cache, Onebox does not create misleading backups. It proves
that Redis may restart empty and monitors memory pressure, evictions, connection
failures, and safe recreation.

For durable Redis state, Onebox requires an appropriate persistence policy,
protects the persistence files, restores them during drills, verifies expected
behavior, and treats destructive changes as data operations.

The dashboard states the result plainly:

```text
redis     disposable cache     recovery not required
```

or:

```text
redis     durable state        protected · last drill passed
```

### File volumes

Every named volume and bind mount is inventoried and classified as disposable,
reconstructible, externally protected, or Onebox-protected. An unclassified
persistent volume prevents a whole-application "recovery proven" result.

### Logs

Onebox provides useful operational access without building another Datadog:

- Fetch and follow container logs through MCP.
- Filter by service, time, deployment, and severity where structure exists.
- Redact known sensitive values.
- Correlate log windows with deployments and health changes.
- Retain a small recent window on the box.
- Configure optional shipping to an external log provider or object storage.

Log content is untrusted data. It is explicitly separated from tool
instructions and capped before entering model context.

Long-term storage, advanced queries, distributed tracing, and application
performance monitoring are integrations rather than initial Onebox services.

### Monitoring and alerts

Onebox Cloud manages a useful baseline:

- External HTTP availability.
- Host heartbeat.
- CPU, memory, disk, and container trends.
- Restart and crash-loop signals.
- Backup freshness and drill outcome.
- Certificate expiry.
- Scheduled-job heartbeat.
- Actionable email and webhook alerts.

Onebox does not initially build custom metrics query languages, tracing,
full-fidelity log storage, or generic incident management.

### Jobs, workers, and queues

Onebox observes what can be inferred and asks for application intent where it
cannot:

- What makes a worker safe to drain?
- What marks a job successful?
- How long may a job run?
- Is a missed schedule actionable?
- Is queue depth available from a supported service driver?

Those answers become durable memory and verification policy.

### Future drivers

After the initial set is proven, likely additions include MySQL/MariaDB, SQLite,
MongoDB, RabbitMQ, NATS, MinIO, and common application-specific adapters. An
unsupported service remains operable as a generic container but does not receive
unsupported data-protection claims.

## Safety and trust model

Onebox's bounded threat-model statement is:

> **Onebox is designed to contain agent mistakes, stale assumptions,
> concurrent operations, and predictable process or network failures within
> supported Onebox-mediated operations. It does not protect against bypass
> through separately held credentials, a malicious root host, compromised
> developer or control-plane identity, undisclosed state, unsupported external
> side effects, or hardware loss beyond the latest verified off-box recovery
> point.**

### Threats Onebox is designed to reduce

- Agent mistakes and hallucinated operations.
- Stale or incomplete observation.
- Plan/apply drift.
- Concurrent or zombie runners.
- Interrupted changes.
- Unsafe application rollback after data change.
- Prompt injection from repositories, logs, remote output, or application data.
- Accidental secret disclosure through plans, logs, arguments, or evidence.
- False confidence in untested backups.
- Loss of operational knowledge between agent sessions.

### Threats outside the initial guarantee

- A malicious workstation administrator.
- A fully compromised agent client or host operating system.
- A user deliberately bypassing Onebox with root access.
- A malicious cloud or storage provider.
- Universal reversal of arbitrary external side effects.
- Availability after physical-host failure without a replacement target.

### Named trust boundaries

| Boundary | Required property |
|---|---|
| User identity to dashboard | Strong authentication, project-scoped authority, revocation, and audit |
| Agent, repository, logs, and application data to MCP | Treat all content as untrusted input; it cannot create policy, approval, memory, or execution by instruction alone |
| MCP to privileged executor | Least privilege and typed capabilities; no raw shell under the strongest safety claim |
| Dashboard to executor | Approval binds the canonical operation graph, artifacts, target, state digest, identity, nonce, and expiry |
| Production host to cloud evidence | Outbound-only reporting with provenance; a compromised root host can still lie |
| Production to backup storage | Encryption-key separation, retention, immutability where available, and tested restore access |
| Registry, DNS, and cloud providers | External effects are explicit typed operations, never hidden shell side effects |
| Production to recovery target | Positive target identity, disposable marking, and isolation from production endpoints and side effects |
| Onebox Cloud outage or cancellation | Runtime and local backup schedules continue; desired state, journals, and recovery artifacts remain exportable |
| Onebox supply chain | Pinned or signed components, secure updates, and component versions recorded in evidence |

### Operation classes

| Class | Example | Default policy |
|---|---|---|
| Observe | Read health, state, logs, or history | No approval |
| Standing protected action | Backup, heartbeat, isolated scheduled drill | Pre-approved policy |
| Reversible mutation | State-bound application deploy or safe restart | One-time approval |
| Conditionally reversible mutation | Secret rotation, database migration, version upgrade | Stronger approval with explicit consequences |
| Unsupported or irreversible action | Unknown destructive command or external side effect | Refuse or explicit break-glass outside the safety claim |

### Exact approval binding

An approval token binds:

- Plan digest and immutable operation graph.
- Project, environment, and target host.
- Relevant repository and host state.
- Operator identity.
- Approval policy and expiration.
- A single-use nonce or equivalent replay defense.

Any material change requires a new proposal and approval.

### Host-side fencing

The existing lock, epoch, and fence regime remains essential. Approval answers
"may this plan run?" Fencing answers "is this still the winning runner?" Both are
required. A previously approved but stale executor must be rejected at the host.

### Secret handling

- Secret values never appear in model-visible tool results.
- Plans and evidence contain identifiers and hashes, not values.
- Local secure entry and encrypted storage are preferred.
- Backup and drill credentials are scoped separately where possible.
- Recovery-key ownership and escrow are explicit, tested parts of recovery.

### Evidence integrity

Journals and reports are append-oriented and content-addressed. The dashboard
distinguishes a reported backup, a successfully restored backup, and a fully
verified application recovery. History cannot be rewritten by updating memory.

This is attributable evidence, not remote attestation against a compromised
root host. "Recovery proven" means the declared supported state was restored and
verified under the recorded procedure; it does not prove that a malicious host
reported truthful source facts.

## Managed service boundary

Onebox manages assurance, not the customer's runtime infrastructure.

### Managed by Onebox Cloud

- Project enrollment and device identity.
- Plan approvals and policy.
- Encrypted operational memory metadata.
- Backup, drill, and recovery status.
- Signed evidence and reports.
- Protection-gap and heartbeat monitoring.
- Email and webhook alerts.
- Billing and team entitlements.

### Controlled by the customer

- Production VPS and Docker runtime.
- PostgreSQL, Redis, proxy, and application containers.
- Domain and DNS provider.
- Container registry.
- Object storage by default.
- Source repository.
- Secret values and recovery keys.
- Disposable recovery target in v1.

### Not managed initially

- VPS hosting.
- Managed database or Redis hosting.
- Container builds and registry hosting.
- DNS or certificate authority infrastructure.
- Full log and metrics platforms.
- General-purpose secret management.
- LLM inference.
- Autonomous production remediation.
- Multi-host orchestration.

## Business model

### What remains free

The open-source local core should include the engine and MCP capabilities needed
to inspect, plan, deploy, verify, roll back, resume, and audit a single project
without a hosted dependency. This creates adoption, trust, and an escape hatch.

### What customers pay for

Customers pay for continuous outcomes and shared trust:

- Durable encrypted operational memory.
- Dashboard approval and policy.
- Scheduled protection and recovery drills.
- Recovery evidence and RPO/RTO history.
- Monitoring and actionable alerts.
- Team identity, audit retention, and governance.
- Guided support during recovery or risky maintenance.

### Pricing hypotheses

Pricing is per protected production stack rather than per seat, deployment,
agent, or token.

| Offer | Scope | Hypothesis |
|---|---|---|
| Onebox local | Local MCP and safe engine | Free/open source |
| Protect | One stack, one primary database, continuous protection and dashboard | $99/month plus storage |
| Team | Up to five stacks, shared policy, approvals, retention, and support | $399/month |
| Enterprise | SSO, compliance evidence, contracted support and recovery objectives | Custom |

These are experiments, not commitments. Early design partners should receive a
service-heavy offer because implementation and trust work are initially manual.

### Market rationale

As of July 2026, deployment and server-management products are inexpensive or
free: [Coolify](https://coolify.io/pricing),
[Laravel Forge](https://marketing.forge.laravel.com/pricing),
[Ploi](https://ploi.io/pricing), [Kamal](https://kamal-deploy.org/), and
[Cloud 66](https://www.cloud66.com/pricing/) all make generic deployment a weak
standalone paid wedge.

Recovery and assurance support higher prices: products such as
[SimpleBackups](https://simplebackups.com/pricing),
[pgverify](https://pgverify.com/), and [FailZero](https://failzero.net/) charge
for backup confidence, restore verification, and full-stack drills. Onebox must
therefore sell demonstrated risk reduction rather than deployment convenience.

## First sellable product

The first sellable outcome is:

> **A Compose and PostgreSQL application can be safely operated through an
> agent, and Onebox can prove on a blank target that the application and its
> declared data recover.**

### Supported v1 envelope

- One Linux production host.
- Local stdio MCP.
- Onebox dashboard account and device pairing.
- Existing, working Docker Compose application.
- PostgreSQL as the first protected database.
- Redis discovery, classification, and basic observation.
- S3-compatible customer storage.
- Declared persistent file volumes.
- HTTP and container health verification.
- User-provided disposable blank recovery host.
- Email and webhook alerts.

### Explicitly unsupported in v1

- Multi-host production.
- Automatic production failover.
- Arbitrary protected shell operations.
- Databases other than PostgreSQL.
- Unclassified persistent data.
- Onebox-hosted log analytics or APM.
- Autonomous patches, upgrades, or remediation.
- Claims of recovery for external side effects.

## Delivery plan

### Milestone 0 — canonical operations service (implemented)

Refactor any remaining command-owned behavior behind a shared Go service:

- `Observe`
- `Propose`
- `Execute`
- `ReadMemory`
- `ProposeMemoryChange`

Planning produces the canonical executable operation graph. The CLI becomes a
thin adapter, and all existing safety mechanisms remain covered by tests.

### Milestone 1 — conversational deployment

Build the primary agent experience:

- Local MCP with typed tools and errors.
- Repository and production observation.
- Immutable deploy proposals.
- Onebox Cloud pairing.
- Dashboard overview and approval card.
- Approval-bound execution and structured progress.
- Evidence timeline.

Exit criteria:

- A user deploys through agent conversation without invoking a Onebox command.
- The dashboard shows and signs the exact executable plan.
- Tests prove that missing approval, plan or artifact tampering, stale host
  state, replay, expiration, wrong project or host, and a concurrent runner all
  cause refusal or re-plan.
- An interrupted deploy can resume through the MCP.
- A stale approved runner is still fenced host-side.
- Fault injection kills the MCP, network, and executor across lifecycle phases;
  resume remains idempotent and the journal reports incomplete state honestly.
- A failed application health check may roll back application release and
  configuration, but never restores an older database over legitimate live
  writes unless a separately validated data-recovery plan was explicitly
  approved.
- No secret value appears in model-visible output or evidence.
- Observation results include target, timestamp, provenance, completeness, and
  a state digest.

### Milestone 2 — protection and recovery vertical slice

Build one complete proof path:

- PostgreSQL backup to S3-compatible storage.
- Persistent file-volume protection.
- Recovery manifest containing releases, image digests, config, secret
  references, data, bootstrap assumptions, and verification.
- Restore onto a user-provided blank target.
- Application-level verification.
- Dashboard recovery status and evidence report.

Exit criteria:

- Seeded application and database data restore correctly on a blank target.
- The restored application passes container and HTTP verification.
- A seeded persistent file restores and is verified alongside a known database
  record; row counts alone are insufficient.
- Measured recovery point and recovery time are recorded.
- A corrupted backup fails clearly.
- A missing key, image, secret, unavailable registry, or unprotected volume
  prevents a green result.
- The production host is not mutated by the drill.
- The target is positively identified as disposable, and outbound email,
  webhook, payment, DNS, and other production side effects are isolated.
- The recovery path used in a drill is the same path used during an emergency.

### Milestone 3 — continuous assurance

Add the work that supports a subscription:

- Narrow on-box observer.
- Backup and heartbeat schedules.
- External uptime and host/container baseline monitoring.
- Certificate and job heartbeat checks.
- Redis semantic classification.
- Protection-gap alerts.
- Recurring recovery drills under standing policy.

Exit criteria:

- Onebox detects a stale backup, missed job, crash loop, certificate risk, and
  failed drill without an active agent session.
- The application continues running when Onebox Cloud is unavailable.
- Local backup schedules continue during a Onebox Cloud outage.
- The observer cannot receive arbitrary remote shell commands.
- A bounded log query redacts a seeded secret, and prompt-like content in logs
  cannot change memory, create a plan, or trigger execution.

### Milestone 4 — paid design partners

Recruit qualified customers before broadening the service matrix.

Offer a 60-day design partnership around a concrete result: protect the real
application, perform a clean recovery drill, deliver the evidence, and repeat a
second production or recovery event.

Suggested validation offer: $1,000 onboarding plus $199/month for hands-on
support. The price is a willingness-to-pay test, not the final self-service
price.

Success threshold:

- At least three of twenty qualified prospects pay.
- At least two permit use against real production state.
- At least two complete a second drill or consequential operation.
- At least two renew after receiving repeated evidence.
- Buyers describe the value as recovery, risk reduction, or operational
  continuity—not merely a nicer deploy command.

## First demonstration

The unedited product demonstration should show:

1. A developer opens an existing repository and says, "Operate and protect this
   application with Onebox."
2. Onebox discovers the Compose services, host, PostgreSQL, Redis, volumes,
   proxy, jobs, and health checks.
3. The agent asks only the ambiguous intent questions.
4. The dashboard displays a precise setup and protection plan.
5. The developer approves it.
6. Onebox deploys safely and enables protection and observation.
7. A deployment is interrupted and resumed without losing control.
8. A recovery drill restores the application onto a blank machine.
9. The restored application and data pass verification.
10. The dashboard reports the measured recovery point, recovery time, and
    evidence.
11. A deliberately corrupted backup is rejected honestly.

This proves the product more effectively than a feature dashboard or a generic
agent chat demonstration.

## Success metrics

### North-star metric

**Percentage of protected production stacks with a successful clean-target
recovery drill inside their required verification window.**

### Supporting metrics

- Time from enrollment to first approved production observation.
- Time to first safe agent-operated deploy.
- Time to first verified recovery.
- Percentage of proposed changes refused because state drifted.
- Percentage of applications with no unclassified persistent state.
- Backup freshness and drill success rate.
- Median measured recovery time versus target.
- Second meaningful agent operation within thirty days.
- Design-partner conversion and renewal.

Safety metrics are not ordinary growth metrics: unapproved mutations and secret
leaks have a target of zero.

## Non-goals

- Kubernetes or multi-host orchestration.
- A hosted PaaS or VPS reseller.
- An LLM or proprietary coding agent.
- A full observability, logging, tracing, or incident-management platform.
- A general-purpose secret manager.
- A universal infrastructure-as-code framework.
- Autonomous production remediation in v1.
- Universal reversibility for arbitrary commands.
- Instant availability after losing the only host.
- Supporting every database or Linux distribution at launch.

## Principal risks

### Trust and willingness to pay move in opposite directions

Developers most comfortable with agents touching production may be least likely
to pay. Buyers with meaningful budgets may be most resistant to agent-operated
production. The product must lead with constrained capabilities, proof, and
human control rather than autonomy.

### Scope expansion into a PaaS

Databases, Redis, logs, monitoring, proxying, DNS, backups, and patching can turn
into an unbounded platform. The capability-driver boundary is essential:
Onebox owns safe orchestration and evidence, while established components own
their specialized mechanics.

### False safety claims

"Structurally cannot destroy production," "every action is reversible," and
"backup succeeded" are too broad. Claims must be scoped to supported operation,
data, credential, size, and threat-model boundaries and backed by evidence.

### Prompt injection and tool bypass

An MCP does not create safety by itself. Untrusted observations must remain data,
production credentials must not be available as ordinary model context, and
the execution path must enforce approval even when the agent asks persuasively.

### Recovery complexity

Application images, secrets, database state, file volumes, DNS, bootstrap
assumptions, and external dependencies can each make a backup insufficient.
Onebox must model the complete recovery inventory and show gaps explicitly.

### Operational burden of the managed service

Taking custody of data, credentials, drill compute, or runtime availability
would create security, compliance, and support obligations. V1 deliberately
keeps production and backup data in customer-controlled infrastructure while
Onebox manages metadata, assurance, and evidence.

## Open decisions

The following require validation or implementation design before they become
fixed product commitments:

- Which Linux distribution and version form the first supported host envelope?
- Which MCP clients receive first-class installation and pairing?
- How is the Onebox-scoped SSH capability isolated from a general local shell?
- What is the exact encrypted-memory and recovery-key ownership model?
- Does the first PostgreSQL path begin with logical backups only, or include WAL
  archiving from the start?
- What file-volume technology is supported first?
- How is the disposable recovery target supplied and proven safe to erase?
- Which minimal metrics are stored by Onebox Cloud, at what resolution and
  retention?
- At what demand threshold should Onebox offer managed storage or drill compute?
- Which operation requires the first stronger-than-single-approver policy?

## Final product statement

> **Onebox is the production-safety layer for serious applications that
> intentionally run on one server. Developers bring their coding agent,
> repository, server, secrets, intent, and approval. They get conversational
> production operations, constrained and verifiable changes, continuous
> protection of common services, durable operational memory, a trust dashboard,
> and measured proof that the application can be recovered.**
