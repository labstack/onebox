## Purpose

Defines trustworthy, driver-native protection for Onebox-run data services, including encrypted off-host backups, verified restores, restore drills, and evidence-based service-tier graduation.

## ADDED Requirements

### Requirement: Backup policy names executable protection

A backup policy SHALL select a declared backup target, an exact host schedule,
a retention policy, an exact restore-drill schedule, a restore-drill maximum
age, a recovery objective, and any permitted recurring backup interruption
window. Onebox SHALL accept the policy only when the
installed runner has an executable driver contract for that service version,
recovery objective, target kind, and runtime prerequisites. The author SHALL
declare the recovery outcome and maximum tolerable data-loss window, not a
backup executable. Declaration alone SHALL NOT change a service's tier.
When a restore-drill schedule is defaulted, Onebox SHALL derive stable
per-service start times from canonical application identity, environment,
declared service name, and driver inside the documented windows. It SHALL
include the full window, host admission delay, and driver time budget in
proof-expiry validation.

#### Scenario: Qualified policy loads
- **GIVEN** a driver supported by the installed backup runner and a declared target
- **WHEN** the service selects that target and a valid schedule and retention policy
- **THEN** validation succeeds and canonical output shows every effective value and its origin

#### Scenario: Unsupported driver fails closed
- **GIVEN** a service driver with no qualified protection contract
- **WHEN** the service declares a backup policy
- **THEN** validation fails with code `backup_driver_unsupported` and directs the operator to remove the policy or choose a qualified driver

#### Scenario: Unsupported recovery objective fails closed
- **GIVEN** a driver version qualified for snapshot recovery but not point-in-time recovery
- **WHEN** its policy requires point-in-time recovery
- **THEN** validation fails with code `recovery_objective_unsupported` and reports the strongest executable objective without silently weakening the declaration

#### Scenario: Required interruption is not authorized
- **GIVEN** a driver whose complete backup contract requires a stopped-service window
- **WHEN** its policy does not explicitly permit that interruption
- **THEN** validation fails with code `backup_interruption_not_authorized` and the service remains `Run`

#### Scenario: Drill schedule cannot preserve proof
- **GIVEN** a restore-drill schedule, jitter budget, and driver time budget whose maximum interval reaches or exceeds the restore-proof maximum age
- **WHEN** the protection policy is validated
- **THEN** validation fails with code `restore_drill_schedule_too_sparse` and names an acceptable maximum cadence

#### Scenario: Default drill schedules are spread
- **GIVEN** multiple protected services use the default restore-drill schedule
- **WHEN** canonical schedules are generated
- **THEN** each receives a stable service-specific start time within the documented windows and the maximum proof interval remains explicit

### Requirement: Restart-bound protection enablement is separately authorized

When a qualified contract requires a restart-bound runtime prerequisite,
Onebox SHALL produce a one-time state-bound enablement plan naming the exact
configuration delta, expected interruption, rollback action, and post-restart
health verification. It SHALL NOT apply that delta or restart the service from
ordinary policy convergence, a recurring interruption window, or a scheduled
operation. Execution SHALL require a fresh strong approval delivered
independently of model-authored text. The enablement record SHALL prove how the
prerequisite was established but SHALL NOT serve as continuing proof. Backup
preflight, status, doctor, and assurance SHALL re-observe the effective runtime
prerequisite and configuration identity. A missing or drifted prerequisite
SHALL block dependent backup operations and `Managed` graduation.

#### Scenario: Enablement restart lacks approval
- **GIVEN** PostgreSQL archive mode, MariaDB binary logging, or ClickHouse backup configuration requires a service restart
- **WHEN** apply has no fresh approval bound to the enablement plan and live service state
- **THEN** it refuses with code `protection_enablement_restart_not_authorized` without changing configuration or restarting the service

#### Scenario: Enablement restart succeeds
- **GIVEN** a fresh strongly approved enablement plan and unchanged live state
- **WHEN** Onebox installs the driver-owned configuration and restarts the service
- **THEN** it verifies service health and the effective prerequisite before recording enablement provenance

