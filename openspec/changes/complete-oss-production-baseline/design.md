## Context

See `proposal.md` for motivation and scope. The existing engine already has the
pieces consequential protection must inherit: SSH/local transports, exact
plans and approvals, app and host locks, fencing epochs, append-only operation
journals, generated Compose, host systemd schedules, typed service drivers,
status/doctor, and recovery gates. It does not have a driver lifecycle
interface, backup repository, active-volume indirection, or a canonical
scheduled runner for operations that must happen while the CLI is absent.

Onebox is agentless in the sense that no resident daemon or listening control
port runs on the target. Scheduled jobs already demonstrate the acceptable
shape: a generated host timer starts a bounded process and exits. The new
backup, restore-drill, housekeeping, and assurance paths must keep that shape
without embedding a second implementation of lifecycle decisions in shell.

Current service runtimes use a stable Compose project and stable named volume.
That protects data from application rollback but gives restore no atomic
cutover primitive. Current service credentials are generated once on the
target, while storage credentials can already travel only through trusted
secret-file flows. Existing projects have no protection fields and must remain
valid and visibly unprotected.

## Goals / Non-Goals

**Goals:**

- Give a single OSS operator a complete protection path for every built-in
  durable service using storage they own, including native consistency,
  scheduled backup or replication, retention, real restore testing, and
  recoverable live cutover.
- Keep all behavior authority in the canonical Go service and reuse the
  engine's plan, lock, fence, journal, approval, structured-event, and recovery
  contracts.
- Make tier, freshness, drift, ownership, defaults, and evidence origins
  observable without revealing credentials or backup content.
- Keep generated runtime, schedules, state, manifests, and helper provenance
  inspectable and removable while never treating removal as permission to
  delete user data or remote backup bytes.
- Establish driver capability seams that use each service's real recovery
  model without hiding weaker snapshot, cold, or replication semantics behind
  a generic `Managed` label.

**Non-Goals:**

- No cross-host database failover, clustered-database orchestration,
  application-consistency protocol for arbitrary workloads, or automatic
  major-version migration. PITR exists only for separately qualified native
  WAL, binlog, or oplog contracts.
- No generic cloud-provider provisioning, hosted repository, central policy,
  central audit, team identity, or independent approval issuer.
- No resident Onebox control daemon, inbound target API, or second user-facing
  adapter. A database-native archive hook or helper process required by a
  qualified driver remains part of that service runtime, not a control plane.
- No user-authored service tier. Tier is always derived from observed evidence.

## Decisions

### 1. Split runtime description from lifecycle capabilities

The current data-driven driver catalogue remains the authority for image,
port, data path, health, credentials, connection shape, settings, and runtime
rendering. A new lifecycle capability record is attached per driver and names:

- supported recovery kinds (`snapshot`, `pitr`, `cold`, `replicated`) and
  backup format schemas;
- pinned helper image and compatible service-version range;
- service-image delivery class (`upstream-digest`, `derived-image`, or
  `external-helper`), its pinned upstream base, build provenance, and SBOM;
- native consistency, log-archive or replication, restore, startup, and
  verification operations;
- whether online backup is supported, expected interruption, achievable RPO,
  and required data-engine or topology preconditions;
- whether the engine writes directly to its repository or emits an artifact
  for the common encrypted artifact transport;
- the resources and credential slots required by each operation;
- the encryption mode, native-retention mapping, service-image digest, and any
  one-time enablement restart required by the contract;
- explicit graduation evidence.

Every durable catalogue entry has a record, but a driver/version/objective
combination not yet qualified remains `Run`. There is no default protection
implementation and no automatic fallback to volume copying. This keeps adding
a runnable service cheap while making the stronger `Managed` claim deliberately
expensive and independently testable.

Tools invoked by a process inside a service container are delivered in that
service runtime. PostgreSQL therefore uses a Onebox-built
PostgreSQL-plus-pgBackRest image derived reproducibly from the catalogue's
pinned upstream PostgreSQL digest. The build publishes an immutable digest,
SBOM, source/base provenance, and compatibility record; restore retains and
uses that exact digest. Tools that can safely operate from a helper container,
including XtraBackup and MariaDB Backup, remain separately pinned helpers with
contract-specific read/write data-volume mounts, database sockets or network
access, credentials, UID/GID compatibility, and explicit prepare/restore
ownership. ClickHouse configuration is a generated, digest-bound mount read by
the service rather than an untracked host edit.

