## Purpose

Defines how a project opts a service into Onebox ownership while preserving deterministic configuration, safe defaults, explicit secrets, and an unrestricted Compose-owned alternative.

## ADDED Requirements

### Requirement: Service ownership is explicit
The system SHALL treat a data or generic service component as Compose-owned unless it contains a `managed` declaration. A managed declaration SHALL select one registered driver contract, and the system SHALL reject ambiguous or conflicting ownership before connecting to a target.

#### Scenario: Existing Compose-owned service remains supported
- **WHEN** a component names a Compose service and does not contain a managed declaration
- **THEN** the system validates and operates that component using the existing Compose-owned behavior without generating another service definition

#### Scenario: Managed service selects a registered contract
- **WHEN** a component contains a managed declaration naming a supported, versioned driver contract
- **THEN** the system resolves that driver as the exclusive owner of the generated service definition

#### Scenario: Managed and authored definitions collide
- **WHEN** a managed component would generate a service, container, volume, network alias, or Compose project identity already claimed by user-authored configuration
- **THEN** validation fails before any target connection or mutation and identifies both owners

#### Scenario: Driver is unavailable
- **WHEN** a managed component names a driver contract the current runner does not support
- **THEN** validation fails with the supported contract identifiers and upgrade or fallback guidance

### Requirement: Managed settings contracts are versioned and bounded
Each managed declaration SHALL identify a versioned driver contract, a versioned profile, and an explicit image reference. Driver contracts SHALL define typed settings, bounded native parameters, protected invariants, change classifications, and validation rules. The system SHALL reject `latest`, an unqualified image name, unknown settings, invalid native parameters, and attempts to override protected invariants.

#### Scenario: Explicit versioned declaration
- **WHEN** a managed component declares a supported driver contract, profile, image tag or digest, and valid settings
- **THEN** the system accepts the declaration and records those inputs in the resolved configuration

#### Scenario: Floating image is rejected
- **WHEN** a managed component omits its image version or uses an unqualified or `latest` image reference
- **THEN** validation fails before target access and requires an explicit versioned reference

#### Scenario: Protected invariant is overridden
- **WHEN** a native parameter or typed setting attempts to replace a driver-owned data path, identity, secret mount, health contract, or control label
- **THEN** validation fails and names the protected setting

#### Scenario: Full native control is required
- **WHEN** an operator needs a Compose or service configuration that the managed contract cannot represent safely
- **THEN** the project can keep that component Compose-owned instead of bypassing managed invariants

### Requirement: Default resolution is deterministic and visible
The system SHALL resolve managed settings using the precedence `driver invariant`, `explicit user value`, `versioned profile default`, then `upstream image default`. Driver invariants SHALL not be configurable. A profile identifier SHALL have immutable semantics. The resolved configuration and every managed-service plan SHALL show each effective Onebox-controlled value with its origin and change classification. Values delegated to the pinned upstream image SHALL be identified as upstream defaults rather than copied or guessed.

#### Scenario: User overrides a profile default
- **WHEN** a valid explicit setting differs from a profile default
- **THEN** the explicit value is effective and is reported with origin `user`

#### Scenario: Driver protects an invariant
- **WHEN** a driver supplies a required non-configurable runtime value
- **THEN** the value is effective and is reported with origin `invariant` and cannot be replaced by user, profile, or native input

#### Scenario: Profile supplies a value
- **WHEN** the user omits a configurable setting supplied by the selected profile
- **THEN** the profile value is effective and is reported with origin `profile` and the profile identifier

#### Scenario: Upstream owns an unset value
- **WHEN** neither the user nor the profile sets a service-native parameter
- **THEN** the desired configuration reports that parameter as delegated to the pinned upstream image and observation reports the actual runtime value when the driver can read it safely

#### Scenario: New defaults are introduced
- **WHEN** Onebox needs to change a profile default or its meaning
- **THEN** it publishes a new profile identifier and does not change the semantics of an existing profile identifier

### Requirement: Secret access is explicitly projected
A managed driver SHALL declare logical secret slots, and project configuration SHALL map each required slot to a named encrypted secret key. Execution SHALL materialize only the selected values into component-scoped mode-protected files or environment entries. Plaintext secret values SHALL NOT appear in resolved configuration, plans, diffs, journals, events, commands, hashes exposed to clients, or observations.

#### Scenario: Managed service receives one secret
- **WHEN** a component maps a driver password slot to one key in the configured encrypted secret source
- **THEN** execution exposes only that key's value to the managed service using the driver's declared delivery mechanism

#### Scenario: Required secret mapping is missing
- **WHEN** a driver-required secret slot has no mapping or the mapped encrypted key is absent
- **THEN** planning or preflight fails before service mutation without revealing other secret names or values

#### Scenario: Secret source changes after planning
- **WHEN** the encrypted source revision changes after a plan is sealed
- **THEN** execution rejects the stale plan without comparing or exposing plaintext secret material

### Requirement: Managed runtime identities are stable and isolated
The system SHALL derive application-scoped Compose project, network, service alias, configuration directory, and volume identities deterministically from validated names. Managed configuration SHALL live outside application release directories, and persistent volumes SHALL remain independent of application release retention and rollback.

#### Scenario: Application release is rolled back
- **WHEN** an application rollback activates an older application release
- **THEN** managed service configuration, image, container, and persistent volumes are not downgraded or replaced

#### Scenario: Application connects to managed service
- **WHEN** an application role and a managed service are healthy
- **THEN** the application reaches the service through a deterministic alias on the application-scoped managed-services network without publishing the service port on the host by default

#### Scenario: Service definition is removed
- **WHEN** configuration removes or disables a managed component that still has applied state or persistent resources on the target
- **THEN** observation reports an orphaned managed service and mutation requires an explicit detach or destroy plan rather than deleting or ignoring it