#### Scenario: Effective prerequisite later drifts
- **GIVEN** protection enablement previously succeeded but the effective archive, binary-log, named-collection, or equivalent prerequisite is now absent or changed
- **WHEN** backup preflight, status, doctor, or assurance observes the service
- **THEN** it reports `protection_prerequisite_drifted`, blocks dependent backup work, and reports the service `Run` until a newly approved enablement restores and verifies the prerequisite

### Requirement: Protection disablement preserves service safety and recovery assets

Removing a protection policy SHALL NOT itself remove runtime support or revert
an image while an installed prerequisite remains effective. Onebox SHALL emit a
state-bound disablement plan naming interruption, prerequisite reversal,
rollback, verification, unit and image transitions, and remote-data handback.
A restart-bound reversal SHALL require a fresh strong approval delivered
independently of model-authored text. Until verification succeeds, Onebox SHALL
retain the recorded service image and every hook, credential, configuration,
and unit required to keep the engine safe. Disablement SHALL NOT delete remote
backups, replicas, manifests, previous volumes, or manifest-referenced images.

#### Scenario: Disablement restart lacks approval
- **GIVEN** an enabled protection prerequisite requires restart-bound reversal
- **WHEN** the policy is removed without a fresh approval bound to the disablement plan and live state
- **THEN** execution refuses with `protection_disablement_not_authorized`, keeps the safe runtime effective, and reports `disable-pending`

#### Scenario: PostgreSQL protection is disabled safely
- **GIVEN** PostgreSQL archive mode and an archive command are effective in the derived service image
- **WHEN** an approved disablement executes
- **THEN** it first disables and verifies archive mode and WAL recycling while retaining the derived image, and only afterward may remove archive support or return the live runtime to ordinary tag rendering

#### Scenario: Disablement crashes between phases
- **GIVEN** prerequisite reversal verified but live image reversion did not complete
- **WHEN** the runner crashes or disconnects
- **THEN** durable phase state resumes safely with the derived image still installed and all remote recovery assets preserved

### Requirement: Backup creation is consistent, encrypted, and retry-safe

Onebox SHALL create a backup through the selected driver's native consistency
method and SHALL satisfy the capability record's declared encryption mode for
the selected recovery kind: client-side encryption, archive-password
encryption, observed server-side encryption, or inherited replica encryption.
The protection manifest and status SHALL identify the active mode and its
secret-free evidence. Onebox SHALL NOT report encrypted protection when the
driver or destination cannot prove the declared mode. When
the native method can stream, Onebox SHALL NOT create a plaintext intermediate.
When the native method requires a local artifact, Onebox SHALL use bounded,
mode-restricted Onebox staging, SHALL exclude it from protection evidence, and
SHALL remove it after verified upload or report its exact residual identity and
cleanup command after failure. The manifest SHALL bind the application,
environment, service, driver, service version, recovery kind, consistency
method, target, protected resources, replay or replica range where applicable,
encryption mode, artifact digests only when artifacts exist, and creation time.
A replicated contract SHALL identify replica state and metadata scope without
inventing a backup generation or artifact. The operation SHALL serialize against service apply,
migration, restore, and incompatible backup operations. Retrying the same
operation identity SHALL resume or return the existing terminal result and
SHALL NOT create an untracked duplicate.

#### Scenario: Successful backup
- **GIVEN** a healthy qualified service and reachable target
- **WHEN** an operator or installed timer requests a backup
- **THEN** Onebox records a verified encrypted snapshot and a secret-free manifest before reporting success

#### Scenario: Stream interruption
- **GIVEN** a backup upload in progress
- **WHEN** the runner disconnects or the operation is cancelled
- **THEN** the incomplete upload is not eligible for restore or retention decisions and the journal identifies the safe retry command

#### Scenario: Concurrent data mutation
- **GIVEN** a migration, service apply, restore, or incompatible backup holds the service protection lock
- **WHEN** another backup starts
- **THEN** it waits within the declared budget or fails with code `backup_conflict` without reading or mutating data

