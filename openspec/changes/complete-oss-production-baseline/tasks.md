## 1. Schema and public contracts

- [ ] 1.1 Add `onebox.run/v1` models for S3-compatible storage and independent MinIO replication targets, failure-domain identity, encryption policy, trusted credential references, recovery kind, maximum data loss, permitted interruption, schedules, retention, restore-proof age, and environment-safe overrides; verify with named schema fixtures for valid intent, inline-secret refusal, self-target refusal, authored-tool refusal, unsupported recovery objectives, and target-identity override refusal.
- [ ] 1.2 Add the closed `external_services` schema with driver-shaped connections, trusted connection sources, protection owners, optional read-only probes, and run/external name exclusivity; verify with `external_service_valid`, `external_service_ambiguous_owner`, and `external_service_lifecycle_field_refused` fixtures.
- [ ] 1.3 Extend canonical output to show authored, default, environment-override, observed, and derived origins for protection, hygiene, service tier, recovery kind, observed RPO/window, expected interruption, and prerequisites without secret values; add golden tests covering each origin and redaction.
- [ ] 1.4 Define versioned JSON/NDJSON records for backup, replay archive, replication, restore, restore-test, housekeeping, assurance, and service-tier status, including operation identity, stable error code, terminal state, native evidence identity, recovery envelope, and resolving commands; add encoding compatibility and unknown-field tests.
- [ ] 1.5 Add stable validation and runtime error codes named by the delta specs, and test that every new failure serializes a secret-free message plus a safe resolving command.
- [ ] 1.6 Update the checked-in JSON Schema and schema parity/conformance corpus from the Go model, then run the focused schema-generation, parity, canonicalization, and redaction test suites.

## 2. Driver lifecycle foundation

- [ ] 2.1 Add a lifecycle-capability interface and per-driver capability record beside the existing runtime driver catalogue, with no default backup implementation; test that unqualified drivers remain runnable but reject protection policies with `backup_driver_unsupported`.
- [ ] 2.2 Model recovery kinds, pinned helper provenance, supported service-version ranges, native/direct versus artifact repository ownership, consistency and topology preconditions, expected interruption, achievable RPO, credential slots, protected resources, replay/replication operations, restore operations, verification operations, and graduation evidence; add catalogue validation tests for missing or contradictory fields across all eleven durable drivers.
- [ ] 2.3 Add `backup_create`, `backup_prune`, `replay_archive`, `replication_check`, `restore_test`, `restore_prepare`, `restore_cutover`, `restore_abort`, `hygiene_run`, and `assurance_check` to the canonical operation graph and structured event registry; test schema dispatch, restricted archive sub-envelopes, and unsupported-runner refusal.
- [ ] 2.4 Implement the service protection lock beneath the application lock so service apply, migration, backup, restore, and credential-affecting work serialize; test bounded contention, cancellation, stale fencing, and `backup_conflict` behavior.
- [ ] 2.5 Extend operation journals with deterministic protection step identities, incomplete-resource records, retry classification, helper provenance, and terminal results; test retry after stream disconnect and lookup after client output loss.
- [ ] 2.6 Route all new credentials through trusted target-side files and secret slots; run focused tests proving plans, events, journals, errors, manifests, notifications, and structured output redact credentials and database content.

## 3. Active-volume state and runtime generation

