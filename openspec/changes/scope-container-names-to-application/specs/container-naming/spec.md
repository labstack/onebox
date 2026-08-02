## Purpose

Defines how Onebox derives the names of the containers it runs, so that several
applications can share one host without colliding on a namespace the container
runtime keeps global.

## ADDED Requirements

### Requirement: Container names are unique across applications on a host

A container name SHALL be derived from the application identifier together with
the component identifier, and SHALL NOT be derived from the component alone.
Two applications on one host declaring components with the same name SHALL
derive different container names.

#### Scenario: Two applications declare the same component name
- **WHEN** two applications on one host each declare a component named `server`
- **THEN** their derived container names differ and both deployments complete

#### Scenario: Single replica
- **WHEN** a component runs one replica
- **THEN** its container name still carries the application identifier

#### Scenario: Multiple replicas
- **WHEN** a component runs more than one replica
- **THEN** each replica's container name carries the application identifier and a distinct slot index

### Requirement: Derivation is deterministic and stable across releases

The same application and component SHALL derive the same container name on every
release, so that a rollout, a rollback, and an observation of the same
installation agree on which container is which.

#### Scenario: Names are stable across releases
- **WHEN** two consecutive releases of one application are deployed
- **THEN** each component's derived container names are identical between them

#### Scenario: Rollback agrees with the rollout
- **WHEN** a release is rolled back
- **THEN** the names derived for the restored release match those the rollout used

### Requirement: Every derived name is application-scoped, including transient ones

The rollout assigns a temporary name to a new container before promoting it to a
stable slot. That transient name SHALL be application-scoped like every other,
and SHALL be included in the pre-mutation collision check. A transient name that
is not scoped is the same host-global collision as a stable one, arriving in the
middle of a rollout when two applications deploy at once.

An authored fixed container name SHALL NOT be honoured. Onebox owns container
naming, so a name declared in a referenced Compose file is refused rather than
preserved; preserving it would reintroduce exactly the collision this contract
removes.

#### Scenario: Transient name collides across applications
- **WHEN** two applications deploy components with the same name at the same time
- **THEN** their transient container names differ

#### Scenario: Transient name is checked before mutation
- **WHEN** a foreign container holds the transient name a rollout would use
- **THEN** the deployment fails before any container is started, stopped, or renamed

#### Scenario: Authored fixed name is refused
- **WHEN** a referenced Compose service declares a fixed container name
- **THEN** the deployment fails naming the key, and the name is not preserved

### Requirement: An over-long derived name is refused, not truncated

A derived container name exceeding sixty-three characters SHALL be refused during
validation, naming the application identifier, the component identifier, and the
limit. Sixty-three is an Onebox limit chosen for headroom rather than a
documented container-runtime maximum, and is the same number the declarative
schema change applies to every other derived name. It SHALL NOT be truncated, because truncation reintroduces the
collision this contract exists to remove.

#### Scenario: Derived name exceeds the limit
- **WHEN** an application and component identifier together exceed the runtime's name limit
- **THEN** validation fails, names both identifiers and the limit, and no deployment is attempted

### Requirement: A foreign holder is detected before mutation

Before renaming any container, Onebox SHALL detect an existing container holding
a derived name that Onebox does not own, determined by its labels, and SHALL fail
naming the holder. It SHALL NOT rename, stop, or remove a container it does not
own, and SHALL NOT fail partway through a rollout on a name conflict that was
observable beforehand.

#### Scenario: Foreign container holds a derived name
- **WHEN** a container Onebox does not own already holds a name a deployment would derive
- **THEN** the deployment fails before any container is started, stopped, or renamed, and the error names the holder

#### Scenario: Onebox's own container holds the name
- **WHEN** the container holding a derived name belongs to this application's previous release
- **THEN** the rollout proceeds and renames it as part of the normal slot handover