#### Scenario: Secrets are redacted
- **GIVEN** storage credentials and database credentials are used during backup
- **WHEN** plans, events, logs, journals, manifests, errors, or structured output are produced
- **THEN** no plaintext credential or database content appears in those surfaces

#### Scenario: Native tool requires local output
- **GIVEN** a qualified native backup method that cannot stream its artifact
- **WHEN** backup creation succeeds or fails after producing local bytes
- **THEN** Onebox never counts those bytes as off-host protection and either removes them after verified upload or reports the restricted residual and explicit cleanup command

#### Scenario: Native destination encryption is unproven
- **GIVEN** a native-direct or replicated contract whose required encryption mode cannot be observed at the destination
- **WHEN** backup or protection status is evaluated
- **THEN** Onebox records `backup_encryption_unverified`, does not claim encrypted protection, and keeps the service `Run`

### Requirement: Retention never weakens the last known protection

Retention SHALL operate only on verified backup generations and replay logs
owned by the exact target, driver contract, and service. Onebox SHALL invoke
the qualified driver's declared mapping from authored minimum generations and
recovery window to repository-aware native retention semantics, SHALL preserve
every dependency needed to satisfy the declared recovery window, and SHALL
apply destructive retention only after a new recoverable generation verifies.
It SHALL refuse deletion when ownership, repository state, replay continuity,
the native mapping, or the newest verified generation is ambiguous. Onebox
SHALL NOT emulate retention by directly deleting objects inside a native
repository or replica. Removing a project or policy
SHALL NOT delete remote backup bytes.

#### Scenario: New backup fails
- **GIVEN** older verified snapshots exist
- **WHEN** a new backup or its integrity verification fails
- **THEN** retention deletes none of the older snapshots

#### Scenario: Foreign repository content
- **GIVEN** the repository contains content not attributable to the service manifest
- **WHEN** retention runs
- **THEN** Onebox leaves that content untouched and reports it as outside its ownership

#### Scenario: Replay chain has a gap
- **GIVEN** a point-in-time policy whose archived log sequence is incomplete
- **WHEN** retention evaluates base backups or replay logs
- **THEN** it deletes nothing needed by the last continuous recovery window and reports code `replay_continuity_broken`

#### Scenario: Retention intent cannot map to native semantics
- **GIVEN** an authored minimum generation count or recovery window the selected driver cannot preserve through its native retention controls
- **WHEN** protection is planned
- **THEN** planning fails with code `backup_retention_unsupported` and performs no repository deletion

### Requirement: Restore is staged before live cutover

A restore SHALL first materialize the selected backup into a new isolated
volume, start an exact-compatible temporary service, and run driver integrity
verification. Live cutover SHALL require a non-expired state-bound plan and a
strong approval delivered independently of model-authored text. Onebox SHALL
stop the current service only after staged verification succeeds, SHALL retain
the pre-restore volume as a rollback point, and SHALL never reinterpret force
as permission to destroy either copy.

#### Scenario: Restore verification succeeds
- **GIVEN** a compatible verified backup and sufficient target capacity
- **WHEN** a restore is prepared
- **THEN** the restored temporary service passes driver verification before a cutover can be approved

#### Scenario: Stale live state
- **GIVEN** a restore plan bound to the current service and volume state
- **WHEN** that state changes before execution
- **THEN** execution fails with code `restore_state_stale` and directs the operator to re-plan

#### Scenario: Verification fails
- **GIVEN** backup bytes were restored into the isolated volume
- **WHEN** the temporary service or integrity checks fail
- **THEN** the live service and its volume remain unchanged and the failed restore is retained or cleaned only through an explicit resolving command

#### Scenario: Runner crashes during cutover
- **GIVEN** a verified staged restore and an authorized cutover
- **WHEN** the runner crashes after stopping or switching a service
- **THEN** durable phase markers let `ob resume` or `ob abort` choose only recovery paths that preserve both the old and restored volumes

### Requirement: Restore drills produce fresh proof

