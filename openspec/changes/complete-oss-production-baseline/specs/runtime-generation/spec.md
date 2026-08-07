## RENAMED Requirements

- FROM: `Service declarations produce no runtime and reserve their names`
- TO: `Service declarations produce driver-owned runtime and reserve their names`

## MODIFIED Requirements

### Requirement: Service declarations produce driver-owned runtime and reserve their names

Generation SHALL emit a separate, stable runtime document for each supported
Onebox-run service and SHALL keep it outside application releases. The document
SHALL resolve the driver's versioned image to an immutable registry digest at
apply, render that digest, and retain it in service state. It SHALL contain the
durable volumes, resource limits, qualified in-container or external-helper
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
- **WHEN** a versioned service image is applied
- **THEN** the generated runtime and service state use the registry-confirmed immutable digest and retain it as a restore and pruning root

#### Scenario: Protected service is renamed
- **WHEN** a plan changes the declared name of a service bound by a protection policy or manifest
- **THEN** preflight fails with `protected_service_identity_changed` and identifies the evidence that would be orphaned

## ADDED Requirements

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
strongly approved conversion updates server and workload credentials together
and verifies reconnection.

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
- **THEN** it refuses automatic credential enablement and requires an approved conversion plan that preserves the prior runtime until authenticated server and workload connectivity verify

### Requirement: Protection artifacts are generated, bound, and inspectable

For a qualified backup policy, generation SHALL produce secret-referential
backup and restore inputs, host schedule units, retention policy, protection
state paths, isolated restore-runtime templates, and any driver-owned archive
hook or helper sidecar required by the selected recovery objective. Every
helper, hook, configuration, and executable artifact SHALL be version-bound and
digest-bound into the operation plan, carry verifiable publication provenance,
and be printable in redacted form before
mutation. A generated unit or sidecar SHALL invoke the canonical protection
contract or the driver's bounded native protocol rather than embed a second
Onebox lifecycle implementation. The scheduled runner and envelope SHALL
declare mutually compatible protocol ranges; incompatibility in either
direction SHALL refuse before mutation.

#### Scenario: Backup schedule is previewed
- **WHEN** a protected service runtime is previewed
- **THEN** output includes the redacted generated schedule, target reference, retention, helper provenance, and artifact digest

#### Scenario: Generated artifact drifts
- **WHEN** a target-side protection unit differs from the plan-bound artifact
- **THEN** execution fails before mutation with a typed drift error and directs the operator to re-plan or apply the protection artifact

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
and approval. The previous selection SHALL remain recorded as the rollback
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
authorized convergence; remote backups, application
data, service volumes, and foreign units SHALL remain untouched.

#### Scenario: Protection is disabled
- **WHEN** an authorized change removes a service backup schedule
- **THEN** the corresponding generated timer is removed and existing remote backups and service volumes remain

#### Scenario: Application is destroyed
- **WHEN** an authorized destroy removes the last reference to a scheduled runner or protection envelope
- **THEN** Onebox removes those owned executable artifacts while preserving remote repositories, replicas, handback manifests, and service data according to the destroy contract
