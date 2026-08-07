## Why

Onebox already provides a state-bound, recoverable release engine, but its OSS
product is not yet a complete single-host production baseline: it does not
create or restore backups, continuously detect operational failures, own log
and image growth, model external dependencies, or ship the CI handoff needed
for build-sourced workloads. This leaves the highest-consequence work outside
the ownership boundary while supporting services can only honestly remain at
the `Run` tier.

## What Changes

- Add driver-owned, encrypted backup, restore, retention, and restore-drill
  operations using user-owned off-host storage and host-native schedules.
  Onebox standardizes policy, evidence, locking, and recovery safety while each
  service driver selects its native consistency and restore engine. Backup
  artifacts and evidence bind the application, environment, service, driver,
  version, recovery kind, method, destination, and exact protected resources.
- Resolve each service runtime that enables managed protection to an immutable
  image digest while leaving unprotected service rendering unchanged, and ship
  digest-pinned Onebox service/helper artifacts where native recovery tooling
  must execute inside or against that runtime. PostgreSQL uses a derived
  PostgreSQL-plus-pgBackRest image built from a pinned upstream base; physical
  MySQL/MariaDB helpers receive only their contract-bound data and credential
  mounts. Every derived artifact carries reproducible-build provenance and is
  retained as a restore dependency.
- Add safe restore choreography that restores into an isolated volume, verifies
  it with an exact-compatible temporary service, requires a plan-bound strong
  approval for live cutover, preserves the prior volume, and never treats a
  failed restore as permission to destroy either copy.
- Introduce explicit `Run` and `Managed` service-tier reporting plus the actual
  recovery envelope (`snapshot`, `pitr`, `cold`, or `replicated`). Every current
  durable driver—PostgreSQL, MySQL, MariaDB, MongoDB, ClickHouse, Redis, Valkey,
  RabbitMQ, MinIO, Meilisearch, and NATS JetStream—gets an independent native
  qualification gate. A driver graduates only after its supported-version,
  consistency, empty-target restore, isolated restore drill, cancellation,
  stale-evidence, and crash-recovery suites pass. Conditional contracts remain
  `Run` until their prerequisites are met. Newly created MongoDB services
  become authenticated single-node replica sets with idempotent initialization,
  PRIMARY health, and replica-set connection projection; existing standalone
  volumes require an explicit state-bound conversion. RabbitMQ gets a stable
  protected node identity and message protection needs an authorized
  stopped-node window; newly created NATS services get generated account
  credentials and a pinned external CLI health probe, while existing
  unauthenticated runtimes require an explicit state-bound conversion. MinIO
  needs an operator-provisioned, independently operated, versioned second
  MinIO deployment outside the protected host plus recovery proof. Enabling
  PostgreSQL/MariaDB/ClickHouse protection may require a separately planned,
  strongly approved one-time service restart; it is never hidden inside
  ordinary schedule convergence.
- Add rollback-aware image pruning, managed container-log rotation, disk
  thresholds, scheduled housekeeping, and status/doctor evidence for each.
- Add a host-native local watchdog for workload/service health, disk pressure,
  certificate runway, backup freshness, and restore-drill freshness. It emits
  the existing webhook notification contract and runs without a resident
  Onebox process.
- Add typed external-service connections for dependencies such as RDS, Neon,
  Supabase, Atlas, and Upstash. Onebox validates their secret-safe connection
  projection and declared protection ownership but does not provision, back up,
  upgrade, or present them as Onebox-managed.
- Ship an official CI workflow that builds and pushes build-sourced workloads,
  resolves immutable image digests, and invokes the existing plan, approval,
  and deploy path without creating a second lifecycle implementation.
- Extend every new CLI operation with versioned JSON/NDJSON results, typed
  secret-free failures, stable identifiers, resolving next commands, and
  idempotent retry behavior.

Today, Onebox only validates externally produced migration-backup evidence,
streams current logs, reports declared observability intent, prunes release
directories and journals, and requires an external image for production
builds. All backup execution, restore execution, continuous checks, log
rotation, rollback-aware image pruning, typed external services, and the CI
workflow above remain proposed until this change is implemented, documented,
strict-validated, and archived.

Compatibility is additive within `onebox.run/v1`. Existing projects continue
to load and keep their present behavior; services without protection policies
keep the current version-tag runtime and do not require registry resolution. A
durable service without a qualified backup policy continues to report `Run`
and unprotected. Enabling protection may resolve, plan, and apply an exact
service image digest as an explicit prerequisite. New structured CLI
documents receive new schema identities rather than changing existing output
shapes. Generated backup units, manifests, restore volumes, and protection
state become Onebox-owned inspectable artifacts on the target; backup bytes
remain in storage owned and paid for by the user.

