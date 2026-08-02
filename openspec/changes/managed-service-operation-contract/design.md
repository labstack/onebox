## Context

See `proposal.md` for motivation and the capability specs for normative behavior.

Onebox already has the hard execution primitives this design needs: a typed `internal/onebox` service, digest-sealed deploy plans and approvals, SSH transport, app locks, host-side fencing, heartbeats, append-only journals, cancellation, structured events, redaction, and observed-versus-recorded status. Managed Traefik proves deterministic rendering, content hashing, stable host state, atomic staging, health-gated applied markers, drift reporting, and shared-resource conflict detection.

The current data-service path is materially weaker. `postgres`, `mysql`, and `redis` components normalize to accessories backed by user-authored Compose. `AccessoryApply` diffs and protects removed mounts, then runs Compose under lock/fence/journal, but it is not driven by a sealed service plan, does not require health before success, and cannot identify service versions, storage formats, or upgrade boundaries. Only deploy currently requires an executable plan. SOPS output is injected into application roles as one flat environment, so it is not an acceptable credential boundary for generated services. MCP exposes read-only observation and proposals; production mutation is still a CLI workflow.

The repository is not yet production-deployed, so schema compatibility is not a hard constraint. This design nevertheless keeps Compose-owned components intact because they are the correct flexibility escape hatch and there is no technical reason to break them.

External constraints checked during design:

- Docker containers have no CPU or memory constraint by default, which can let one service destabilize a single host. Resource policy must be explicit rather than assumed: https://docs.docker.com/engine/containers/resource_constraints/
- Named volumes and explicit names/labels provide stable identities outside a Compose project lifecycle: https://docs.docker.com/reference/compose-file/volumes/
- Cross-project communication requires a shared network, and aliases are network-scoped: https://docs.docker.com/reference/compose-file/services/ and https://docs.docker.com/compose/how-tos/networking/
- Compose secrets require explicit per-service grants, matching the desired least-privilege projection: https://docs.docker.com/reference/compose-file/secrets/
- PostgreSQL major releases can change storage format and require dump/restore, `pg_upgrade`, or replication; ordinary container recreation cannot be treated as an upgrade: https://www.postgresql.org/docs/current/upgrading.html
- The official PostgreSQL image changed its `PGDATA` and volume layout for PostgreSQL 18+, so data paths belong to a version-aware driver invariant: https://hub.docker.com/_/postgres
- Redis persistence choices have different loss, rewrite, and backup behavior, so `ephemeral` and `durable` cannot share one implied default: https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/
- MCP tools support strict input/output schemas, structured content, and behavioral annotations: https://modelcontextprotocol.io/specification/2025-11-25/schema
- Sensitive input must use URL-mode elicitation or another out-of-band surface rather than model-visible forms: https://modelcontextprotocol.io/specification/draft/client/elicitation
- Remote HTTP MCP authorization must use audience-bound tokens and must not pass tokens through to downstream systems: https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization

The installed Go MCP SDK supports output schemas, structured content, tool annotations, and URL elicitation. It does not yet expose the current MCP Tasks protocol. Onebox therefore needs its own durable operation abstraction and polling tools while keeping an adapter boundary for native Tasks later.

## Goals / Non-Goals

**Goals:**

- Make MCP and the coding agent the complete primary interface for service discovery, configuration, proposal, approval handoff, execution, progress, and verification.
- Establish one production-grade managed-service contract reusable by built-in PostgreSQL and Redis drivers.
- Make settings flexible without letting arbitrary configuration bypass service identity, persistence, networking, health, secret, or upgrade invariants.
- Make all defaults deterministic, versioned, visible, and bound into plans.
- Keep application releases and managed-service lifecycles independent.
- Fail closed on stale state, incomplete evidence, unpinned images, unsupported transitions, or malformed tool output.
- Preserve recoverability across cancellation, process death, SSH loss, and partial convergence.

**Non-Goals:**

- Implement PostgreSQL, Redis, MySQL, backup, restore, upgrade, or scheduler behavior in this foundation change.
- Build an external driver/package marketplace or execute downloaded driver code.
- Support arbitrary Compose overrides inside managed mode.
- Automatically adopt an existing user-owned container or volume.
- Automatically tune a database from host size or workload guesses.
- Make the CLI the normal workflow or duplicate MCP-only logic in CLI commands.
- Add multi-host orchestration, HA, failover, or an on-box general-purpose agent.
- Claim protection, recovery, or safe upgrade before the corresponding provider change proves it.