The PostgreSQL capability release record is the closed mapping from an authored
selector such as `postgres: 17` plus an exact upstream PostgreSQL patch/base
digest to a compatible pgBackRest version, derived image digest, publication
time, and support state. For an existing service, first protection enablement
selects only a qualified derived image built over the service's observed current
upstream base; it never smuggles a PostgreSQL patch upgrade into protection
enablement. A newly created protected service selects the latest qualified
mapping for its selector. The selected mapping binds into the plan, and
ordinary apply does not chase a moved upstream tag. Existing protected services
keep their recorded mapping until a separately planned patch update. A first
protection enablement for which no matching derived image is published refuses with
`protection_service_image_unpublished` and leaves the service unchanged and
`Run`. When a previously recorded digest is present locally and verifies, apply
and restore may use that cache during registry outage; otherwise they refuse
with `service_image_digest_unavailable`. Unprotected services continue using
the current version-tag rendering and gain no registry-resolution dependency.

An internal non-graduating test driver implements every lifecycle seam before
the first production driver. It is unavailable in project schema, status, and
documentation and can never report `Managed`; it exists only so repository,
manifest, restore, cutover, and drill foundations are independently testable.

Alternative considered: back up every Docker volume with one archive command.
Rejected because a readable archive of live database files is not necessarily
a transactionally consistent or version-compatible backup.

### 2. Separate native consistency from repository transport

The protection pipeline has two independently versioned interfaces:

1. A driver contract creates a consistent recoverable generation, log segment,
   stream snapshot, or replica and knows how to restore and verify it.
2. A repository contract stores or identifies driver output off host with
   authenticated identity, an explicit encryption mode, integrity, retry,
   retention, and listing or replica observation.

Drivers with a native remote repository use it directly: pgBackRest,
Percona Backup for MongoDB, ClickHouse `BACKUP`/`RESTORE`, and MinIO replication
retain their own repository and continuity semantics. Drivers that emit a
bounded file or directory use a pinned Restic artifact repository: physical
MySQL/MariaDB streams, Redis/Valkey RDB files, RabbitMQ stopped-node archives,
Meilisearch snapshots/dumps, and NATS account snapshots. Restic is therefore
an encrypted artifact transport, not a database consistency engine and not a
reason to give unrelated drivers the same recovery claim.

The first common destination is user-owned S3-compatible storage. It declares
endpoint, bucket, prefix, region, TLS policy, encryption policy, secret
reference, and a failure-domain identity. Direct engines receive target-side
mode-0600 generated configuration. Artifact engines receive a distinct Restic
repository prefix and password. Plans and units contain references only. A
local filesystem target exists only in tests and never counts as off-host.
MinIO replication additionally requires a distinct remote deployment identity;
Onebox refuses a destination resolving to the protected MinIO instance, its
host, or its data volume.

Encryption is capability data, not a uniform verb. Each driver/target record
chooses and proves one of `client-side`, `archive-password`,
`server-side-sse`, or `replica-inherited`. Status and manifests name the active
mode and its evidence. ClickHouse native S3 protection qualifies only when its
selected archive-password or target SSE contract is observed; MinIO
replication records inherited source/target encryption and KMS compatibility
without claiming Onebox re-encrypted replicated objects. A contract whose
declared encryption policy cannot be proven remains `Run`.

When an engine can stream, the pipeline never creates a plaintext intermediate.
When a native API can only materialize a file, it writes to a mode-0700,
operation-scoped, capacity-bounded staging directory. The directory is not
protection evidence, is removed after verified upload, and remains named in the
journal with an explicit cleanup command after failure. Onebox does not invent
its own encryption envelope or multipart protocol.

Every recoverable point gets a sealed secret-free Onebox protection manifest
containing schema,
app/environment/service identity, driver and service image digest, recovery
kind, native method and helper provenance, protected resources, repository
identity, encryption mode, native backup or replica identity, base and replay
range when applicable, artifact digests and size only when artifacts exist,
operation, interruption, and timestamps. A `replicated` record carries replica
identity, observation range, lag, version/delete coverage, and metadata scope;
it does not invent a backup generation, artifact digest, or size.
Native repository metadata remains authoritative for restore; the manifest
binds that metadata into Onebox's evidence and ownership model.