- [ ] 3.1 Define the sealed, fenced active-volume record containing logical name, selected Docker volume, selection operation, previous selection, and epoch; add encode/decode, tamper, missing-state, and stale-epoch tests.
- [ ] 3.2 Seed existing installations by recording the current stable service volume without copying, renaming, recreating, or adopting foreign resources; add migration tests for fresh, existing, missing, and colliding volumes.
- [ ] 3.3 Render service Compose from the active-volume selection while preserving stable logical names and app rollback behavior; add golden and local-Docker tests proving application rollback never changes the service volume.
- [ ] 3.4 Reserve generated service, restore, timer, project, network, volume, and state names during preflight; test that foreign collisions fail closed and are neither adopted nor mutated.
- [ ] 3.5 Generate redacted protection inputs, schedule units, archive hooks, driver-owned helper sidecars, state paths, retention policy, restore templates, helper provenance, and artifact digests, then bind them into executable plans; add drift tests for every generated artifact class and prove snapshot-only drivers receive no replay helper.
- [ ] 3.6 Add inspection and authorized removal for Onebox-owned protection units while preserving remote backups, live data, previous volumes, and foreign units; verify with disable and destroy regression tests.

## 4. Canonical scheduled runner

- [ ] 4.1 Build the short-lived `ob-scheduled-runner` from the same canonical Go service used by the CLI, with no listener and an allowlist containing only scheduled operation schemas; add command-surface tests proving it cannot plan deployments or mint approvals.
- [ ] 4.2 Define and seal the scheduled-operation envelope with schema version, app/environment identity, operation kind, timing policy, artifact digests, state binding, and secret-file references; test tamper, unsupported-version, and stale-envelope refusal.
- [ ] 4.3 Install the runner and envelope with digest verification and least-privilege permissions under the Onebox host layout; add target-layout tests for upgrade, replay, ownership, and mode enforcement.
- [ ] 4.4 Generate, inspect, enable, disable, and remove exact systemd service/timer units for base backup, replay archive, replication checks, restore drill, housekeeping, and assurance; verify reboot persistence and idempotent convergence in the systemd integration suite.
- [ ] 4.5 Make scheduled executions use the canonical lock, fence, journal, retry identity, helper-provenance, cancellation, and redaction paths; add crash, cancellation, lock-contention, and disconnected-client fault tests.

## 5. Repository, manifest, and backup core

- [ ] 5.1 Implement the closed S3-compatible target adapter for endpoint, bucket, prefix, region, TLS policy, encryption policy, failure-domain identity, and trusted mode-0600 credential file, plus a test-only local repository that explicitly reports non-off-host protection; add validation, self-target, alias, and target-probe tests.
- [ ] 5.2 Define repository interfaces for native-direct engines, encrypted Restic artifact storage, and independent MinIO replication without forcing a common repository format; add contract tests for listing, identity, health, retry, retention ownership, and read-only inspection.
- [ ] 5.3 Package and verify the pinned Restic artifact helper, enforce its repository/runner compatibility, and prove only artifact-producing driver contracts can select it; add incompatible-helper, digest-mismatch, and accidental-database-engine selection tests.
- [ ] 5.4 Implement streaming artifact ingestion without a plaintext intermediate and restricted operation-scoped staging for native APIs that require files; test successful cleanup, residual reporting, capacity bounds, cancellation, target loss, helper crash, and retry/resume.
- [ ] 5.5 Persist the sealed secret-free manifest with app, environment, service, driver, service image, recovery kind, native method/helper, target, protected resources, native repository identity, base/replay or replica range, digest, size, interruption, operation, and timestamps; add tamper, cross-service ownership, and replay-range tests.
- [ ] 5.6 Implement manual `backup create`, `backup list`, `backup inspect`, replay/replication inspection, and protection `status` through the canonical CLI with versioned JSON/NDJSON; add golden tests for human output, structured output, idempotent retry, and read-only behavior.
- [ ] 5.7 Implement driver-repository-aware retention only over verified generations and replay dependencies owned by the exact service and target, and only after a newly recoverable generation exists; test failed-new-backup preservation, replay gaps, minimum generations, foreign content, ambiguity refusal, and policy removal without remote deletion.
- [ ] 5.8 Add target diagnostics and stable classifications for unreachable, unauthorized, non-independent, locked, corrupt, incomplete, incompatible, replay-gap, replication-lag, encryption, and capacity failures; verify each result is secret-free and names a read-only diagnosis or recovery command.

