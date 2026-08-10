## RENAMED Requirements

- FROM: `Service declarations produce no runtime and reserve their names`
- TO: `Service declarations produce driver-owned runtime and reserve their names`

## MODIFIED Requirements

### Requirement: Service declarations produce driver-owned runtime and reserve their names

Generation SHALL emit a separate, stable runtime document for each supported
Onebox-run service and SHALL keep it outside application releases. A service
that has never enabled protection, or whose locally confirmed disablement completed,
SHALL retain the existing version-tag image behavior and SHALL NOT require
protection-driven registry digest resolution. Removing a policy from an enabled
service SHALL move durable runtime state to `disable-pending`; it SHALL NOT by
itself change the image, configuration, hooks, credentials, or units required by
the still-effective engine prerequisites. Before managed
protection can be enabled, Onebox SHALL map the declared service selector to a
qualified immutable service image digest, bind the mapping into a state-bound
plan, render that digest after authorized convergence, and retain it in service
state. A derived mapping SHALL identify the exact upstream patch/base digest,
compatible native helper version, derived digest, publication time, and support
state. Protection enablement for an existing service SHALL use a qualified
derived image over its observed current upstream base and SHALL NOT introduce a
patch upgrade. When that base predates the published mapping window, Onebox
SHALL require a separate state-bound, same-major service patch plan before
protection. A new protected service SHALL use the latest qualified mapping for its
selector. Onebox SHALL NOT silently substitute an unpublished or unqualified
image. During registry outage Onebox SHALL accept a previously recorded,
locally present image for apply or restore only when its digest verifies
exactly. The document SHALL contain the durable volumes, resource limits,
qualified in-container or external-helper
health semantics, shared network, and references to target-generated
credentials without containing credential values. Application deployment and
rollback SHALL NOT recreate, stop, or remove those service volumes.

Every derived service container, project, network, volume, restore volume,
timer, and state-file name SHALL be reserved during preflight. A foreign
resource holding a reserved name SHALL be refused rather than adopted.
External-service declarations SHALL generate no container, volume, or network.
Once a protection policy or manifest binds a service identity, changing its
declared name SHALL be refused rather than treated as remove-and-recreate.

#### Scenario: Run service produces separate runtime
- **WHEN** runtime is generated for a project declaring a supported service
- **THEN** inspection includes a separate service artifact whose digest binds into the plan and whose credential values remain absent

#### Scenario: Application rollback preserves service
- **WHEN** an application release is rolled back
- **THEN** the service project and active service volume remain unchanged

#### Scenario: Service name is already occupied
- **WHEN** a foreign resource holds a container, project, network, volume, restore-volume, timer, or state name the service derives
- **THEN** preflight fails and names the collision without adopting or mutating it

#### Scenario: External service produces no runtime
- **WHEN** runtime is generated for a declared external service
- **THEN** no service container, volume, network, or lifecycle unit is emitted

#### Scenario: Service image resolves to a digest
- **WHEN** managed protection is enabled for a versioned service image
- **THEN** the generated runtime and service state use the registry-confirmed immutable digest and retain it as a restore and pruning root

#### Scenario: Unprotected service remains tag-based
- **WHEN** a service that never enabled protection or completed locally confirmed disablement is applied while its registry is unreachable
- **THEN** generation preserves the existing version-tag runtime behavior and introduces no protection-driven registry failure

#### Scenario: Protection image mapping is unpublished
- **WHEN** a service enables protection but its declared selector has no qualified published service-image mapping and no previously recorded verified digest
- **THEN** planning refuses with `protection_service_image_unpublished`, leaves the runtime unchanged, and keeps the service `Run`

#### Scenario: Existing service has only a newer derived mapping
- **WHEN** an existing service enables protection but the only published derived mapping would change its observed upstream patch/base digest
- **THEN** planning refuses with `protection_service_patch_required` and names a separate same-major `ob service apply --refresh-image` patch plan rather than upgrading as a side effect of protection enablement

#### Scenario: Pre-protection same-major patch succeeds
- **GIVEN** an existing service is not protected and a qualified patch exists in its declared major
- **WHEN** its locally confirmed `ob service apply --refresh-image` plan executes
- **THEN** Onebox preserves its volume and rollback identity, applies the exact qualified upstream digest, restarts and verifies the service, leaves protection disabled, and names the subsequent protection command

#### Scenario: Service patch would cross a major version
- **WHEN** a refresh-image plan would change the declared service major
- **THEN** planning refuses with `service_major_upgrade_unsupported` without changing the image, volume, or protection state

#### Scenario: Recorded protection image is cached
- **WHEN** an existing protected service is applied or restored while the registry is unreachable and its recorded digest is present locally
- **THEN** Onebox verifies and uses the exact local digest without resolving the mutable tag

#### Scenario: Derived image publication is overdue
- **WHEN** a supported upstream patch exceeds the documented derived-publication target without a qualified mapping
- **THEN** status reports `protection_image_update_overdue`, the affected selector and upstream version, and the current recorded digest without silently upgrading the service