## Decisions

### 1. MCP is the product; the canonical service is the authority

Managed-service behavior is implemented first in `internal/onebox` as typed requests and results. `internal/mcp` exposes those operations as the primary product surface. `cmd/ob` may adapt the same methods for tests, support, and break-glass recovery but cannot own validation, planning, approval, or execution behavior.

The initial MCP surface is deliberately small:

| Tool | Mutation | Purpose |
|---|---:|---|
| `onebox_service_catalog` | No | Discover drivers, profiles, settings, defaults, effects, and compatibility |
| `onebox_observe` | No | Observe application and managed-service state |
| `onebox_propose_service_change` | No | Convert typed intent into an immutable state-bound proposal |
| `onebox_apply_project_change` | Local only | Apply an exact revision-bound managed-service patch to the configured project file and validate it |
| `onebox_get_operation` | No | Poll accepted execution and retrieve bounded evidence |
| `onebox_execute_approved_operation` | Yes | Accept an exact approved proposal idempotently |
| `onebox_cancel_operation` | Yes | Request cooperative cancellation where allowed |

The model never needs to author YAML, interpret raw Compose, carry a plan file, or construct a shell command. When structured intent differs from the project, `onebox_propose_service_change` returns a closed semantic project change bound to the current file revision and reports that runtime planning is not yet ready. `onebox_apply_project_change` applies only that exact change to the configured project path, preserves unrelated YAML content, atomically replaces the file, runs CUE and Go validation, and returns the new revision without connecting to the target. A stale base revision or unexpected edit is refused. The agent then proposes again from durable desired state; an executable runtime plan is never based only on transient conversation.

The local project-change tool requires the MCP process's existing workspace write authority, accepts no arbitrary path or document, and is accurately annotated as a local mutation. Its idempotency key and change digest make retries return the same result. Git remains the review and collaboration record; Onebox does not commit or push changes.

All tools use strict input and output schemas and return structured content. Tool annotations match actual behavior. Results are capped and deterministic; large redaction-safe artifacts are exposed through opaque resource references only when requested.

Alternative considered: keep the CLI workflow and let an LLM invoke shell commands. Rejected because it exposes presentation text as an API, makes approval artifacts awkward, increases context, and lets arbitrary stdout/stderr cross the model boundary.

### 2. Durable operation identity does not depend on an MCP connection

Proposal and operation state are stored behind an `OperationRepository` interface. The local/development implementation uses mode-`0700` directories, mode-`0600` canonical JSON records, advisory locking, write-to-temp plus atomic rename, and bounded retention under the platform state directory. It stores only redaction-safe proposals, approval metadata, task state, and correlation data; plaintext secrets are never persisted there.

The remote append-only journal remains the authority for effects on the target. If the MCP process dies, a replacement process reconstructs accepted execution from the local operation record plus the target journal. If local task state and target evidence disagree, the result is `incomplete` and requires observation; it is never guessed successful.

`onebox_execute_approved_operation` returns an operation id as soon as the operation is durably accepted. `onebox_get_operation` is authoritative even if notifications were missed. An adapter may expose the same records through MCP Tasks when the Go SDK supports the protocol; task adoption must not change operation ids or terminal semantics.

Alternative considered: keep operation state only in memory. Rejected because an MCP restart would lose idempotency and progress identity while a production mutation could still be running.

### 3. Managed authoring uses a common envelope plus driver-owned typed settings

A data or generic service remains Compose-owned unless `managed` is present. The common envelope is:

```yaml
components:
  database:
    type: postgres
    managed:
      driver: onebox/postgres/v1
      profile: standard/v1
      image: postgres:18.4
      resources:
        memory: 2GiB
        memory_reservation: 1GiB
        cpus: "2.0"
      secrets:
        password: POSTGRES_PASSWORD
      settings: {}           # typed by the selected built-in driver
      native_parameters: {}  # bounded string map validated by that driver
    persistence:
      mode: durable
      volumes: [database-data]
```

`driver`, `profile`, and `image` are explicit. `latest` and unqualified image names are rejected. `type` states product semantics; `driver` selects a concrete versioned operational contract and permits future alternate built-in implementations without changing component type. The first release accepts only compiled-in driver ids.

`settings` is a CUE-discriminated, typed structure owned by the driver. It is not `map[string]any`. `native_parameters` is the bounded escape hatch for legitimate upstream settings that do not merit first-class fields. Each driver validates parameter names and scalar string values against its supported service/image version, rejects duplicates and control characters, classifies their effects, and maintains a protected-key set. Data directory, service identity, secret delivery, control labels, managed network, and health/verification wiring are always protected.