## 6. Restore, cutover, and restore drills

- [ ] 6.1 Implement recovery-point selection and compatibility validation bound to manifest, native repository state, base/replay or replica continuity, driver, service image, active-volume state, interruption contract, and available capacity; add stale, corrupt, discontinuous, wrong-service, incompatible-version, and insufficient-disk tests.
- [ ] 6.2 Implement `restore prepare` to create an operation-named isolated volume, materialize the selected snapshot, PITR point, cold generation, or replica recovery through the driver contract, start an exact-image temporary service, and run integrity verification without changing live selection; add local-Docker and cancellation tests.
- [ ] 6.3 Persist staged-restore evidence and cleanup state so failed or incomplete temporary resources remain inspectable and require an explicit resolving command; test runner crash and safe repeated cleanup.
- [ ] 6.4 Implement `restore test` through the same download, decrypt, restore, startup, and verification path as live restore, recording evidence identity, runner, version, method, result, timestamps, and expiry; test that repository checksum-only evidence cannot satisfy the contract.
- [ ] 6.5 Generate a state-bound cutover plan and require a fresh strong approval delivered independently of model-authored text; test absent, expired, mismatched, and stale-state approval refusal.
- [ ] 6.6 Implement restore cutover with durable phase markers: stop live service, commit fenced selection, render/start/verify the restored service, and retain the former volume as the previous recovery choice; add injected-crash tests at every phase boundary.
- [ ] 6.7 Implement `ob resume` and `ob abort` recovery choices that preserve both volumes and never treat force as deletion permission; test safe rollback when possible and honest manual-recovery stop when safety cannot be proven.
- [ ] 6.8 Implement backup, replay archive, replication, and restore structured event streams and terminal records, then test retry after connection loss returns or resumes the same operation instead of starting an unrelated mutation.

## 7. PostgreSQL qualification gate

- [ ] 7.1 Implement the exact PostgreSQL/pgBackRest compatibility matrix and pinned helper/configuration generation, including repository encryption, S3-compatible settings, least-privilege credentials, resource limits, and refusal outside listed versions; add catalogue, rendering, and helper-provenance tests.
- [ ] 7.2 Implement pgBackRest stanza creation/check, full/differential/incremental backup, WAL archive push/get integration, continuity evidence, retention, and recovery-point listing; add native repository tests for gaps, duplicates, target loss, and archive-command failure.
- [ ] 7.3 Implement isolated pgBackRest restore and PITR to an exact-compatible PostgreSQL service, verifying startup, timeline, target recovery point, catalogue readability, object counts, and declared checks; add successful, corrupt base, missing WAL, wrong timeline, and incompatible-version tests.
- [ ] 7.4 Run the PostgreSQL qualification matrix for base backup, WAL archiving, PITR boundaries, isolated restore, restore drill, live cutover, corrupt artifact, cancellation, runner loss, target outage, retention safety, and injected crash recovery in local-Docker E2E.
- [ ] 7.5 Only after task 7.4 passes, enable PostgreSQL `Managed`/`pitr` graduation for the exact tested matrix and add evidence tests for current, stale, discontinuous, failed, missing, and recovered protection.
- [ ] 7.6 Only after task 7.5 passes, document qualified PostgreSQL versions, measured recovery envelope, direct pgBackRest recovery, and all configurations that remain `Run`.

## 8. MySQL qualification gate