#### Scenario: Protected service patch is available
- **WHEN** an enabled service has a qualified exact non-major current-to-candidate transition for its delivery class and driver
- **THEN** status retains its evidence-derived tier, reports `protection_service_patch_available`, and names `ob service apply --refresh-image` without changing the live image

#### Scenario: Image refresh is requested during disablement
- **WHEN** `ob service apply --refresh-image` targets a `disable-pending` service
- **THEN** planning refuses with `service_image_patch_disable_pending`, reports the pending age and deadline, and names the exact disablement command without changing the image or schedules

#### Scenario: Protection policy is removed while prerequisites remain active
- **WHEN** an apply removes the policy from a service with installed protection prerequisites
- **THEN** the service enters `disable-pending`, retains its recorded digest and working hooks, reports `Run`, and requires a state-bound, locally confirmed disablement before ordinary tag rendering

#### Scenario: Image reversion would strand an archive command
- **WHEN** a plan would replace the recorded service image while an effective archive command still depends on a binary present only in that image
- **THEN** planning refuses with `protection_image_revert_unsafe` without changing the live runtime

#### Scenario: Protected service is renamed
- **WHEN** a plan changes the declared name of a service bound by a protection policy or manifest
- **THEN** preflight fails with `protected_service_identity_changed` and identifies the evidence that would be orphaned

## ADDED Requirements

### Requirement: Protected service image maintenance is driver-qualified and recovery-safe

Refreshing an enabled service image in any service delivery class SHALL use a
state-bound, locally confirmed, driver-qualified non-major `service_image_patch` plan rather
than disable and re-enable protection. Onebox SHALL offer the plan only when the
driver lifecycle contract publishes the exact current-to-candidate service and
any applicable helper digest transition plus driver-specific compatibility and continuity
checks. There SHALL be no default protected patch behavior. The plan SHALL bind
the live volume and configuration identity, a fresh pre-patch recovery point,
and the driver continuity marker. Across the service restart it SHALL keep every
effective prerequisite, credential, hook, and schedule unchanged. It SHALL
verify service health, the driver compatibility matrix, effective protection
configuration, repository readability, and driver-native continuity
before committing the candidate digest. It SHALL retain every previous digest
referenced by a protection manifest and SHALL roll back safely or stop with
explicit recovery choices when verification fails.

#### Scenario: Protected upstream-digest patch succeeds
- **GIVEN** an upstream-digest service has a qualified exact non-major transition with compatible helper and continuity checks
- **WHEN** its locally confirmed refresh-image plan executes
- **THEN** the service restarts on the candidate digest, protection remains effective, driver-native continuity verifies, and prior manifest image roots remain retained

#### Scenario: Protected transition is not qualified
- **WHEN** an enabled service has no published exact current-to-candidate transition for its delivery class and driver
- **THEN** planning refuses with `protected_service_patch_unsupported`, identifies the current service and applicable helper digests, and leaves the runtime and tier evidence unchanged

#### Scenario: Protected same-major patch succeeds
- **GIVEN** PostgreSQL protection is enabled and a compatible qualified same-major derived image is available
- **WHEN** the locally confirmed refresh-image plan executes
- **THEN** PostgreSQL restarts once on the new digest with archive mode still effective, WAL continuity and repository readability verify, protection remains enabled, and manifest roots retain the prior digest

#### Scenario: Protected patch compatibility is unproven
- **WHEN** the new pgBackRest cannot prove compatibility with the recorded stanza or repository
- **THEN** planning refuses with `protected_service_patch_incompatible` before changing the image or interrupting the service

### Requirement: Protection prerequisites that alter service runtime are explicit

Driver-owned runtime prerequisites SHALL be generated, planned, and verified
as part of the service contract. Newly created MongoDB services SHALL use a
generated internal-auth keyfile, a single-node replica set initialized
idempotently, PRIMARY health, and a `replicaSet` connection projection.
Existing standalone MongoDB data SHALL NOT be converted automatically or
ambiguously. RabbitMQ SHALL use a stable generated node name recorded as
protected identity. NATS SHALL use generated least-privilege account
credentials and a digest-pinned external CLI health probe when the upstream
service image cannot provide an in-container check. Existing unauthenticated
NATS runtimes SHALL remain unchanged and `Run` until a separate state-bound,
locally confirmed conversion declares expected connection interruption, updates
server and workload credentials together, redeploys dependent workloads,
verifies authenticated reconnection and stream health, and preserves a bounded
rollback to the prior server/runtime projection until verification passes.

#### Scenario: New MongoDB runtime is initialized
- **WHEN** a newly declared MongoDB service is first applied or safely retried
- **THEN** one authenticated single-node replica set is initialized idempotently, health requires PRIMARY, and projected connections include the replica-set identity without exposing the keyfile

#### Scenario: Existing MongoDB volume is standalone
- **WHEN** protection planning finds existing standalone MongoDB data
- **THEN** it refuses automatic conversion and requires a separate state-bound conversion plan that preserves the original volume and rollback choice

#### Scenario: RabbitMQ identity is generated
- **WHEN** a RabbitMQ service runtime is applied
- **THEN** its stable generated node name and Erlang-cookie references are recorded as protected resources and reused by isolated restore