There is no `compose_overrides` field. Users needing entrypoint replacement, arbitrary mounts, capabilities, privileged mode, host networking, custom lifecycle scripts, or unsupported native configuration use the existing Compose-owned mode. This keeps managed guarantees honest without reducing Onebox's overall flexibility.

Alternative considered: a fully open settings or Compose merge map. Rejected because merge precedence becomes part of the safety contract, unknown keys evade validation, and overrides can silently defeat persistence or verification.

### 4. Defaults are layered, immutable by identifier, and observable

Resolution order is:

1. Driver invariants — mandatory and not configurable.
2. Explicit user values.
3. Versioned profile defaults.
4. Pinned upstream image defaults.

Profile identifiers are immutable. Changing a value or meaning creates a new profile id, such as `standard/v2`; upgrading Onebox cannot mutate `standard/v1`. Scaffolding writes an explicit recommended driver, profile, and image rather than relying on an evolving binary default. The image is always explicit because service image changes can imply data-format changes.

Onebox reports concrete invariant, user, and profile values with origin and effect. It does not clone every PostgreSQL or Redis upstream default into its own schema. An unset native setting is shown as `delegated to pinned upstream image`; after provisioning, a driver may read an allowlisted runtime value and report it as observed upstream state. This avoids false precision when upstream defaults depend on image build, architecture, or service version.

Every setting descriptor includes:

- JSON/CUE type, validation, unit, and safe description.
- Secret and model-visibility classification.
- Origin and selected profile.
- Effect: `live`, `reload`, `restart`, `recreate`, `upgrade`, `destructive`, or `unsupported`.
- Whether the value is desired, applied, or observed upstream state.
- Deprecation and replacement metadata.

Resource limits are part of the common envelope. The foundation supports explicit memory, reservation, CPU, and process/file limits. A production provider profile may require a memory budget or environment policy may reject unbounded services. Onebox does not calculate a hidden limit from host size; planning validates declared aggregate budgets and host reserve using observed capacity.

Alternative considered: automatically select “sensible” database tuning from host RAM. Rejected because workload, co-tenancy, swap, kernel, and database size make it non-deterministic and potentially dangerous.

### 5. Drivers are built-in typed policy modules, not executable packages

The internal registry maps exact contract ids to compiled Go drivers. A driver provides pure metadata and narrowly scoped lifecycle behavior:

```text
Descriptor          catalog schema, profiles, secret slots, capabilities
Resolve             authored config -> effective typed config + origins
ValidateProject     cross-component and host-independent validation
Render              deterministic Compose/config payload
Inspect             bounded typed actual state
Classify            desired + applied + actual -> effect/risk/refusals
ValidateStaged      offline/remote pre-mutation validation
Verify              bounded post-convergence evidence
```

Future provider changes may add optional `Protect`, `Restore`, and `Upgrade` capabilities with separate plan kinds. Absence of a capability is reported explicitly and cannot fall back to arbitrary hooks.

Drivers receive typed values and a transport abstraction. Native parameter names and values are treated as data and never interpolated unvalidated into a shell program. Remote commands use fixed templates, validated identifiers, and existing quoting helpers. Driver output is separated into trusted local diagnostics and redaction-safe public facts.

Alternative considered: download templates or scripts from a registry. Rejected for the first production boundary because it creates a code-signing, sandboxing, provenance, update, and support problem before driver demand is proven.

### 6. Generated services have independent projects and immutable revisions

Each managed component receives deterministic identities derived from validated app and component names, with a hash suffix if Docker length limits require it:

```text
Compose project: ob-<app>-svc-<component>
Service alias:   <component>
Shared network:  ob-<app>-services
Remote root:     /var/lib/ob/<app>/services/<component>/
Volume names:    ob-<app>-<component>-<logical-volume>
```

The remote root contains immutable configuration revisions and atomic state:

```text
services/<component>/
├── revisions/<payload-digest>/
│   ├── compose.yaml
│   └── config/
├── applied.json
└── operations/
```

The stable Compose project name preserves logical service, network, and volume identity while an explicit Compose file from one immutable revision controls convergence. Containers may be recreated and receive new runtime ids. `applied.json` is written atomically only after verification and records driver/profile, authored image, resolved digest, payload digest, service version, volume identities, operation id, and verified time. Secret files are operation-scoped or revision-scoped mode-`0600`, excluded from public digests, unlinked when no longer required, and never described as cryptographically erased by ordinary filesystem deletion; they are not copied into immutable non-secret evidence.