Alternative considered: use Restic for all live data volumes. Rejected because
repository safety does not create database consistency. Alternative considered:
standardize on WAL-G across several engines. Rejected because a common binary
does not erase different restore, compatibility, and failure contracts, and it
would make tool reuse—not recovery correctness—the architectural boundary.
Alternative considered: build a custom Go object repository. Rejected because
it would duplicate mature encryption, resume, locking, and retention behavior.

### 3. Run schedules through a short-lived canonical runner

Applying protection uploads a pinned, digest-verified `ob-scheduled-runner`
artifact and a sealed scheduled-operation envelope under the Onebox host
layout. A generated systemd timer launches the runner with local transport,
the envelope path, and secret-file references. The runner imports the same
canonical Go service and operation graph as the CLI, accepts only scheduled
operation schemas, opens no port, exits after one operation, and cannot mint an
approval. It is an execution artifact, not another adapter or resident agent.

The runner enforces lock, fence, journal, retry identity, helper provenance,
and redaction exactly as a CLI-triggered operation. Updating Onebox regenerates
the envelope and runner provenance. The CLI verifies the installed runner's
digest and signed or transparency-verifiable publication provenance before
use. Compatibility is bidirectional: the envelope declares the minimum and
maximum runner protocol, while the runner declares the CLI/envelope protocols
it accepts. Either an older CLI facing a newer incompatible runner or a newer
CLI facing an older incompatible runner refuses without mutation and names the
matching upgrade/apply command. `ob destroy` removes unreferenced runners,
envelopes, and units from the Onebox-owned layout while preserving repositories,
replicas, manifests, and service data.

Alternative considered: generate shell scripts containing backup logic.
Rejected because locking, failure classification, redaction, retry, and
journaling would become a second lifecycle implementation. Alternative
considered: require the operator's workstation or CI to stay online. Rejected
because a backup schedule must survive disconnection and reboot.

### 4. Protection operations extend the canonical operation graph

New operation kinds are `backup_create`, `backup_prune`, `replay_archive`,
`replication_check`, `restore_test`, `restore_prepare`, `restore_cutover`,
`restore_abort`, `hygiene_run`, and `assurance_check`. Each has a random
operation identity, state binding, supported runner schemas, deterministic step
identities, and terminal result. Database-invoked archive hooks use a restricted
sub-envelope bound to their owning service and may append archive evidence but
cannot invoke arbitrary operations.

A service protection lock is nested under the existing app lock. Backup,
service apply, migrations, restore, and credential-affecting operations are
mutually exclusive for a service. Replay-log upload and native replication may
run concurrently only where the driver contract proves that concurrency safe.
Ordinary application writes continue during qualified online methods; a cold
contract obtains explicit interruption approval before stopping the service.
A version, engine, topology, or memory-headroom precondition may refuse online
backup. Scheduled lock contention ends
with a typed retryable result and bounded randomized delay; it never breaks a
lock or silently skips without evidence.

Cancellation marks the current step incomplete, terminates helper containers,
and records any remote partial snapshot as ineligible. A retry with the same
identity resumes a supported partial transfer or returns the existing terminal
result. A new schedule occurrence receives a new identity and first reconciles
any prior incomplete occurrence.

Protection enablement is separate from recurring backup interruption. A
contract that must enable `archive_mode`, binary logging, a named collection,
or another restart-bound prerequisite emits a one-time state-bound enablement
plan that names the configuration delta, expected outage, rollback, and health
checks. Apply refuses with `protection_enablement_restart_not_authorized` until
a fresh strong approval arrives independently of model text. The approved
operation writes the generated configuration, deliberately restarts and
verifies the service, and records how the prerequisite was established. That
record is provenance, not continuing proof: backup preflight, status, doctor,
and assurance re-observe the effective runtime prerequisite and configuration
digest. Drift immediately reports `protection_prerequisite_drifted`, blocks new
backup work that depends on it, and keeps or returns the service to `Run` until
an approved enablement plan re-establishes it. A policy's recurring
interruption permission cannot authorize this restart, and the scheduled
runner can never perform it implicitly. MariaDB receives a driver-owned
`log_bin` configuration because its project settings surface cannot enable
binary logging; PostgreSQL archive settings and ClickHouse named collections
use the same planned-enablement contract where their effective runtime changes.

### 5. Derive service tier from evidence, never configuration

`Run`, `Managed`, and `External` are output states, not author-set fields.

