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

### Requirement: An over-long derived name is refused, not truncated

A derived container name exceeding the container runtime's limit SHALL be refused
during validation, naming the application identifier, the component identifier,
and the limit. It SHALL NOT be truncated, because truncation reintroduces the
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