- [ ] 8.1 Implement the exact MySQL/Percona XtraBackup/server-edition/storage-engine compatibility matrix with pinned helper, privileges, lock/precondition checks, and resource limits; add tests for supported versions, mixed engines, incompatible editions, unreadable metadata, and active conflicts.
- [ ] 8.2 Implement full/incremental physical backup, prepare, artifact encryption/upload, base-position capture, closed-binary-log rotation/archive, continuity tracking, and retention; add helper integration tests for broken chains, partial streams, duplicate logs, and target loss.
- [ ] 8.3 Implement empty exact-compatible MySQL restore plus binary-log replay to a selected point, verifying startup, server identity, replay position, table/catalogue readability, counts, and declared checks; add successful, corrupt, gap, and cross-version refusal tests.
- [ ] 8.4 Run the MySQL qualification matrix for physical backup, binlog archive, PITR boundaries, isolated restore, restore drill, live cutover, corrupt artifact, cancellation, runner loss, target outage, retention safety, and crash recovery in local-Docker E2E.
- [ ] 8.5 Only after task 8.4 passes, enable MySQL `Managed`/`pitr` graduation for the exact tested envelope and add tier-evidence tests; leave every other MySQL version or engine `Run` without logical fallback.
- [ ] 8.6 Only after task 8.5 passes, document the qualified MySQL matrix, observed RPO, direct recovery dependencies, and unsupported configurations.

## 9. MariaDB qualification gate

- [ ] 9.1 Implement the separate MariaDB/`mariadb-backup`/storage-engine compatibility matrix with pinned helper, privileges, backup-stage/precondition checks, and resource limits rather than inheriting MySQL qualification; add version, engine, helper-mismatch, and active-condition tests.
- [ ] 9.2 Implement full/incremental physical backup, prepare, artifact encryption/upload, binlog-coordinate capture, closed-binlog archive, continuity tracking, and retention; add native helper tests for broken increments, partial streams, binlog gaps, and target loss.
- [ ] 9.3 Implement empty exact-compatible MariaDB restore plus binlog replay to a selected point, verifying startup, server identity, replay position, catalogues, counts, and declared checks; add successful, corrupt, gap, and cross-version refusal tests.
- [ ] 9.4 Run the MariaDB qualification matrix for physical backup, binlog archive, PITR boundaries, isolated restore, restore drill, live cutover, corrupt artifact, cancellation, runner loss, target outage, retention safety, and crash recovery in local-Docker E2E.
- [ ] 9.5 Only after task 9.4 passes, enable MariaDB `Managed`/`pitr` graduation for the exact tested envelope and add tier-evidence tests independently of MySQL.
- [ ] 9.6 Only after task 9.5 passes, document qualified MariaDB versions, observed RPO, direct recovery dependencies, and configurations that remain `Run`.

## 10. MongoDB replica-set runtime and qualification gate

- [ ] 10.1 Change newly created MongoDB services to a single-node replica set with generated internal keyfile, idempotent initialization, PRIMARY health, durable config, and `replicaSet` connection projection; add fresh-install, restart, retry, and redaction E2E tests.
- [ ] 10.2 Add an explicit state-bound conversion plan for existing standalone MongoDB volumes that preserves the original data and refuses automatic or ambiguous adoption; add successful conversion, stale plan, failed startup, and recovery tests.
- [ ] 10.3 Implement the MongoDB/PBM compatibility matrix and least-privilege pinned PBM helper sidecar, refusing protection when replica-set, PRIMARY, helper-health, or storage evidence is absent; add standalone, election, incompatible-tool, sidecar-loss, and healthy-replica-set tests.
- [ ] 10.4 Implement PBM logical base backup, oplog slicing, S3-compatible storage, continuity/status translation, retention, and recovery-point listing; add gaps, target loss, interrupted backup, election, and stale-sidecar integration tests.
- [ ] 10.5 Implement isolated PBM restore and PITR to an exact-compatible MongoDB replica set, verifying startup, PRIMARY, target timestamp, catalogues, collection/index counts, users/roles scope, and declared checks; add successful, corrupt-base, missing-oplog, and incompatible-version tests.
- [ ] 10.6 Run the MongoDB qualification matrix for base backup, oplog PITR boundaries, isolated restore, restore drill, live cutover, standalone refusal, corruption, cancellation, runner/sidecar loss, election, target outage, retention safety, and crash recovery in local-Docker E2E.
- [ ] 10.7 Only after task 10.6 passes, enable and document MongoDB `Managed`/`pitr` for the exact single-node replica-set envelope; keep standalone, incompatible, and unhealthy-helper configurations `Run`.