- `Run`: Onebox runs the service, but at least one managed-tier requirement is
  missing, stale, failed, unsupported, or unverifiable.
- `Managed`: the driver is qualified; the service image has been resolved to
  an immutable digest and that exact digest is effective; resources are effective;
  driver health passes; the installed schedule or replication contract matches
  the declaration; the latest recoverable point satisfies the authored RPO;
  replay continuity or replica completeness passes where applicable; and
  restore-drill proof is fresh.
- `External`: Onebox projects and observes a dependency but owns none of its
  lifecycle.

Status carries each contributing fact, its source, observation time, expiry,
and resolving command. A previously managed service immediately degrades to
`Run` when evidence expires; historical proof remains visible but cannot be
mistaken for current protection.

Driver health may be an in-container health check or a bounded, digest-pinned
driver helper probe when the upstream image intentionally contains no shell.
The lifecycle record names which mechanism qualifies. NATS uses the pinned NATS
CLI with generated least-privilege account credentials; absence of that probe
or credential evidence keeps it `Run` rather than creating a health exception.

Tier and recovery envelope are separate. `Managed` is accompanied by
`snapshot`, `pitr`, `cold`, or `replicated`, the observed recovery point/window,
measured drill restore time, and expected interruption. A managed RDB snapshot
does not imply PITR; a managed cold RabbitMQ contract does not imply online
backup; a healthy MinIO replica does not imply protection from deletion unless
versioned recovery has been exercised.

### 6. Qualify all durable drivers through their native recovery model

The shipped driver catalogue is the scope: PostgreSQL, MySQL, MariaDB, MongoDB,
ClickHouse, Redis, Valkey, RabbitMQ, MinIO, Meilisearch, and NATS JetStream.
Their initial contracts are deliberately different:

- **PostgreSQL:** pgBackRest owns encrypted full/differential/incremental base
  backups, S3-compatible repository metadata, WAL archive push/get, retention,
  restore, and PITR. Onebox publishes a reproducible derived image containing
  PostgreSQL and the compatible pgBackRest binary over the exact pinned
  upstream PostgreSQL base digest, then generates the stanza and
  PostgreSQL archive/restore configuration inside that runtime. Enabling
  `archive_mode` is a planned, strongly approved one-time restart. Onebox
  checks WAL continuity and verifies the recovered cluster.
  pgBackRest documents repository encryption, S3-compatible storage, WAL
  archiving, retention, restore, and PITR:
  https://pgbackrest.org/user-guide.html
- **MySQL:** a pinned Percona XtraBackup helper creates and prepares a physical
  backup for separately listed compatible MySQL versions and storage engines.
  The helper mounts the exact service data volume with contract-bound access,
  uses generated credentials and compatible UID/GID ownership, and never scans
  arbitrary Docker volumes.
  A short-lived scheduled archiver forces rotation as needed, uploads closed
  binary logs, and proves a gap-free sequence from the base position. Restore
  prepares an empty exact-compatible volume and replays logs to the requested
  point. Versions outside the XtraBackup matrix may later qualify a separately
  tested MySQL Shell logical snapshot, but never silently fall back during a
  physical or PITR request. Percona documents full/incremental creation and
  independent validation containers:
  https://docs.percona.com/percona-xtrabackup/8.4/docker-compose-tutorial.html
- **MariaDB:** a separately pinned `mariadb-backup` helper creates and prepares
  physical full/incremental generations, records binary-log coordinates, and
  uses a MariaDB-specific closed-binlog archive/replay path. It never inherits
  MySQL qualification merely because both use port 3306. Onebox generates the
  driver-owned `log_bin` configuration and requires an approved one-time
  enablement restart before PITR can qualify; the helper receives only the
  exact data volume, credentials, and ownership mapping. MariaDB documents the
  hot physical backup, prepare, restore, incremental, and binlog-coordinate
  contracts:
  https://mariadb.com/docs/server/server-usage/backup-and-restore/mariadb-backup/
- **MongoDB:** new services become authenticated single-node replica sets with
  idempotent initialization, PRIMARY health, and `replicaSet` connection
  options. Existing standalone volumes require explicit conversion. A pinned
  Percona Backup for MongoDB helper and sidecar create logical base backups,
  archive oplog slices, restore, and perform PITR against user-owned storage.
  Physical PBM backup is not claimed for the official `mongo` image. PBM
  documents replica-set-consistent backup, remote storage, and oplog-based
  PITR: https://docs.percona.com/percona-backup-mongodb/