The managed network is created explicitly under the app lock and labeled with Onebox ownership. Generated services and application roles attach to it. Aliases are checked for conflict before mutation. Managed service ports are not published to the host by default.

Named volumes have explicit names and Onebox labels and are never children of release retention. Ordinary Compose commands never use `down -v`. Volume deletion belongs to a future dedicated destructive plan.

Alternative considered: merge generated services into the application release Compose. Rejected because application rollback or retention could then change database images/configuration and blur service ownership.

### 7. Managed-service plans fail closed and bind both intent and reality

`ManagedServicePlan` is a new executable envelope containing the existing canonical `OperationPlan` plus a managed-service artifact. It has its own schema version and runner compatibility list. The operation graph adds service stage, converge, and verify steps; future backup/restore/upgrade steps use new plan kinds rather than overloading apply.

The plan binds:

- Authored config and resolved effective-config digests.
- Driver contract/profile and runner provenance.
- Encrypted secret source revision plus selected logical slots, never plaintext hashes.
- Authored image and required immutable manifest digest.
- Target, app, environment, component, project, network, and volume identities.
- Applied record digest and actual container image/mount/network facts.
- Observed service and data-format versions.
- Observation completeness and warnings used for classification.
- Expected action, interruption, verification, risk, reversibility, approval, and expiry.

Managed images must resolve to a registry digest. The existing “warn and remain tag-bound” behavior for some application images is not sufficient for stateful managed services. Multi-platform manifest selection and target architecture are bound before execution.

Planning is read-only: it may inspect the registry and target but cannot create a network, volume, secret file, or container. Execution reacquires the app lock and recomputes every binding. Any mismatch requires a new proposal.

Change effects are ordered by severity: `no_op < live < reload < restart < recreate < upgrade < destructive`. The driver may raise but never lower the common classifier. `force` cannot bypass stale bindings, image pinning, incomplete observation, protected invariants, persistent-volume effects, or upgrade-only transitions. Break-glass is a distinct operation kind with explicit reason and stronger approval.

### 8. Approval is a separate trusted capability

Conversation text and model tool arguments are untrusted intent. They never constitute approval. A required approval is issued by a verified user identity, scoped to the exact plan digest, target, effects, approval class, and expiry.

The agent initiates an approval handoff to a trusted dashboard or client surface only when authority is required; users are not expected to navigate a separate operations UI. The handoff contains an opaque proposal id and non-sensitive display metadata, not a bearer credential in a model-visible URL. Sensitive setup, including secret entry or provider credentials, uses HTTPS URL-mode elicitation or a separate authenticated surface. Form-mode elicitation never asks for credentials.

For remote HTTP MCP, the server uses OAuth 2.1 semantics, PKCE for public clients, exact redirect validation, short-lived tokens, resource/audience binding, and no token passthrough. Production credentials used for SSH, registries, storage, or KMS remain owned by the Onebox execution boundary and are not returned to the MCP client.

The local CLI approval artifact may remain for offline break glass, but it is not the primary UX and is validated by the same canonical approval service.

### 9. Execution is a crash-consistent state machine

Execution follows this order:

1. Atomically accept the approved operation idempotently.
2. Connect, acquire the app lock, write a new fence epoch, and start heartbeat.
3. Re-observe and verify all plan bindings under authority.
4. Create an operation-scoped local and remote staging directory.
5. Render, hash, upload, and validate the complete staged payload.
6. Atomically rename staging into its immutable revision directory.
7. Execute only the plan-authorized driver action.
8. Re-inspect actual container, image, network, mounts, service version, and data-format facts.
9. Run bounded driver health and verification checks.
10. Atomically write `applied.json`, then append successful terminal journal evidence.
11. Release authority and publish terminal operation state.

Every mutating host command is fence-guarded. Applied state is never inferred from uploaded files. A matching `applied.json` is not a no-op unless actual state also matches and verifies.

On failure, the desired applied digest is not written. The previous immutable revision and all volumes remain. The journal records which state could be positively observed. A driver may request automatic configuration rollback only when the sealed plan classified it safe and can verify the old state afterward; otherwise execution halts and preserves evidence. Ordinary apply never reinitializes, detaches, or deletes a volume.