## 11. ClickHouse qualification gate

- [ ] 11.1 Implement the ClickHouse native-backup version matrix and generated named S3 collection with secret references, encryption/password policy, privileges, concurrency limits, and resource budgets; add rendering, query-log redaction, and unsupported-version tests.
- [ ] 11.2 Implement native full/incremental `BACKUP` orchestration, asynchronous status translation from `system.backups`, manifest binding, base-chain retention, and cancellation/retry reconciliation; add successful, failed, duplicate-ID, missing-base, and target-outage tests.
- [ ] 11.3 Implement native `RESTORE` into an empty exact-compatible ClickHouse service and verify databases, tables, parts, counts, access scope, and declared queries; add corrupt, wrong-password, incomplete-chain, and incompatible-version tests.
- [ ] 11.4 Run the ClickHouse qualification matrix for full/incremental backup, isolated restore, restore drill, live cutover, cancellation, runner loss, target outage, retention safety, and crash recovery in local-Docker E2E.
- [ ] 11.5 Only after task 11.4 passes, enable and document ClickHouse `Managed`/`snapshot` for the exact tested envelope and leave other versions/configurations `Run`.

## 12. Redis qualification gate

- [ ] 12.1 Implement the Redis version/persistence compatibility matrix and snapshot preflight for memory and copy-on-write headroom, module compatibility, AOF/RDB state, and authentication; add supported, insufficient-memory, busy-persistence, and module tests.
- [ ] 12.2 Request and wait for a complete immutable RDB generation, ingest it through encrypted artifact storage with generated runtime configuration, and record server identity, persistence state, key counts, digest, and snapshot time; add partial-RDB, target-loss, cancellation, and retry tests.
- [ ] 12.3 Restore the RDB into an empty exact-compatible Redis service and verify load completion, server identity, databases, key counts, TTL samples, and declared checks; add corrupt-RDB, incompatible-version, and module-mismatch tests.
- [ ] 12.4 Run the Redis qualification matrix for snapshot backup, isolated restore, restore drill, live cutover, memory pressure, cancellation, runner loss, target outage, retention safety, and crash recovery in local-Docker E2E.
- [ ] 12.5 Only after task 12.4 passes, enable and document Redis `Managed`/`snapshot`, its measured RPO and fork-memory requirement, while making clear that AOF durability is not PITR evidence.

## 13. Valkey qualification gate

- [ ] 13.1 Implement a Valkey-specific version/persistence compatibility matrix and remote-RDB preflight without inheriting Redis file compatibility outside explicitly tested versions; add server-identity, memory, busy-persistence, and cross-product refusal tests.
- [ ] 13.2 Create and ingest a complete immutable Valkey RDB through encrypted artifact storage, recording persistence state, key counts, digest, and snapshot time; add partial-transfer, target-loss, cancellation, and retry tests.
- [ ] 13.3 Restore into an empty exact-compatible Valkey service and verify load completion, server identity, databases, key counts, TTL samples, and declared checks; add corrupt-RDB, Redis-produced-RDB refusal, and incompatible-version tests.
- [ ] 13.4 Run the Valkey qualification matrix for snapshot backup, isolated restore, restore drill, live cutover, memory pressure, cancellation, runner loss, target outage, retention safety, and crash recovery in local-Docker E2E.
- [ ] 13.5 Only after task 13.4 passes, enable and document Valkey `Managed`/`snapshot` for the exact tested matrix, independently of Redis.

## 14. RabbitMQ cold qualification gate