- **ClickHouse:** the server's native `BACKUP`/`RESTORE` commands write to an
  S3-compatible target through generated named collections, using versioned
  full or incremental generations. Onebox monitors `system.backups`, restores
  into an empty exact-compatible service, and verifies databases, tables,
  parts, counts, and declared queries. The named collection is a digest-bound
  generated service configuration; a required restart uses the one-time
  enablement plan. Qualification records either archive-password or target SSE
  evidence rather than assuming native S3 writes are encrypted. ClickHouse
  documents native S3,
  incremental, asynchronous, and password-protected backup behavior:
  https://clickhouse.com/docs/concepts/features/backup-restore/overview
- **Redis:** qualified Redis versions with native sealed backup support use its
  BASE/INCR/manifest contract and restore with the matching `preload-file`
  mechanism, making the seal boundary the observed recovery point. A separately
  qualified older-version fallback requests an immutable RDB and sends it
  through encrypted artifact storage. That fallback restores in two phases:
  first boot with AOF loading disabled and no prior append directory, verify the
  RDB dataset, then enable AOF and wait for a successful rewrite before the
  restore can be cut over. An existing or empty AOF is never allowed to take
  precedence over the restored RDB. Versions outside either tested matrix stay
  `Run`. Redis documents its persistence behavior at:
  https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/
- **Valkey:** the contract is independently versioned but likewise uses its
  native remote RDB snapshot path and verifies server identity before restore;
  Redis/Valkey file compatibility is never assumed across unlisted versions.
  Restore uses the same explicit AOF-disabled first boot, RDB verification,
  AOF enablement, and completed rewrite gate, adapted and tested against the
  Valkey command/runtime contract.
  Valkey documents immutable RDB backup and `valkey-cli --rdb`:
  https://valkey.io/topics/persistence/
- **Meilisearch:** exact-version native snapshots provide routine disaster
  recovery; portable dumps are created before a separately authorized upgrade
  but do not authorize the upgrade itself. The artifact repository encrypts
  and stores the result, and restore verification checks task completion,
  index/settings/document counts, and queries. Meilisearch distinguishes
  snapshots for periodic recovery from dumps for migration:
  https://www.meilisearch.com/docs/resources/self_hosting/data_backup/overview
- **NATS JetStream:** a pinned NATS CLI creates an account snapshot including
  file-backed streams, stream configuration, durable consumers, state, and
  messages; memory-only streams fail protection preflight. Newly created NATS
  runtimes use generated least-privilege account credentials, and the same
  pinned CLI provides the external driver health probe because the service
  image carries no shell. Existing unauthenticated runtimes remain `Run` until
  a state-bound, strongly approved conversion names the expected connection
  interruption, updates server and projected workload credentials in one
  fenced operation, redeploys dependent workloads, verifies authenticated
  reconnection and stream health, and can roll back both runtime and workload
  projections before the prior credentials are discarded. Restore targets an
  empty isolated server/account and verifies stream and consumer state. NATS
  documents native account and stream backup/restore:
  https://docs.nats.io/running-a-nats-service/nats_admin/jetstream_admin/disaster_recovery
- **RabbitMQ:** a live definitions export protects topology only and never
  graduates the service. Complete single-node protection requires an explicitly
  permitted interruption. The driver pins a stable generated
  `RABBITMQ_NODENAME`, treats it and the Erlang cookie as protected identity,
  and records both identity references in every manifest. The cold path exports
  definitions, stops the exact-version/node-name broker, archives its full data
  directory and required generated identity material, restarts and verifies it,
  then restore-drills that same cold path in
  isolation. RabbitMQ explicitly discourages copying message data from a live
  node and binds disk restore to node identity:
  https://www.rabbitmq.com/docs/backup
- **MinIO:** ordinary volume copying and mirroring are insufficient. The
  qualified contract requires an operator-provisioned second MinIO deployment
  outside the protected host and configures versioned replication to that
  independently operated target. It exports bucket/IAM metadata needed by the
  declared scope, checks lag and failures, and exercises recovery into an
  isolated service. Onebox does not provision the remote deployment and does
  not claim transparent failover. MinIO documents replication as its BC/DR
  primitive and requires versioning and independent targets:
  https://min.io/docs/minio/container/operations/concepts/availability-and-resiliency.html