Cancellation stops cooperatively, records a terminal cancelled operation, and performs bounded cleanup with a fresh short-lived context. Because a remote command may complete after cancellation, final status is determined by re-observation; task status remains cancelled as required by asynchronous-operation semantics.

### 10. Secrets use per-slot files and non-plaintext bindings

The existing SOPS renderer is extended to parse the encrypted source once and project selected keys into separate component-scoped files. Drivers prefer upstream `_FILE` mechanisms or read-only secret mounts. A driver that must use an environment variable receives only that slot, not the full application environment.

Plans bind the encrypted source bytes digest, source identity, and slot mapping. They do not hash plaintext into a client-visible artifact because low-entropy passwords can be attacked from hashes. Execution verifies the encrypted-source binding before decrypting. Plaintext exists only in memory and protected temporary files for the shortest practical period.

Command strings, journal details, events, diffs, operation records, and observations are tested against sentinel secrets. Public errors use stable codes and field paths. Raw remote stderr and logs stay on a separately authorized, bounded trusted-local path.

### 11. Observation is a three-way comparison with completeness

Managed observation reports desired, applied, and actual state separately. It gathers independent SSH reads concurrently, as current status does, but each field retains provenance and read status. Positive absence is distinct from failure. Missing required evidence marks the component and aggregate observation incomplete rather than healthy.

Observation also enumerates bounded Onebox-labeled resources and managed-service roots for the selected application so it can report applied services that no longer have a desired component. Failure to enumerate either source makes orphan evidence incomplete; absence is never inferred from an unreadable directory or Docker query.

Actual state includes only bounded allowlisted facts: container/image digest, status/health, restart count, network alias, mount/volume identity, service/data version, and driver verification facts. Settings observation is allowlisted per driver. Untrusted service text, configuration bodies, and logs are not returned automatically.

Large lists use deterministic ordering and pagination. Large diffs and journals are stored as redaction-safe resources referenced by opaque ids. Agent-facing errors contain code, component, phase, field path, retryability, required action, correlation id, and bounded summary.

### 12. Application operations only depend on managed-service facts

Application planning observes required managed components and binds a compact service-state digest. A missing, unhealthy, incompletely observed, or incompatibly drifted managed service blocks the app plan and returns a structured next action suitable for `onebox_propose_service_change`.

App deploy, resume, abort, and rollback never invoke managed-service convergence. Application Compose receives only the external managed network and required aliases. Rolling back the app therefore cannot downgrade a database or cache.

### 13. Adoption, detach, destroy, and provider upgrades are separate contracts

The foundation can recognize existing managed state produced by the same driver contract and valid applied record. It does not adopt arbitrary Compose services or volumes. Adoption requires provider-specific inspection, backup evidence, identity mapping, and an explicit future plan.

Removing `managed` while target state exists produces an orphan warning and blocks silent mutation. Detach transfers ownership without deleting resources; destroy is a dedicated destructive plan. Service major upgrades are provider-specific upgrade plans and cannot pass through ordinary apply.

### 14. Compatibility and rollout are schema-gated

The authoring change is additive: existing Compose-owned components remain valid. The managed envelope is closed and discriminated by driver id. Exact provider `settings` schemas arrive with each provider change. An older runner may reject the new field, so projects using it set an appropriate `minimum_onebox_version` and executable plan schema.

`ManagedServicePlan` begins at `onebox.run/executable-managed-service-plan/v1alpha1`; operation steps and results receive corresponding schema bumps. Runner provenance advertises exact support. Unsupported schemas fail before interpretation.

Rolling the Onebox binary back does not stop a running managed service. The older runner cannot safely plan or mutate the new component and reports a compatibility error. Immutable service revisions and remote journals preserve recovery evidence.

### 15. Verification strategy is fault-oriented

The foundation is not complete on unit happy paths alone. Required coverage includes:

- CUE and Go contract tests for every ownership union, setting type, profile, native parameter, secret slot, and compatibility error.
- Golden catalog/tool schemas and structured-content validation.
- Deterministic canonicalization, rendering, hashing, naming, ordering, and pagination tests.
- Fuzz tests for names, native parameters, YAML/JSON decoding, redaction, error bounding, and canonical digests.
- Sentinel-secret tests over plans, resources, events, journals, errors, commands, and observations.
- Fake-transport fault injection before and after every state-machine boundary: lock, fence, upload, rename, Compose action, inspection, verification, applied-state write, and journal finish.
- Crash/restart tests proving operation idempotency and reconstruction from remote journal evidence.
- Concurrency tests for duplicate MCP calls, same proposal retries, competing app operations, cancellation, and lost notifications.
- Plan-binding tests for config, profile, image, secret source, network, volume, applied state, container image, service version, target, runner, approval, and expiry drift.
- Opt-in Docker end-to-end tests with a minimal test driver before PostgreSQL and Redis drivers rely on the substrate.
- MCP protocol/conformance tests for schemas, annotations, elicitation safety, authorization failures, response bounds, and malformed internal output.
- Performance budgets for proposal and observation SSH round trips, catalog response size, operation polling, and journal reconstruction.