- [ ] 14.1 Implement RabbitMQ version, node-name, Erlang-cookie, storage, and queue-type discovery plus separate topology-only and complete-cold capability reporting; add tests proving definitions-only evidence never qualifies message recovery.
- [ ] 14.2 Implement live definitions export and verification as non-graduating topology evidence; add users/vhosts/policies/queues/exchanges/bindings round-trip tests with credential redaction.
- [ ] 14.3 Implement a strongly approved stopped-node backup that drains/stops the broker, archives the complete data directory and required generated identity material through encrypted artifact storage, restarts and verifies the live broker, and records interruption; add stop failure, upload failure, restart failure, and abort tests.
- [ ] 14.4 Restore the cold generation into an empty exact-version, exact-node-name isolated broker and verify definitions, queue types, durable messages, streams, and declared checks; add live-copy refusal, corrupt-store, wrong-cookie, wrong-node-name, and incompatible-version tests.
- [ ] 14.5 Run the RabbitMQ qualification matrix for cold backup, isolated restore, restore drill, approved interruption, cancellation, runner loss, target outage, retention safety, and crash recovery; only then enable and document `Managed`/`cold` with measured downtime, leaving topology-only protection `Run`.

## 15. MinIO replication qualification gate

- [ ] 15.1 Implement MinIO source/target identity, version, TLS, versioning, object-lock/encryption, capacity, and failure-domain preflight, refusing the same deployment, host, data volume, or backup prefix; add alias, DNS, credential, and `backup_target_not_independent` tests.
- [ ] 15.2 Generate and apply authorized bucket/site replication configuration only to a pre-existing user-owned remote MinIO deployment, export declared bucket/IAM metadata scope, and preserve secrets in target files; add drift, partial-configuration, and no-remote-provisioning tests.
- [ ] 15.3 Observe replication queue, lag, failures, versions, deletes, metadata scope, and resynchronization as evidence without claiming transparent failover; add stale, incomplete, delete-marker, object-lock, KMS mismatch, and remote-outage tests.
- [ ] 15.4 Exercise recovery into an isolated MinIO service from the replica plus exported metadata, verifying current and older object versions, bucket configuration, declared IAM scope, checksums, and application reads; add deleted-object, corrupt-object, missing-metadata, and incompatible-version tests.
- [ ] 15.5 Run the MinIO qualification matrix for replication setup, lag, outage/resync, version/delete recovery, isolated drill, cancellation, runner loss, target replacement, and crash recovery; only then enable and document `Managed`/`replicated`, leaving mirrors and same-domain targets `Run`.

## 16. Meilisearch qualification gate

- [ ] 16.1 Implement the Meilisearch version matrix and native snapshot task/status integration with artifact staging, encryption, resource limits, and master-key secret references; add supported-version, task-failure, capacity, and redaction tests.
- [ ] 16.2 Create exact-version periodic snapshots and separate portable pre-upgrade dumps, manifesting their different compatibility and purpose; add task retry, partial artifact, target-loss, and snapshot-versus-dump classification tests.
- [ ] 16.3 Restore into an empty compatible Meilisearch service and verify task completion, indexes, settings, synonyms, document counts, and declared searches; add corrupt, newer-dump-to-older-version, wrong-key, and incompatible-snapshot tests.
- [ ] 16.4 Run the Meilisearch qualification matrix for snapshot backup, isolated restore, restore drill, live cutover, cancellation, runner loss, target outage, retention safety, and crash recovery in local-Docker E2E.
- [ ] 16.5 Only after task 16.4 passes, enable and document Meilisearch `Managed`/`snapshot`; keep portable dumps as upgrade evidence rather than backup-tier evidence.

## 17. NATS JetStream qualification gate