Each driver and supported version begins `Run`. Its task group ends only after
backup or replication, empty-target restore, isolated restore drill, corrupt or
incomplete artifact, cancellation, runner loss, incompatible version, target
outage, retention safety, and crash-recovery tests pass. PITR additionally
requires an injected replay-gap suite. RabbitMQ additionally requires stopped-
node interruption tests; MinIO additionally requires delete/version recovery,
metadata recovery, independence, and replication-lag tests. Only then may the
capability catalogue and current documentation name that exact envelope
`Managed`. A tested direct-recovery and ownership-handback runbook for the
driver's exact helper/image/repository contract is part of that gate, not
post-graduation documentation cleanup.

### 7. Restore through active-volume indirection

Service state gains a fenced, sealed `active-volume` record containing the
logical service volume, actual Docker volume, selection operation, previous
selection, and epoch. Existing installations are adopted non-destructively by
recording their current stable volume as the initial active selection; no data
is copied or renamed.

Restore creates an operation-named volume, asks the native driver to materialize
the selected snapshot, replay point, cold generation, or replica recovery into
it, starts an isolated Compose project using the immutable service image digest
resolved and recorded when the protected runtime was applied, and recreates
identity prerequisites. A mutable catalogue tag is never restore evidence; the
digest is a pruning root for as long as any manifest or staged restore depends
on it. The temporary service then runs driver verification. `restore_test`
stops there and removes its temporary resources only after evidence is durable;
a failed drill is retained for explicit inspection/cleanup.

Live cutover uses a fresh plan binding the manifest, active-volume record,
service runtime, live container, and repository state. After a strong external
approval, it stops the current service, atomically writes the fenced selection,
regenerates the service runtime against the new actual volume, starts and
verifies it, and records the previous selection. If startup verification fails,
the operation switches back only when the old selection remains verified and
the journal proves no unsafe data-side effect; otherwise it stops and exposes
honest recovery choices. Force cannot delete or bypass either volume.

Retention of pre-restore volumes is independent of remote backup retention.
No automatic task deletes one in this change; a future explicit prune contract
must prove it is not active or the last recovery choice.

Once a protection policy or manifest binds a service identity, its declared
name is immutable. Renaming is refused with
`protected_service_identity_changed` and the affected manifests because
RabbitMQ node identity, volume selection, repository prefixes, and isolated
restore identities derive from it. A future explicit migration contract may
move that identity; remove-and-redeclare is not treated as a rename.

### 8. Make defaults and origins explicit

Backup targets are declared at project scope. A service selects a target and
declares recovery kind, maximum data loss, schedule, retention, maximum
restore-drill age, and whether interruption is permitted. The author cannot
select pgBackRest, Restic, PBM, a helper image, or a command. Environment
overrides may replace schedule and retention only; they cannot change service
identity, driver, recovery kind, interruption permission, persistence owner,
or introduce an undeclared target.

Defaults are additive `onebox.run/v1` values:

- no backup policy by default, therefore `Run`;
- recovery kind has no default: enabling protection requires an explicit
  `snapshot`, `pitr`, `cold`, or `replicated` objective so Onebox never invents
  an RPO or interruption contract;
- daily backup at 02:00 UTC only when protection is explicitly enabled without
  a schedule;
- seven recoverable base generations and at least seven days of continuous
  replay where the selected recovery kind supports it. Each lifecycle record
  maps that intent to native semantics: artifact repositories keep the last
  seven owned generations; pgBackRest derives full/differential/archive counts;
  PBM derives supported count/age rules; ClickHouse retains seven recoverable
  chain heads with all ancestors; snapshot drivers retain seven owned native
  generations; and replicated MinIO proves remote version/lifecycle coverage
  without Onebox deleting replica objects. Planning refuses any mapping that
  cannot preserve the stated minimum and never emulates native retention by
  deleting objects inside a native repository;
- restore proof expires after seven days;
- restore drills run twice weekly by default. Canonicalization derives a stable
  per-service UTC offset inside separate Sunday and Wednesday six-hour windows
  from the protected service identity, so services do not share one start slot.
  Validation includes the full window, host admission delay, and driver time
  budget when proving that the maximum interval remains shorter than the
  restore-proof maximum age;
- generated container logs use the Docker `local` driver with 20 MiB per file
  and five files where no author policy exists;