No provider can claim production readiness until its own change adds data-integrity, upgrade, backup/restore, and provider failure-injection tests appropriate to its semantics.

### 16. Documentation distinguishes implemented and proposed behavior

`README.md`, `docs/schema-v1.md`, and `docs/mcp.md` describe the current binary. While this change is active, its proposal, specifications, design, and tasks are the normative source for the proposed managed-service behavior but do not make that behavior available. `docs/product.md` summarizes direction without duplicating the contract.

Examples of managed fields and tools outside the OpenSpec change are labeled non-executable until implementation and conformance tests pass. During archive, current guides are updated only for behavior that actually shipped. Provider enablement remains proposed until its own change is reviewed, implemented, verified, and archived.

## Risks / Trade-offs

- [The managed envelope still cannot express every upstream option] → Keep a bounded native-parameter map and preserve Compose-owned mode as the unrestricted escape hatch.
- [Versioned profiles increase visible configuration] → Scaffold explicit recommended values and let the agent explain only fields relevant to the current decision.
- [An unbounded service can exhaust the single host] → Expose resource limits, report unbounded state, allow environment policy to require limits, and validate aggregate host reserve during planning.
- [Cross-project aliases can conflict] → Derive and inspect deterministic network aliases and refuse conflicts before mutation.
- [Local MCP process loss can obscure progress] → Persist accepted operation state atomically and reconstruct effects from the remote append-only journal.
- [The local operation repository is not a team approval system] → Keep it as development/break-glass storage; production approval and identity use the authenticated control-plane repository.
- [Native settings may change meaning across service versions] → Bind driver, image digest, and observed service version; validate against the pinned image and route version transitions through provider upgrade plans.
- [A configuration change can start a container but damage service behavior] → Require driver verification before applied evidence and permit automatic rollback only when explicitly classified and verified safe.
- [Secret references can leak through errors or hashes] → Bind encrypted-source revisions, project only named slots, avoid plaintext hashes, and run sentinel-secret tests across every public surface.
- [LLMs may retry or reorder calls] → Require idempotency keys, stable proposal/operation ids, immutable transitions, and authoritative polling.
- [Tool annotations can create false trust] → Treat annotations as descriptive only; enforce all authorization, approval, and validation server-side.
- [MCP protocol and SDK capabilities evolve] → Keep canonical operations protocol-independent and add transport adapters without changing operation identity or safety semantics.
- [The foundation is large before a real provider exists] → Implement it in vertical slices with a minimal test driver, then validate every abstraction immediately against PostgreSQL; do not build a registry or optional capability not demanded by that driver.
- [Agents may mistake roadmap prose for available tools] → Keep current guides implementation-only, label proposed examples, expose runner capabilities through MCP, and update shipped documentation only at archive.

## Migration Plan

1. Add the closed managed envelope, versioned driver registry, catalog types, and compatibility reporting with no production driver enabled. Existing Compose-owned behavior remains unchanged.
2. Add canonical managed proposal, plan, approval, operation repository, and observation types with a deterministic in-process test driver.
3. Add the independent remote layout, network/volume identity helpers, state machine, journals, and fault-injection tests.
4. Add the LLM-first MCP catalog, proposal, operation polling, approved execution, and cancellation tools with strict schemas and safe errors. Keep execution disabled unless a trusted approval provider is configured.
5. Add thin CLI adapters only where needed for tests and break glass.
6. Validate the substrate with opt-in Docker end-to-end tests, then archive this foundation change.
7. Implement `managed-postgres-driver` as the first consumer. Do not enable a default managed declaration until that change passes provider production-readiness gates.
8. Implement protection/restore and Redis as separate reviewed changes.

Rollback of foundation code is straightforward while no provider is enabled. After a provider is enabled, rolling back the binary leaves containers and volumes running but removes mutation authority; operators restore a compatible runner rather than rewriting remote state. No migration step deletes generated networks, service revisions, operation records, or volumes.