- [ ] 17.1 Implement the NATS version, account, file-backed stream, credentials, and consumer-state compatibility matrix, refusing memory-only streams with an exact resolving command; add mixed-storage, permissions, and unsupported-version tests.
- [ ] 17.2 Use the pinned NATS CLI to create checked account backups containing stream configuration/state, durable consumers, and messages, then ingest them through encrypted artifact storage; add partial stream, critical-warning, target-loss, cancellation, and retry tests.
- [ ] 17.3 Restore into an empty exact-compatible NATS service/account and verify streams, subjects, message counts/content samples, durable consumer configuration/state, and declared checks; add existing-stream, corrupt-snapshot, and incompatible-version tests.
- [ ] 17.4 Run the NATS qualification matrix for account backup, isolated restore, restore drill, live cutover, cancellation, runner loss, target outage, retention safety, and crash recovery in local-Docker E2E.
- [ ] 17.5 Only after task 17.4 passes, enable and document NATS JetStream `Managed`/`snapshot` for file-backed streams while memory-only or incomplete accounts remain `Run`.

## 18. Host operational hygiene

- [ ] 18.1 Apply the documented Docker `local` logging defaults of 20 MiB and five files to every generated workload, service, helper, and proxy container while preserving authored policies; add rendering and effective-origin golden tests.
- [ ] 18.2 Verify effective runtime log policies and report unsupported custom drivers as externally managed instead of protected; add status/doctor tests for default, authored, drifted, and unverifiable policies.
- [ ] 18.3 Build the image ownership and reachability graph from current and retained releases, services, proxy, scheduled jobs, helpers, and active/incomplete restore state; add graph tests proving every rollback and recovery root is retained.
- [ ] 18.4 Implement scoped pruning of only proven Onebox-owned unreachable images, recomputing reachability after cancellation or retry and never calling system-wide prune; add foreign-image, retained-release, interrupted-prune, and idempotency E2E tests.
- [ ] 18.5 Probe headroom for each distinct filesystem backing the app base, Docker data root, backup staging, and restore staging, applying explicit default or authored warning/critical thresholds; add boundary, multi-filesystem, and unavailable-probe tests.
- [ ] 18.6 Gate space-increasing deploy, backup, and restore operations on critical pressure while keeping reads, cleanup planning, backup listing, and recovery available; test `disk_pressure_critical` and every allowed escape path.
- [ ] 18.7 Implement housekeeping plan/run/status and its host schedule through the canonical runner, with versioned structured reclaimed-byte, deleted-image, and preserved-root results; add reboot, disable, retry, cancellation, and redaction tests.

## 19. Local continuous assurance

- [ ] 19.1 Implement the read-only assurance check graph for workloads, run services, external probes, disk, certificate runway, backup freshness, replay continuity, replication lag, restore-proof freshness, and generated units/helpers; add a capability test proving no converge or repair operation is reachable.
- [ ] 19.2 Persist assurance evidence atomically with start, completion, check set, runner provenance, observations, expiry, and per-check state; add tests distinguishing current, stale, incomplete, never-run, and failed evidence.
- [ ] 19.3 Install assurance schedules through the canonical runner and reconcile interrupted checks as fresh reads on the next occurrence; add reboot, cancellation, runner-upgrade, and partial-check fault tests.
- [ ] 19.4 Add transition and bounded-reminder notifications through the existing webhook contract, with notification delivery state separate from observed health; test transition deduplication, reminder cadence, webhook failure, and secret redaction.
- [ ] 19.5 Extend status and doctor with versioned assurance records, stable check codes, evidence identifiers, expiry, and resolving commands while keeping reads side-effect free; add human/JSON golden tests and mutation guards.

## 20. External-service connections

- [ ] 20.1 Normalize external-service declarations into canonical driver-shaped connection metadata and the derived `External` tier; add canonicalization and run/external ownership-conflict tests.
- [ ] 20.2 Resolve workload `needs` entries across run and external services, projecting only requested parts from trusted staged secret files and generating no external service runtime; add runtime golden and plaintext-leak tests.
- [ ] 20.3 Implement optional read-only external health probes with bounded age and plan binding; add healthy, permission-denied, unreachable, changed-after-plan, and `external_service_state_stale` tests proving no corrective mutation occurs.
- [ ] 20.4 Refuse apply, backup, restore, upgrade, rotation, and destroy against external dependencies with `external_service_not_owned`, preserving the declared owner in secret-free output; add command matrix tests.
- [ ] 20.5 Validate fresh plan-bound external backup evidence or an independently authorized override for migration policy without creating, storing, restoring, or claiming provider backups; add stale, mismatched, missing, and accepted receipt tests.