- disk warning is the stricter of 10% free or 5 GiB; critical is the stricter
  of 5% free or 2 GiB.

Canonical and structured outputs label authored, default, environment
override, observed, and derived values. An authored custom logging driver is
preserved; where Onebox cannot verify its retention, protection status says
`external` rather than silently overriding it.

### 9. Prune only from a proven ownership and reachability graph

Image roots are computed from current and retained releases, service runtime
documents, the proxy, scheduled jobs, helper images, every service-image digest
referenced by a retained protection manifest, and active/in-progress restore
state. Onebox deletes only images carrying its ownership evidence and
unreachable from all roots. It never calls `docker system prune`, never deletes
volumes as housekeeping, and recomputes the graph after cancellation or retry.

Disk checks cover every distinct filesystem backing the app base, Docker data
root, backup staging, and restore/drill staging. The host contract may select a
separate restore/drill staging filesystem. Each driver estimates and caps its
second-copy footprint before a drill. A host-wide admission coordinator holds
an atomic per-filesystem reservation ledger across all apps and services;
materialization begins only when the new reservation plus every active
reservation fits the configured staging budget. Distinct filesystems may admit
drills concurrently, while shared filesystems serialize or defer safely.
Insufficient aggregate headroom records
`drill_deferred_capacity` with the required bytes and resolving commands rather
than misreporting a corrupt backup; if proof later expires, tier still honestly
falls to `Run`. Critical pressure blocks space-increasing mutations but leaves
reads, planning, audit, recovery, and bounded cleanup available.

### 10. Reuse the scheduled runner for continuous assurance

An assurance envelope runs read-only checks for workload/service health, disk,
certificate runway, backup age, restore-proof age, and generated unit state.
Evidence is written atomically with start/finish state and expiry. Notifications
are emitted on state transition and a bounded reminder interval through the
existing webhook contract; the default unchanged-failure reminder is 24 hours
and may be authored explicitly. Webhook failure is recorded separately from
the observed health state.

The check process has no converge methods in its operation graph. Status and
doctor read evidence but never trigger a check. This preserves the invariant
that observation cannot mutate.

### 11. Model external dependencies without provider lifecycle

`external_services` is a separate closed map so ownership cannot be ambiguous.
An entry names a supported driver-shaped connection, a trusted env-file source,
the protection owner label, and an optional read-only health probe. `needs`
resolves both run and external services and uses the existing connection-part
mapping. The generated application runtime receives staged secret references;
no service runtime is generated.

Plans bind redacted connection structure, probe result, and observation age.
Migration policy may require externally generated plan-bound backup evidence.
No provider SDK or provisioning command is introduced.

### 12. Keep CI as orchestration around the CLI

The repository ships a reusable GitHub Actions workflow and an equivalent
documented shell sequence. They use pinned upstream actions, build/push each
selected workload, resolve registry-confirmed digests, and pass `--image`
mappings to `ob plan`. The workflow publishes only redacted structured outputs
and never creates an approval. Deployment runs only when a matching approval
artifact arrives through a separately trusted CI mechanism.

The workflow stores operation identity before invoking deploy. Retry first
inspects that identity and uses resume/status behavior; it never blindly starts
a second deploy. The workflow contains no rendering, diff, drift, lock,
rollback, or recovery implementation.

## Risks / Trade-offs

- [Several native tools become part of the recovery dependency chain] → Keep a
  closed driver/version/helper matrix, pin every digest, record provenance in
  every manifest, test the oldest supported format, and publish direct recovery
  procedures for each qualified contract. Restic receives the same treatment
  only for artifact-producing drivers.
- [Derived PostgreSQL images make Onebox part of the PostgreSQL patch path] →
  Poll supported upstream image digests at least every 24 hours; target a
  qualified derived publication within 72 hours of a supported security patch
  and seven days of another supported patch. Build from exact upstream digests
  in a reproducible official workflow, publish source/base provenance and SBOMs,
  report `protection_image_update_overdue` when the target is missed, verify the
  chosen digest before apply and restore, never auto-upgrade a running protected
  service, and retain every digest referenced by recovery evidence.
- [A scheduled executable appears agent-like] → Keep it short-lived, schema
  restricted, non-listening, least-privileged, and built from the canonical Go
  service; document that no daemon or inbound control plane exists.