A restore drill SHALL execute the same materialize, start, and driver-verify
path that the qualified recovery-kind contract uses for a real restore without
changing the live service; no applicable step may be substituted or skipped.
Artifact contracts therefore exercise decrypt and download, native-direct
contracts exercise their server-side restore, and replicated contracts recover
from the independent replica plus declared metadata.
Its evidence SHALL identify the selected artifact, native recovery point, or
replica observation as applicable, plus the runner, exact service image digest,
validation method, result, and completion time. Merely checking object
existence or a repository checksum SHALL NOT count as a restore drill.

#### Scenario: Scheduled restore drill passes
- **GIVEN** a service requires a restore drill within a maximum age
- **WHEN** the host schedule restores and verifies a retained backup in isolation
- **THEN** status records fresh passing restore proof for that service

#### Scenario: Drill evidence becomes stale
- **GIVEN** the last passing restore drill exceeds its maximum age
- **WHEN** status, doctor, or the watchdog evaluates protection
- **THEN** the service is reported as not currently managed and the output names `ob restore test` as the resolving command

#### Scenario: Drill is deferred for capacity
- **GIVEN** the selected driver has declared its bounded drill footprint and neither the default nor authored staging filesystem has sufficient headroom
- **WHEN** a scheduled restore drill begins
- **THEN** it records `drill_deferred_capacity` with required bytes and safe capacity remedies, does not classify the backup as corrupt, and does not extend prior proof expiry

### Requirement: Recovery claims follow the native service envelope

Onebox SHALL publish the qualified recovery kind, supported service versions,
expected backup interruption, measured recovery point, measured restore time,
and unmet prerequisites for every built-in durable driver. PostgreSQL SHALL
qualify point-in-time recovery only with a verified physical base and continuous
WAL replay. MySQL and MariaDB SHALL qualify point-in-time recovery only with a
verified physical base and continuous binary-log replay under their separate
version and storage-engine contracts. MongoDB SHALL qualify online snapshot or
point-in-time recovery only as a healthy replica set with continuous oplog
evidence. ClickHouse SHALL use its consistent native backup and restore path.
Redis SHALL use a separately qualified sealed native BASE/INCR/manifest path or
an explicitly qualified immutable-RDB fallback whose restore prevents AOF from
taking precedence. Valkey SHALL use its separately qualified immutable RDB
snapshot path with the same AOF-precedence safety property. Meilisearch SHALL
use its native snapshot path; portable dumps are upgrade evidence only and
SHALL NOT satisfy backup-tier evidence. NATS JetStream SHALL protect stream and
consumer state through authenticated native stream or account snapshots and a
qualified external helper health probe. RabbitMQ SHALL NOT claim
message recovery from a live data-directory copy or a definitions-only export.
MinIO SHALL NOT claim protection unless its replica is independent, versioned,
complete for the declared metadata scope, and recovery-tested. No driver SHALL
fall back to a generic live-volume archive.

#### Scenario: Point-in-time claim has continuous evidence
- **GIVEN** a relational or MongoDB policy requiring point-in-time recovery
- **WHEN** status reports the effective recovery window
- **THEN** the base generation, first and last replay positions, continuity result, and expiry are identified without exposing data or credentials

#### Scenario: Redis-compatible snapshot is verified
- **GIVEN** a Redis or Valkey service with a snapshot policy
- **WHEN** a backup is created and restore-tested
- **THEN** evidence identifies the sealed Redis backup set or immutable RDB as applicable, driver identity, AOF-safe load procedure, key-count verification, and the snapshot recovery point

#### Scenario: RabbitMQ definitions are insufficient
- **GIVEN** a RabbitMQ service for which only definitions were exported
- **WHEN** protection status is evaluated
- **THEN** topology protection is reported separately, message recovery remains unproven, and the service remains `Run`

#### Scenario: MinIO target shares the failure domain
- **GIVEN** a MinIO service whose proposed replica resolves to the same host, data volume, or storage deployment
- **WHEN** its protection policy is planned
- **THEN** planning fails with code `backup_target_not_independent` and never reports replicated protection

