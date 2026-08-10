## Purpose

Models application dependencies operated outside Onebox without confusing typed connectivity and risk evidence with ownership of the external service lifecycle.

## ADDED Requirements

### Requirement: External services declare connection shape and ownership

An external service SHALL declare a driver-shaped connection, a trusted secret
source, a persistence/protection owner, and optional read-only health probe.
Onebox SHALL project only the requested connection parts into workloads and
SHALL report the service tier as `External`, never `Run` or `Managed`.

#### Scenario: External PostgreSQL connection
- **GIVEN** a project declares an external PostgreSQL dependency and maps connection parts
- **WHEN** runtime inputs are generated
- **THEN** only those parts are projected from the trusted secret source and no PostgreSQL container or volume is generated

#### Scenario: Lifecycle command targets external service
- **GIVEN** an external dependency
- **WHEN** an operator requests service apply, backup, restore, upgrade, credential rotation, or destroy for it
- **THEN** Onebox refuses with code `external_service_not_owned` and identifies the declared owner

### Requirement: External credentials remain secret and authoritative

External credentials SHALL originate only from supported trusted secret flows
and SHALL NOT be accepted as ordinary model-visible command arguments. Plans,
canonical output, generated runtime, journals, notifications, status, and
public errors SHALL contain references and redacted connection structure, not
plaintext values.

#### Scenario: Structured connection status
- **GIVEN** an external connection URL containing credentials
- **WHEN** status is requested as JSON
- **THEN** output identifies the service, driver shape, owner, and reachability without userinfo, query secrets, or plaintext values

### Requirement: External health observation never converges

Onebox MAY perform a declared read-only driver health probe and SHALL bind its
result and age into plans that depend on the service. It SHALL NOT create
accounts, databases, schemas, buckets, indexes, or provider resources while
observing or planning.

#### Scenario: External state changes after planning
- **GIVEN** a deployment plan bound to an external health observation
- **WHEN** the observation-relevant state changes before deployment
- **THEN** execution fails with code `external_service_state_stale` and directs the operator to re-plan

#### Scenario: Probe lacks permission
- **GIVEN** the trusted credential cannot run the declared health probe
- **WHEN** preflight runs
- **THEN** it reports a typed read failure and performs no corrective mutation

### Requirement: External protection reporting is explicit

Where policy requires a migration backup report, an external service SHALL name
its protection owner and required report identity. Onebox SHALL validate a
fresh plan-bound report or an explicit audited override bound to the plan's
required local-confirmation class, but SHALL NOT
create, store, restore, or claim the external provider's backup.

#### Scenario: Fresh external report
- **GIVEN** a migration affects an external durable service
- **WHEN** a matching fresh backup report is supplied
- **THEN** the plan may proceed while status continues to identify protection as externally owned