- [Physical backup, snapshots, and replay archiving can load the single host] →
  Enforce per-driver CPU, memory, I/O, staging, and throughput budgets; expose
  duration/size evidence; schedule full generations off peak; and refuse when
  disk or copy-on-write headroom is insufficient.
- [A restore drill needs a second dataset copy on one host] → Estimate and cap
  each driver's drill footprint, permit a distinct staging filesystem, report
  capacity deferral separately from restore failure, and keep expiry/tier
  reporting honest until the operator supplies headroom.
- [Restore volume indirection complicates existing stable naming] → Seed the
  pointer from the current volume without renaming, preserve logical names in
  user output, and fault-test every state transition before enabling cutover.
- [MySQL and MariaDB physical compatibility is narrower than their image tag
  range] → Publish an exact server/helper/engine matrix, test it in containers,
  and remain `Run` outside it rather than falling back to an unrequested logical
  method.
- [MongoDB replica-set conversion changes connection and initialization] → New
  services use the qualified form; existing standalone services require an
  explicit state-bound conversion and remain `Run` until it succeeds.
- [PBM introduces a resident database helper] → Treat it as a least-privileged,
  version-bound part of the MongoDB runtime, not a Onebox control daemon; expose
  its health and refuse `Managed` when it is absent or incompatible.
- [RabbitMQ cannot safely copy live message storage] → Require an authored
  interruption window and independently approved stop/start operation; report
  definition-only protection separately and never call it `Managed`.
- [MinIO replication can faithfully replicate deletion or corruption] → Require
  an independent versioned target, record lag and delete/version behavior,
  export required metadata, and test recovery of an older object version.
- [Remote storage outage can create false confidence] → Tier derives from
  freshness and real restore proof, watchdog alerts on expiry, and retention
  never removes old verified snapshots after a failed new backup.
- [The umbrella change is large] → Implement capability foundations first,
  graduate one driver at a time, and keep all unfinished drivers visibly
  `Run`; no task group may update current docs before its evidence gate passes.

## Migration Plan

1. Add schema fields, canonical origins, output-only tiers, operation schemas,
   and inert status reporting while all services remain `Run`.
2. Add active-volume state seeded from existing service volumes, with read-only
   validation and no restore cutover enabled.
3. Add the scheduled runner with bidirectional compatibility and removal,
   service/helper image publication and provenance, S3-compatible target
   contract, direct-native and Restic-artifact repository interfaces, the
   non-graduating test driver, protection manifests, manual backup/list/status,
   and fault tests.
4. Add isolated restore and restore drills; then add approved live cutover and
   crash recovery.
5. Qualify the derived PostgreSQL/pgBackRest image, approved archive-mode
   enablement, base/WAL/PITR, and update current docs only after its full
   evidence gate. Repeat independently for MySQL XtraBackup plus binlogs and
   MariaDB Backup plus generated binlog configuration and approved enablement.
6. Add MongoDB single-node replica-set creation, explicit standalone conversion,
   PBM base backups, and oplog PITR, then qualify MongoDB protection.
7. Qualify ClickHouse native backup and encryption, Redis sealed native backup
   or its separately gated RDB fallback, Valkey's safe two-phase RDB restore,
   Meilisearch snapshot, and authenticated NATS account snapshot/health
   independently.
8. Add RabbitMQ's explicit cold contract and MinIO's independent versioned
   replication contract, keeping each `Run` until its special evidence passes.
9. Add log rotation, disk gates, image reachability pruning, assurance timers,
   and webhook transitions.
10. Add external-service connections and the official CI workflow.
11. Run the full conformance, fault, local-Docker E2E, schema parity, redaction,
   and strict OpenSpec suites. Rollback of the software keeps new schema fields
   inert to older binaries only where minimum-runner policy permits; it never
   deletes backup repositories, active or previous volumes, or generated
   evidence.

Ownership handback is explicit: disabling schedules removes generated units but
leaves repositories, replicas, and volumes; inspection prints redacted
manifests and effective commands; driver-specific direct-recovery runbooks let
the operator restore without Onebox, including Restic only where it stores the
artifact. Deleting remote backups or previous restore volumes and dismantling a
MinIO replica require future explicit destructive contracts and are not part
of rollback.

## Open Questions

- The exact OCI publication location for `ob-scheduled-runner`, derived service
  images, and qualified helper images can be selected during packaging without
  changing their pinned provenance or behavioral contracts.