### Requirement: Service tier follows observed evidence

A service SHALL report `Managed` only while its service runtime uses the exact
immutable image digest recorded at apply and retained by its protection
manifests, resource policy is effective, driver health is verified through the
qualified in-container or digest-pinned external probe, declared protection
objective is currently satisfied, backup or replication schedule is installed, the
latest recoverable point is within policy, and restore-drill proof is fresh. A
qualified driver missing any evidence SHALL report `Run` with the missing
evidence. Graduation SHALL be independent for PostgreSQL, MySQL, MariaDB,
MongoDB, ClickHouse, Redis, Valkey, RabbitMQ, MinIO, Meilisearch, and NATS; one
driver's passing suite SHALL NOT qualify another driver or version. `Managed`
SHALL always be accompanied by the effective recovery kind and observed RPO so
the tier cannot imply point-in-time or zero-data-loss behavior it does not have.
An overdue derived-image rebuild SHALL be reported as separate security
maintenance and SHALL NOT demote otherwise valid recovery evidence; `Managed`
SHALL NOT be presented as a claim that the service base image is currently
patched.

#### Scenario: All graduation evidence is current
- **GIVEN** every required protection and runtime check passes for a qualified driver
- **WHEN** service status is rendered
- **THEN** the service reports `Managed` and identifies the evidence without exposing secrets

#### Scenario: Service image tag moved
- **GIVEN** a catalogue tag now resolves differently from the immutable service digest recorded for the protected runtime
- **WHEN** service status or restore compatibility is evaluated
- **THEN** Onebox uses and reports the recorded digest, refuses an unavailable digest with `service_image_digest_unavailable`, and never treats the mutable tag as equivalent evidence

#### Scenario: Derived image rebuild is overdue
- **GIVEN** current backup, replay, health, and restore-proof evidence passes but the derived-image publication target was missed
- **WHEN** service status is rendered
- **THEN** it reports `Managed` for the recovery contract plus `protection_image_update_overdue` as separate security maintenance without claiming patch currency

#### Scenario: Backup is stale
- **GIVEN** a previously managed service whose latest verified backup exceeds policy
- **WHEN** status is read
- **THEN** the service reports `Run`, code `backup_stale`, and the command that creates a fresh backup

#### Scenario: MongoDB is standalone
- **GIVEN** a MongoDB service without replica-set evidence
- **WHEN** protection status is evaluated
- **THEN** it remains `Run` and online oplog-consistent protection is unavailable

#### Scenario: External driver health probe is unavailable
- **GIVEN** a driver such as NATS whose qualified health mechanism uses a pinned external helper
- **WHEN** helper provenance, credentials, or a passing bounded probe is absent
- **THEN** the service remains `Run` and status identifies the missing health evidence

#### Scenario: Managed snapshot does not imply point-in-time recovery
- **GIVEN** a managed service qualified only for snapshot recovery
- **WHEN** service status is rendered
- **THEN** it reports `Managed`, recovery kind `snapshot`, and its observed recovery point without presenting a continuous recovery window

### Requirement: Protection CLI is agent-operable

Protection enable/disable, backup, restore, restore-test, list, inspect, and status commands SHALL provide
versioned JSON and NDJSON contracts. Mutations SHALL stream ordered events and
a terminal result; failures SHALL carry stable codes, secret-free messages,
operation identifiers, and resolving next commands. Read commands SHALL NOT
create, repair, prune, or converge protection state.

#### Scenario: Agent retries after disconnection
- **GIVEN** an agent lost the output stream for a backup or restore operation
- **WHEN** it retries with the same operation identifier or inspects that identifier
- **THEN** it receives the existing state or safely resumes rather than starting an unrelated mutation

#### Scenario: Structured failure
- **GIVEN** a backup target is unreachable
- **WHEN** structured output is requested
- **THEN** the terminal record carries code `backup_target_unreachable`, no secret material, and the read-only command that diagnoses the target
