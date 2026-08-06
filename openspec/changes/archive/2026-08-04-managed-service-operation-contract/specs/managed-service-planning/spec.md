## Purpose

Defines immutable planning, approval, drift detection, and application-deployment boundaries for every mutation of a Onebox-managed service.

## ADDED Requirements

### Requirement: Every managed-service mutation requires an executable plan
The system SHALL represent managed-service creation, configuration change, restart, recreation, detach, and future upgrade, backup, or restore operations as expiring digest-sealed plans. Execution SHALL reject a managed-service mutation without the exact supported plan schema and, when policy requires it, an approval bound to that plan.

#### Scenario: Apply is requested without a plan
- **WHEN** an operator requests managed-service apply without an executable managed-service plan
- **THEN** execution refuses before target mutation

#### Scenario: Approved plan is unchanged
- **WHEN** a valid unexpired plan and its matching required approval reach execution with all bindings intact
- **THEN** execution may proceed to locked revalidation and convergence

#### Scenario: Plan content is edited
- **WHEN** any plan field is modified after sealing
- **THEN** digest verification fails before target mutation

### Requirement: Plans bind desired and observed state
A managed-service plan SHALL bind the application, environment, target, component, driver contract, profile, resolved settings digest, encrypted secret-source revision, pinned image digest, managed network identity, persistent-volume identities, applied configuration digest, observed container image, observed service and data-format version, observation completeness, and expiry. An unresolved required binding SHALL block a mutating plan.

#### Scenario: Image tag resolves successfully
- **WHEN** planning resolves the declared image reference to a valid registry manifest digest
- **THEN** the plan and rendered service definition use the immutable digest and retain the human-authored reference for display

#### Scenario: Image cannot be pinned
- **WHEN** the registry cannot provide a valid immutable digest for a managed-service image
- **THEN** planning fails instead of creating a tag-bound mutating plan

#### Scenario: Observation is incomplete
- **WHEN** planning cannot positively identify the applied digest, running image, required volume, or service version needed to classify the change
- **THEN** no ordinary mutating plan is produced and the output identifies the missing evidence

#### Scenario: Bound state drifts before execution
- **WHEN** any bound desired or observed input differs during locked execution revalidation
- **THEN** execution rejects the plan and requires fresh observation and planning

### Requirement: Plans expose resolved behavior and change impact
A managed-service plan SHALL include a redaction-safe desired-versus-observed diff, effective Onebox-controlled settings with origins, delegated upstream defaults, driver change classification, expected container action, persistent-resource effects, verification contract, risk, reversibility, approval class, interruption expectation, and explicit refusal conditions.

#### Scenario: No effective change exists
- **WHEN** desired state, applied state, running image, volumes, and driver observation already match
- **THEN** the plan is identified as a no-op and execution does not recreate or restart the service

#### Scenario: Restart setting changes
- **WHEN** the driver classifies a setting change as requiring restart but not recreation
- **THEN** the plan states the restart and expected service interruption without claiming persistent data changes

#### Scenario: Destructive or upgrade-class change is detected
- **WHEN** a setting, image, volume, service version, or data-format transition is classified as destructive or upgrade-only
- **THEN** ordinary apply planning refuses the transition and directs the operator to the dedicated operation contract

### Requirement: Risk and approval cannot be bypassed by force
The system SHALL derive risk and reversibility from driver classification and persistent-resource effects. A force flag SHALL NOT convert an unsupported, destructive, upgrade-only, stale, unpinned, or incompletely observed change into an ordinary apply. Break-glass behavior SHALL require an explicit plan kind, reason, and approval class.

#### Scenario: Volume removal is requested
- **WHEN** desired state detaches or removes a persistent volume used by the applied service
- **THEN** ordinary apply refuses and force does not bypass the refusal

#### Scenario: Operator requests break glass
- **WHEN** a future supported break-glass operation is planned
- **THEN** the plan records the exact exceptional effect, reason requirement, and stronger approval instead of weakening ordinary apply

### Requirement: Application deployments treat managed services as prerequisites
Application planning and execution SHALL observe each required managed service and SHALL block when it is absent, unhealthy, incompatibly drifted, or incompletely observed. Application deploy, resume, abort, and rollback SHALL NOT implicitly create, restart, recreate, upgrade, downgrade, detach, or delete a managed service.

#### Scenario: Managed database is ready
- **WHEN** all required managed services are healthy, compatible, and sufficiently observed
- **THEN** application planning records their state bindings and may proceed without mutating them

#### Scenario: Managed service needs convergence
- **WHEN** a required managed service is absent or has blocking desired drift
- **THEN** application planning stops with a component-specific next action that an agent can pass to the managed-service proposal tool

#### Scenario: Application rollback runs
- **WHEN** an application release is rolled back
- **THEN** the managed service remains at its currently applied service version and configuration