## 21. Official CI image delivery

- [ ] 21.1 Add a reusable GitHub Actions workflow using pinned upstream actions to build selected build-sourced workloads, push them, confirm registry digests, and emit the versioned workload-to-digest result; validate the workflow with multi-workload and digest-resolution failure fixtures.
- [ ] 21.2 Make the workflow call only shipped CLI validation, planning, approval validation, deploy, status, and resume commands, and add a repository check that rejects duplicated render, drift, lock, fencing, rollback, or recovery logic in workflow scripts.
- [ ] 21.3 Separate plan-only output from deployment so a missing independently delivered matching approval always stops before deploy and the workflow never mints approval; add absent, mismatched, expired, and accepted approval tests.
- [ ] 21.4 Persist operation identity before deployment, inspect it after runner loss, and reuse a plan only while its state binding remains valid; add retry fixtures for in-progress, terminal, expired-plan, and changed-state cases.
- [ ] 21.5 Audit CI artifacts and logs so only redacted validation, digest mappings, plans, and structured results are uploaded while registry, SSH, backup, application, and approval-delivery secrets remain in the CI secret facility; add automated secret-canary tests.
- [ ] 21.6 Publish an equivalent documented shell sequence that performs the same build/push/digest/plan/approved-deploy handoff without becoming a second deployment implementation; verify every documented command in the CLI docs test harness.

## 22. Integration, documentation, and release evidence

- [ ] 22.1 Add an end-to-end service-tier suite proving `Run`, `Managed`, and `External` plus `snapshot`, `pitr`, `cold`, and `replicated` recovery envelopes are derived from observed evidence; cover stale backup, replay gaps, replication lag, stale drill, unauthorized interruption, unhealthy helpers, unit drift, unpinned images, missing resources, and recovery to current evidence.
- [ ] 22.2 Add a cross-capability fault suite covering disconnect, retry, cancellation, stale plans, partial evidence, corrupt state, disk pressure, lock contention, runner/helper crash, archive gaps, replica outage, and target outage without data deletion or secret disclosure.
- [ ] 22.3 Document target setup, failure-domain independence, recovery objectives, schedules, retention, manual backup, replay/replication inspection, restore test, approved live restore, abort/resume, tier versus recovery envelope, host schedules, housekeeping, assurance, external ownership, and CI handoff in README, product, CLI, and schema references.
- [ ] 22.4 Publish driver-specific direct recovery and ownership-handback runbooks for pgBackRest, XtraBackup, MariaDB Backup, PBM, ClickHouse, Redis, Valkey, RabbitMQ, MinIO, Meilisearch, NATS, and Restic artifact repositories, explicitly covering Onebox absence, generated-unit/helper removal, repository/replica preservation, previous-volume preservation, and unsupported destructive cleanup.
- [ ] 22.5 Ensure documentation names a driver/version/recovery envelope as `Managed` only after its independent qualification gate has passed; add a consistency test comparing published claims with the runtime capability catalogue.
- [ ] 22.6 Run formatting, static analysis, unit, conformance, redaction, systemd, native-helper, local-Docker E2E, schema parity, workflow validation, and documentation command suites, recording exact commands and passing evidence in the change.
- [ ] 22.7 Run strict OpenSpec validation and resolve every diagnostic; confirm no proposed behavior is described as currently shipped and no remote backup, replica, service volume, previous volume, or foreign resource gains an implicit deletion path before requesting archive review.