#### Scenario: NATS helper health is generated
- **WHEN** a NATS runtime is applied
- **THEN** generated account credentials and the pinned external NATS CLI probe provide bounded driver health without adding shell utilities to the service image

#### Scenario: Existing NATS runtime is unauthenticated
- **WHEN** protection planning finds an existing unauthenticated NATS service
- **THEN** it refuses automatic credential enablement and requires a locally confirmed conversion plan naming expected interruption, affected workloads, rollback, and post-change authenticated health before the prior runtime can be discarded

### Requirement: Protection artifacts are generated, bound, and inspectable

For a qualified backup policy, generation SHALL produce secret-referential
backup and restore inputs, host schedule units, retention policy, protection
state paths, isolated restore-runtime templates, and any driver-owned archive
hook or helper sidecar required by the selected recovery objective. Every
helper, hook, configuration, and executable artifact SHALL be version-bound and
digest-bound into the operation plan, carry verifiable publication provenance,
and be printable in redacted form before mutation. A generated unit or sidecar
SHALL invoke the canonical protection
contract or the driver's bounded native protocol rather than embed a second
Onebox lifecycle implementation. The scheduled runner and envelope SHALL
declare mutually compatible protocol ranges; incompatibility in either
direction SHALL refuse before mutation.
The plan-bound desired artifact set SHALL derive from current project intent
plus durable protection lifecycle state. While a service is `disable-pending`,
the recorded last-effective target, retention, image, configuration, hooks,
credentials, and required schedules SHALL remain in that desired set; their
absence from current project text SHALL NOT by itself be drift. A target-side
difference from that durable retained projection SHALL still fail closed as
real drift.

#### Scenario: Backup schedule is previewed
- **WHEN** a protected service runtime is previewed
- **THEN** output includes the redacted generated schedule, target reference, retention, helper provenance, and artifact digest

#### Scenario: Generated artifact drifts
- **WHEN** a target-side protection unit differs from the plan-bound artifact
- **THEN** execution fails before mutation with a typed drift error and directs the operator to re-plan or apply the protection artifact

#### Scenario: Pending-disable artifact is intentionally retained
- **GIVEN** durable lifecycle state records a `disable-pending` retained artifact projection
- **WHEN** an unrelated apply observes matching image, hook, configuration, and required schedule artifacts that are absent from current project text
- **THEN** it preserves them, does not report drift, and allows unrelated plan steps to continue

#### Scenario: Native archive hook is generated
- **WHEN** a qualified point-in-time driver requires database-driven log archiving or a driver-owned helper process
- **THEN** inspection shows its redacted configuration, supported version, privileges, target reference, schema, and digest without exposing archive credentials

#### Scenario: Snapshot-only driver emits no replay helper
- **WHEN** a qualified driver provides snapshot recovery without a native replay log
- **THEN** generation emits only the snapshot and restore artifacts and never synthesizes a continuous-log process

#### Scenario: Runner and envelope are incompatible
- **WHEN** either an installed runner rejects the envelope protocol or the CLI rejects the installed runner protocol
- **THEN** execution fails before mutation with a typed compatibility error and names the matching upgrade or apply command

### Requirement: Restore volume selection is durable and reversible

The active volume selected for a service SHALL be recorded in fenced Onebox
state rather than inferred from whichever volume happens to exist. A restore
SHALL create a uniquely named staging volume, bind it to its backup and
operation identity, and change the active selection only after verification
and local confirmation. The previous selection SHALL remain recorded as the rollback
choice until an explicit retention operation makes it ineligible.

#### Scenario: Restore is staged
- **WHEN** verified backup bytes are restored
- **THEN** they populate a new operation-bound volume while the active selection still names the live volume

#### Scenario: Cutover completes
- **WHEN** an authorized verified restore cuts over
- **THEN** fenced state selects the restored volume and retains the prior selection as a recovery choice

#### Scenario: Crash before state commit
- **WHEN** the runner crashes before the active selection is durably committed
- **THEN** recovery treats the former live volume as active and does not infer cutover from the staging volume's presence

### Requirement: Generated host units have explicit ownership and removal

Housekeeping, assurance, backup, and restore-drill units SHALL reside under the
Onebox-owned host layout, carry application/environment labels, and be listed
by inspection. Removing or disabling a declaration SHALL remove only its
generated units, envelopes, and unreferenced runner artifacts after an
authorized convergence and, where applicable, successful prerequisite
disablement. Units or hooks required by an effective engine prerequisite SHALL
remain until that prerequisite is verified absent. Remote backups, manifests,
manifest-referenced images, application data, service volumes, and foreign
units SHALL remain untouched.

#### Scenario: Protection is disabled
- **WHEN** an authorized disablement has reverted and verified every installed prerequisite
- **THEN** the corresponding generated timer, hook, and unreferenced live-runtime artifact may be removed while remote backups, manifests, restore-image digests, and service volumes remain

#### Scenario: Application is destroyed
- **WHEN** an authorized destroy removes the last reference to a scheduled runner or protection envelope
- **THEN** Onebox removes those owned executable artifacts while preserving remote repositories, handback manifests, and service data according to the destroy contract
