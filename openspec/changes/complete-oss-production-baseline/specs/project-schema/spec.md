## MODIFIED Requirements

### Requirement: A declared service is run by Onebox

A service declaration SHALL cause Onebox to run that service: its image, its
durable volume, its health check, its credential, and the connection details
the application reads. Declaring `postgres: 17` SHALL be sufficient; the author
SHALL NOT have to supply a password, a port, or a data path.

The set of drivers Onebox can run SHALL be closed. A declaration naming a
driver outside it SHALL be rejected, naming the available drivers and directing
the author to a daemon workload. A setting SHALL be applied through the
mechanism its driver actually reads; an unsupported setting SHALL be rejected.

The contract SHALL accept a backup policy only when it selects a declared
target and the service driver has an executable protection contract. A backup
declaration SHALL NOT itself establish protection or the `Managed` tier.
Durable services without current backup and restore-drill evidence SHALL remain
valid but SHALL be reported as unprotected `Run` services. Durable workload
volumes remain outside this change's protection contract and SHALL continue to
be reported individually as unbacked rather than disappearing from diagnostics.

#### Scenario: Executable backup declaration
- **WHEN** a qualified service declares a valid target, schedule, retention, and restore-drill policy
- **THEN** the project validates and canonical output records each effective value and origin

#### Scenario: A backup declaration is refused
- **WHEN** a service without a qualified protection contract declares a backup policy
- **THEN** validation fails because Onebox could not perform the declared protection

#### Scenario: Unprotected durable data is reported
- **WHEN** diagnostics run against a durable service without current protection evidence or a workload whose persistence mode is durable
- **THEN** each service is reported as unprotected and `Run`, each durable workload is reported as unbacked, and output names the applicable inspection or protection command without implying workload-volume backup

#### Scenario: A scalar service declaration is sufficient
- **WHEN** a project declares `services: {postgres: 17}`
- **THEN** the generated service runtime runs PostgreSQL 17 with a durable volume, health check, and generated credential while protection remains undeclared

#### Scenario: An unknown driver is refused with alternatives
- **WHEN** a service names a driver Onebox cannot run
- **THEN** validation fails, lists the drivers Onebox can run, and directs the author to a daemon workload

#### Scenario: An inapplicable setting is refused
- **WHEN** a service declares a setting its driver has no mechanism to read
- **THEN** validation fails rather than silently ignoring it

## ADDED Requirements

### Requirement: Backup targets are closed and secret-referential

A backup target SHALL declare a supported off-host storage or replication kind,
a non-secret destination, a failure-domain identity, an encryption policy, and
a trusted credential-file reference. Destination identifiers, schedules,
durations, retention counts, and target names SHALL use closed grammars.
Plaintext storage credentials SHALL NOT be valid project values. A target SHALL
NOT be accepted for a service when it resolves to that service's host, volume,
repository prefix, or storage deployment.

#### Scenario: S3-compatible target
- **WHEN** a target declares an S3-compatible repository and a supported encrypted credential entry
- **THEN** it validates without decrypting credentials and canonical output contains only the credential reference

#### Scenario: Inline storage secret
- **WHEN** a target declares an access key or secret value inline
- **THEN** validation fails with a typed error directing the author to a trusted secret entry

#### Scenario: Independent MinIO replica target
- **WHEN** a MinIO replication target declares an operator-provisioned second deployment outside the protected host, a distinct deployment identity, versioning requirement, TLS endpoint, and trusted administrative credential reference
- **THEN** it validates without provisioning the remote deployment or exposing its credentials

#### Scenario: Target aliases the protected service
- **WHEN** a target's declared or observed failure domain aliases the protected service or its host
- **THEN** planning fails with `backup_target_not_independent` before backup or replication configuration changes

### Requirement: Recovery intent is separate from backup tooling

A service protection policy SHALL declare a closed recovery kind, maximum data
loss, exact restore-drill schedule, restore-proof age, minimum recoverable
generations and recovery window, optional restore-staging filesystem, and
whether recurring backup interruption is permitted. It SHALL NOT select a
backup executable, helper image, command, or
repository layout. Onebox SHALL derive those implementation details from the
driver, service version, objective, and target, and canonical output SHALL show
the resulting recovery envelope and its origin.

#### Scenario: Point-in-time objective validates
- **WHEN** a qualified driver and version declare point-in-time recovery with a supported maximum data-loss interval
- **THEN** canonical output identifies the objective as authored and the selected driver contract as derived

#### Scenario: Tool selection is authored
- **WHEN** a project attempts to name a backup executable or helper image
- **THEN** validation fails because helper selection belongs to the versioned driver contract

#### Scenario: Cold protection requires explicit interruption
- **WHEN** a service can protect all durable state only through a stopped-service backup and the policy omits interruption permission
- **THEN** validation fails with `backup_interruption_not_authorized`

#### Scenario: Policy attempts to authorize an enablement restart
- **WHEN** a project tries to treat recurring interruption permission as authorization for a restart-bound protection prerequisite
- **THEN** validation fails because the restart requires a separate state-bound plan and strong approval

### Requirement: Protection defaults and overrides preserve intent

Backup schedule, native-mappable retention intent, target, recovery objective,
permitted recurring interruption, restore staging, and restore-drill schedule
and maximum age SHALL expose their effective origins. Environment
overrides MAY tune schedule and retention but SHALL NOT silently change the
protected service, driver, recovery kind, interruption permission, repository
identity, or persistence ownership.

#### Scenario: Environment changes schedule
- **WHEN** an environment override supplies a backup schedule
- **THEN** canonical output reports the effective schedule as an override while retaining the same service and target identity

#### Scenario: Environment attempts to replace target identity
- **WHEN** an environment override points protection at a different undeclared repository
- **THEN** validation fails and directs the author to declare an explicit target

### Requirement: External services are distinct from run services

The schema SHALL accept external-service declarations carrying a closed driver
shape, trusted connection source, protection owner, and optional read-only
probe. A name SHALL NOT be declared as both a Onebox-run service and an
external service, and external services SHALL NOT accept Onebox lifecycle or
backup policy fields.

#### Scenario: External connection validates
- **WHEN** an external service declares a supported connection shape and trusted secret source
- **THEN** the project validates and the service is canonically identified as `External`

#### Scenario: Ambiguous ownership
- **WHEN** the same name is declared as both run and external
- **THEN** validation fails before target contact and names the ownership conflict