Explicit non-goals:

- No generic live `tar` of database volumes and no claim that arbitrary
  workload data is transactionally consistent.
- No automatic destructive in-place restore, implicit volume deletion, or
  removal of the pre-restore volume before the retention contract permits it.
- No major-version service upgrade, credential rotation, adoption, detach, or
  arbitrary workload-volume protection in this change. Point-in-time recovery
  is implemented only where a qualified driver has a native log-replay path;
  it is never inferred from periodic snapshots.
- No generic repository format or promise that one backup executable is
  appropriate for every service. Restic is limited to encrypted transport and
  retention for driver-produced artifacts; it is not a database consistency
  engine.
- No claim of hot RabbitMQ message backup, no MinIO replication back into the
  source deployment or its host, and no `Managed` graduation from definitions,
  object mirroring, checksums, or replication status without an actual recovery
  drill.
- No cloud database provisioning or provider-specific AWS, GCP, Azure, Atlas,
  Neon, Supabase, or Upstash lifecycle behavior. The first backup destination
  contract is portable S3-compatible object storage.
- No hosted backup storage, central control plane, SSO/RBAC, organization
  policy, independently issued approval, central audit, compliance reporting,
  multi-host failover, autoscaling, canary delivery, or full metrics/logging
  backend. Those are separate premium or future capabilities; basic local
  backup and restore are not paywalled.

## Capabilities

### New Capabilities

- `managed-data-protection`: Driver-native backup, encrypted off-host storage,
  retention, restore, restore drills, evidence, tier graduation, and safe
  recovery choreography for qualified supporting services.
- `host-operational-hygiene`: Managed log rotation, rollback-aware image
  pruning, disk protection, housekeeping schedules, and their observable
  failure behavior.
- `local-continuous-assurance`: Agentless host timers that continuously check
  health, certificates, disk, backup freshness, and restore proof, then deliver
  compact webhook outcomes.
- `external-service-connections`: Typed, secret-safe dependencies whose
  lifecycle and protection remain explicitly external to Onebox.
- `ci-image-delivery`: An official digest-pinned CI handoff into the canonical
  Onebox planning, approval, and execution service.

### Modified Capabilities

- `project-schema`: Accept declared backup targets and policies, inputs needed
  for derived service-tier reporting, and external-service connections only
  where executable behavior and honest ownership reporting now exist.
- `runtime-generation`: Generate and bind qualified service-protection
  artifacts, restore-volume selection, housekeeping/watchdog units, and the
  current driver-backed service runtime instead of treating service
  declarations as permanently inert.

## Impact

- Extends the closed project schema, generated JSON Schema, canonical output,
  conformance corpus, service driver model, operation graph, plans, approvals,
  journals, status, doctor, notifications, and documentation authority map.
- Adds canonical Go service operations and CLI groups for backup and restore;
  the CLI remains the only adapter and all lifecycle decisions remain in the
  shared service and execution engine.
- Adds a short-lived target-side scheduled-runner artifact, sealed operation
  envelopes, service protection state, generated systemd units, temporary
  restore projects/volumes, and rollback points under the existing Onebox-owned
  layout. Runner installation, bidirectional compatibility, provenance, and
  removal become owned lifecycle behavior.
- Adds a pinned driver-native helper matrix—pgBackRest, Percona XtraBackup,
  MariaDB Backup, Percona Backup for MongoDB, and native ClickHouse,
  Redis/Valkey, RabbitMQ, MinIO, Meilisearch, and NATS tools—plus Restic only
  where a native driver emits an artifact rather than owning an off-host
  repository. It also adds derived service-image build, publication, SBOM,
  digest, and provenance verification for tools such as pgBackRest that must
  be present inside the service container, including a closed mapping from a
  declared PostgreSQL version to a qualified upstream patch/base digest and
  derived-image digest. Plaintext data and credentials may
  not enter plans, logs, journals, structured output, or model-visible
  arguments.
- Adds fault-oriented integration coverage for interrupted streams, partial
  uploads, unavailable storage, overlapping deploys, stale locks, expired
  approvals, corrupted artifacts, incompatible versions, failed verification,
  timer retries, notification failure, and runner disconnection.
- Requires README, schema, CLI, and product documentation to distinguish
  shipped OSS protection from premium/future assurance and to report each
  service tier without overstating protection.
